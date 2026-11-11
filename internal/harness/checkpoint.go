package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/lannokaede/LanAgent/internal/llm"
)

var ErrUncertainTool = errors.New("tool may have executed; inspect its effects and resolve before resuming")

// Checkpoint is a durable graph boundary, including partially completed batches.
// Running is written before a tool starts and cleared only with its observation.
// A crash between these writes requires reconciliation, never blind replay.
type Checkpoint struct {
	Version         int               `json:"version"`
	Profile         string            `json:"profile"`
	RunID           string            `json:"run_id"`
	Phase           string            `json:"phase"`
	Messages        []llm.Message     `json:"messages"`
	Pending         []llm.ToolCall    `json:"pending,omitempty"`
	Round           int               `json:"round"`
	Calls           int               `json:"calls"`
	Running         string            `json:"running,omitempty"`
	Usage           llm.Usage         `json:"usage"`
	Final           *llm.ChatResponse `json:"final,omitempty"`
	Route           string            `json:"route,omitempty"`
	RouteSource     string            `json:"route_source,omitempty"`
	UsageIncomplete bool              `json:"usage_incomplete,omitempty"`
}

type CheckpointSink func(Checkpoint) error

func (s *runState) checkpoint(phase string) error {
	if s.save == nil {
		return nil
	}
	return s.save(Checkpoint{Version: 1, Profile: s.profile, RunID: s.runID, Phase: phase, Messages: cloneMessages(s.messages), Pending: append([]llm.ToolCall(nil), s.pending...), Round: s.round, Calls: s.toolCalls, Running: s.running, Usage: s.usage, Final: s.final, Route: s.route, RouteSource: s.routeSource, UsageIncomplete: s.usageIncomplete})
}

func (s *runState) executionID(call llm.ToolCall) string {
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%s", s.runID, s.round, call.ID)))
	return hex.EncodeToString(hash[:])
}

// Resume continues exactly at a saved graph boundary. The caller exclusively
// owns the checkpoint and must persist every sink call before returning.
func (r *Runner) Resume(ctx context.Context, cp Checkpoint, save CheckpointSink, emit func(Event) error) ([]llm.Message, *llm.ChatResponse, error) {
	if err := cp.validate(); err != nil {
		return nil, nil, err
	}
	if cp.Profile != "" && cp.Profile != r.ProfileID() {
		return nil, nil, errors.New("checkpoint tool/permission/exercise profile differs; restore original flags before resuming")
	}
	if cp.Running != "" {
		return cloneMessages(cp.Messages), nil, fmt.Errorf("%w: %s", ErrUncertainTool, cp.Running)
	}
	s := &runState{messages: cloneMessages(cp.Messages), pending: append([]llm.ToolCall(nil), cp.Pending...), round: cp.Round, toolCalls: cp.Calls, usage: cp.Usage, final: cp.Final, runID: cp.RunID, profile: cp.Profile, save: save, emit: emit, route: cp.Route, routeSource: cp.RouteSource, usageIncomplete: cp.UsageIncomplete}
	return r.runGraph(ctx, s, cp.Phase)
}

// validate rejects inconsistent durable state before the graph can call a model
// or execute a tool. Legacy v1 checkpoints may omit route and profile metadata.
func (cp Checkpoint) validate() error {
	invalid := func(reason string) error { return fmt.Errorf("invalid checkpoint: %s", reason) }
	if cp.Version != 1 || cp.RunID == "" || cp.Round < 0 || cp.Calls < 0 || len(cp.Messages) == 0 {
		return invalid("version, identity, counters or messages")
	}
	if err := llm.ValidateMessages(cp.Messages); err != nil {
		return invalid(err.Error())
	}
	if cp.Route != "" && cp.Route != routeAgent && cp.Route != routeDirect {
		return invalid("route")
	}
	if cp.Route == routeDirect && (cp.Phase == "tools" || len(cp.Pending) != 0 || cp.Running != "" || cp.Calls != 0 || cp.Round > 1 || (cp.Final != nil && len(cp.Final.ToolCalls) != 0)) {
		return invalid("direct reply contains tool execution or multiple model rounds")
	}
	remaining, trailingBatch, err := checkpointCalls(cp.Messages)
	if err != nil {
		return invalid(err.Error())
	}
	switch cp.Phase {
	case "model":
		if len(cp.Pending) != 0 || cp.Running != "" || cp.Final != nil || len(remaining) != 0 {
			return invalid("model phase has unfinished tools or a final response")
		}
	case "tools":
		if cp.Round == 0 || cp.Final != nil || !trailingBatch || len(cp.Pending) != len(remaining) {
			return invalid("tool phase does not match the assistant decision")
		}
		for i, call := range remaining {
			if cp.Pending[i] != call {
				return invalid("pending tools differ from the unobserved calls")
			}
		}
		if cp.Running != "" {
			state := runState{runID: cp.RunID, round: cp.Round}
			if len(cp.Pending) == 0 || cp.Running != state.executionID(cp.Pending[0]) {
				return invalid("running execution does not match the next pending tool")
			}
		}
	case "done":
		if cp.Final == nil || cp.Final.Content == "" || len(cp.Final.ToolCalls) != 0 {
			return invalid("done phase has no final answer")
		}
		if len(cp.Pending) != 0 || cp.Running != "" || len(remaining) != 0 {
			return invalid("done phase has unfinished tools")
		}
		last := cp.Messages[len(cp.Messages)-1]
		if last.Role != llm.RoleAssistant || len(last.ToolCalls) != 0 || last.Content != cp.Final.Content {
			return invalid("final answer differs from the last assistant message")
		}
	default:
		return invalid("phase")
	}
	return nil
}

// checkpointCalls checks tool-message pairing across history and returns the
// remaining calls in their original order. Completed history may observe a batch
// in any order; the remaining batch is used to validate pending execution.
func checkpointCalls(messages []llm.Message) ([]llm.ToolCall, bool, error) {
	var calls []llm.ToolCall
	unobserved := map[string]bool{}
	trailingBatch := false
	for _, message := range messages {
		if message.Role == llm.RoleTool {
			if !unobserved[message.ToolCallID] {
				return nil, false, errors.New("tool observation has no matching unresolved call")
			}
			delete(unobserved, message.ToolCallID)
			continue
		}
		if len(unobserved) != 0 {
			return nil, false, errors.New("message interrupts an unfinished tool batch")
		}
		calls = message.ToolCalls
		trailingBatch = len(calls) != 0
		for _, call := range calls {
			if unobserved[call.ID] {
				return nil, false, errors.New("duplicate tool call identity")
			}
			unobserved[call.ID] = true
		}
	}
	var remaining []llm.ToolCall
	for _, call := range calls {
		if unobserved[call.ID] {
			remaining = append(remaining, call)
		}
	}
	return remaining, trailingBatch, nil
}

// ProfileID binds recovery to the registered capabilities and pinned exercise.
// Routing does not change these capabilities; its selected mode is checkpointed
// separately so enabling a new-turn optimization keeps existing profiles valid.
func (r *Runner) ProfileID() string {
	if r == nil || r.registry == nil {
		return ""
	}
	pinned := ""
	if r.pinned != nil {
		pinned = r.pinned()
	}
	specs := r.registry.ListSpecs()
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	data, _ := json.Marshal(struct {
		Tools  []llm.ToolDef
		Pinned string
		Policy ToolPolicy
	}{specs, pinned, r.tools})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

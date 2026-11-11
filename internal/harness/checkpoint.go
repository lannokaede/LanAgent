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
	if cp.Version != 1 || cp.RunID == "" || (cp.Phase != "model" && cp.Phase != "tools" && cp.Phase != "done") || cp.Round < 0 || cp.Calls < 0 {
		return nil, nil, errors.New("invalid checkpoint")
	}
	if cp.Profile != "" && cp.Profile != r.ProfileID() {
		return nil, nil, errors.New("checkpoint tool/permission/exercise profile differs; restore original flags before resuming")
	}
	if cp.Route != "" && cp.Route != routeAgent && cp.Route != routeDirect {
		return nil, nil, errors.New("invalid checkpoint route")
	}
	if cp.Route == routeDirect && (cp.Phase == "tools" || len(cp.Pending) != 0 || cp.Running != "" || cp.Calls != 0 || cp.Round > 1 || (cp.Final != nil && len(cp.Final.ToolCalls) != 0)) {
		return nil, nil, errors.New("direct reply checkpoint contains tool execution or multiple model rounds")
	}
	if cp.Running != "" {
		return cloneMessages(cp.Messages), nil, fmt.Errorf("%w: %s", ErrUncertainTool, cp.Running)
	}
	s := &runState{messages: cloneMessages(cp.Messages), pending: append([]llm.ToolCall(nil), cp.Pending...), round: cp.Round, toolCalls: cp.Calls, usage: cp.Usage, final: cp.Final, runID: cp.RunID, profile: cp.Profile, save: save, emit: emit, route: cp.Route, routeSource: cp.RouteSource, usageIncomplete: cp.UsageIncomplete}
	return r.runGraph(ctx, s, cp.Phase)
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

package personal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/lannokaede/LanAgent/internal/harness"
	"github.com/lannokaede/LanAgent/internal/llm"
)

type pendingState struct {
	Workspace  string             `json:"workspace"`
	Checkpoint harness.Checkpoint `json:"checkpoint"`
}

func (s *Session) loadPending() error {
	f, e := s.root.Open(s.ID + ".pending.json")
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	defer f.Close()
	data, e := io.ReadAll(io.LimitReader(f, maxSessionBytes+1))
	if e != nil {
		return e
	}
	if len(data) > maxSessionBytes {
		return errors.New("checkpoint too large")
	}
	var state pendingState
	if e = json.Unmarshal(data, &state); e != nil {
		return e
	}
	if state.Workspace != s.state.Workspace {
		return errors.New("checkpoint workspace mismatch")
	}
	cp := state.Checkpoint
	if cp.Version != 1 || cp.RunID == "" {
		return errors.New("invalid checkpoint")
	}
	if cp.RunID == s.state.LastRun {
		return s.root.Remove(s.ID + ".pending.json")
	}
	s.pending = &cp
	return nil
}
func (s *Session) savePending(cp harness.Checkpoint) error {
	data, e := json.Marshal(pendingState{Workspace: s.state.Workspace, Checkpoint: cp})
	if e != nil {
		return e
	}
	if len(data) > maxSessionBytes {
		return errors.New("checkpoint exceeds size limit")
	}
	if e = s.writeAtomic(s.ID+".pending.json", data); e != nil {
		return e
	}
	s.pending = &cp
	return nil
}
func (s *Session) HasPending() bool { return s.pending != nil }
func (s *Session) Pending() *harness.Checkpoint {
	if s.pending == nil {
		return nil
	}
	data, _ := json.Marshal(s.pending)
	var cp harness.Checkpoint
	_ = json.Unmarshal(data, &cp)
	return &cp
}

func (s *Session) Resume(ctx context.Context, r *harness.Runner, emit func(harness.Event) error) (*llm.ChatResponse, error) {
	if s.root == nil || s.pending == nil || r == nil {
		return nil, errors.New("no active checkpoint or runner")
	}
	runID := s.pending.RunID
	record := func(event harness.Event) error {
		if err := json.NewEncoder(s.trace).Encode(map[string]any{"run_id": runID, "time": time.Now().UTC(), "event": event}); err != nil {
			return err
		}
		return s.trace.Sync()
	}
	if e := record(harness.Event{Type: "run_started"}); e != nil {
		return nil, e
	}
	updated, response, e := r.Resume(ctx, *s.pending, s.savePending, func(event harness.Event) error {
		if e := record(event); e != nil {
			return e
		}
		if emit != nil {
			return emit(event)
		}
		return nil
	})
	if e != nil {
		return nil, errors.Join(e, record(harness.Event{Type: "run_failed"}))
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	if len(updated) < 2 || updated[0].Role != llm.RoleSystem {
		return nil, errors.New("invalid completed checkpoint")
	}
	next := snapshot{Version: 1, Workspace: s.state.Workspace, Messages: updated[1:], LastRun: runID}
	if e = s.commit(next); e != nil {
		return nil, e
	}
	s.state = next
	if e = s.root.Remove(s.ID + ".pending.json"); e != nil {
		return nil, e
	}
	s.pending = nil
	if e = record(harness.Event{Type: "run_completed", Data: response.Usage}); e != nil {
		return nil, e
	}
	return response, nil
}

// Resolve records an operator-observed outcome. It never executes the tool.
func (s *Session) Resolve(executionID, result string) error {
	if s.pending == nil || s.pending.Running == "" || s.pending.Running != executionID || len(s.pending.Pending) == 0 {
		return errors.New("no matching uncertain execution")
	}
	if result == "" || len(result) > 256<<10 {
		return errors.New("provide an observed result, at most 256 KiB")
	}
	cp := *s.pending
	call := cp.Pending[0]
	cp.Messages = append(append([]llm.Message(nil), cp.Messages...), llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Content: "[Operator reconciled outcome]\n" + result})
	cp.Pending = append([]llm.ToolCall(nil), cp.Pending[1:]...)
	cp.Running = ""
	cp.Phase = "tools"
	return s.savePending(cp)
}
func (s *Session) Discard() error {
	if s.pending == nil {
		return nil
	}
	if s.pending.Running != "" {
		return fmt.Errorf("%w: %s", harness.ErrUncertainTool, s.pending.Running)
	}
	if e := s.root.Remove(s.ID + ".pending.json"); e != nil {
		return e
	}
	s.pending = nil
	return nil
}

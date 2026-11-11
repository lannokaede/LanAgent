package harness

import (
	"context"
	"errors"
	"testing"

	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/tool"
)

func checkpointBatch() Checkpoint {
	calls := []llm.ToolCall{
		{ID: "first", Name: "echo", ArgsJSON: `{"value":1}`},
		{ID: "second", Name: "echo", ArgsJSON: `{"value":2}`},
	}
	return Checkpoint{Version: 1, RunID: "restore", Phase: "tools", Round: 1, Calls: 1,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "perform two steps"},
			{Role: llm.RoleAssistant, ToolCalls: calls},
			{Role: llm.RoleTool, ToolCallID: calls[0].ID, Content: "already completed"},
		}, Pending: append([]llm.ToolCall(nil), calls[1:]...)}
}

func TestResumeRejectsInconsistentCheckpointsBeforeCallingDependencies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Checkpoint)
	}{
		{"empty messages", func(cp *Checkpoint) { cp.Messages = nil }},
		{"unsupported role", func(cp *Checkpoint) { cp.Messages[0].Role = "untrusted-role" }},
		{"missing decision", func(cp *Checkpoint) { cp.Messages = cp.Messages[:1] }},
		{"pending name changed", func(cp *Checkpoint) { cp.Pending[0].Name = "different" }},
		{"pending arguments changed", func(cp *Checkpoint) { cp.Pending[0].ArgsJSON = `{"value":999}` }},
		{"completed tool scheduled again", func(cp *Checkpoint) { cp.Pending = cp.Messages[1].ToolCalls }},
		{"pending tool omitted", func(cp *Checkpoint) { cp.Pending = nil }},
		{"orphan observation", func(cp *Checkpoint) { cp.Messages[2].ToolCallID = "unrelated" }},
		{"duplicate observation", func(cp *Checkpoint) { cp.Messages = append(cp.Messages, cp.Messages[2]) }},
		{"duplicate decision identity", func(cp *Checkpoint) { cp.Messages[1].ToolCalls[1].ID = "first" }},
		{"interrupted batch", func(cp *Checkpoint) {
			cp.Messages = append(cp.Messages, llm.Message{Role: llm.RoleUser, Content: "new turn"})
		}},
		{"model phase with pending tool", func(cp *Checkpoint) { cp.Phase = "model" }},
		{"tool phase with final answer", func(cp *Checkpoint) { cp.Final = &llm.ChatResponse{Content: "answer"} }},
		{"tool phase before model", func(cp *Checkpoint) { cp.Round = 0 }},
		{"running execution mismatch", func(cp *Checkpoint) { cp.Running = "another-execution" }},
		{"done phase without answer", func(cp *Checkpoint) { cp.Phase, cp.Pending = "done", nil }},
		{"unknown phase", func(cp *Checkpoint) { cp.Phase = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := checkpointBatch()
			tc.change(&cp)
			echo := &echoTestTool{}
			registry := tool.NewRegistry()
			if err := registry.Register(echo); err != nil {
				t.Fatal(err)
			}
			models, saves, events := 0, 0, 0
			runner := NewRunner(providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
				models++
				return &llm.ChatResponse{Content: "unexpected"}, nil
			}), registry, 4)
			_, response, err := runner.Resume(context.Background(), cp, func(Checkpoint) error { saves++; return nil }, func(Event) error { events++; return nil })
			if err == nil || response != nil || echo.calls != 0 || models != 0 || saves != 0 || events != 0 {
				t.Fatalf("invalid state reached dependencies: err=%v response=%+v tools=%d models=%d saves=%d events=%d", err, response, echo.calls, models, saves, events)
			}
		})
	}
}

func TestResumePreservesLegacyPartialBatchAndEmptyToolBoundary(t *testing.T) {
	for _, allCompleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial batch", true: "completed batch"}[allCompleted], func(t *testing.T) {
			cp := checkpointBatch() // No route/profile fields in the legacy format.
			if allCompleted {
				cp.Messages = append(cp.Messages, llm.Message{Role: llm.RoleTool, ToolCallID: cp.Pending[0].ID, Content: "second completed"})
				cp.Pending = nil
				cp.Calls++
			}
			echo := &echoTestTool{}
			registry := tool.NewRegistry()
			if err := registry.Register(echo); err != nil {
				t.Fatal(err)
			}
			models := 0
			runner := NewRunner(providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
				models++
				if len(req.Messages) != 4 || req.Messages[2].Content != "already completed" || req.Messages[3].ToolCallID != "second" {
					t.Fatalf("lost paired tool history: %+v", req.Messages)
				}
				return &llm.ChatResponse{Content: "done"}, nil
			}), registry, 4)
			var final Checkpoint
			_, response, err := runner.Resume(context.Background(), cp, func(saved Checkpoint) error { final = saved; return nil }, nil)
			wantTools := 1
			if allCompleted {
				wantTools = 0
			}
			if err != nil || response.Content != "done" || echo.calls != wantTools || models != 1 || final.Phase != "done" || final.Calls != 2 {
				t.Fatalf("wrong recovery: err=%v response=%+v tools=%d models=%d checkpoint=%+v", err, response, echo.calls, models, final)
			}
		})
	}
}

func TestResumeUncertainExecutionRemainsBlocked(t *testing.T) {
	cp := checkpointBatch()
	state := runState{runID: cp.RunID, round: cp.Round}
	cp.Running = state.executionID(cp.Pending[0])
	runner := NewRunner(providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
		t.Fatal("uncertain execution called model")
		return nil, nil
	}), tool.NewRegistry(), 4)
	if _, _, err := runner.Resume(context.Background(), cp, nil, nil); !errors.Is(err, ErrUncertainTool) {
		t.Fatalf("lost uncertain-effect guard: %v", err)
	}
}

func TestResumeKeepsCompletedHistoricalBatchWithUnorderedObservations(t *testing.T) {
	calls := []llm.ToolCall{{ID: "a", Name: "echo", ArgsJSON: "{}"}, {ID: "b", Name: "echo", ArgsJSON: "{}"}}
	cp := Checkpoint{Version: 1, RunID: "followup", Phase: "model", Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "old task"},
		{Role: llm.RoleAssistant, ToolCalls: calls},
		{Role: llm.RoleTool, ToolCallID: "b", Content: "second result"},
		{Role: llm.RoleTool, ToolCallID: "a", Content: "first result"},
		{Role: llm.RoleAssistant, Content: "old answer"},
		{Role: llm.RoleUser, Content: "follow-up"},
	}}
	runner := NewRunner(providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		if len(req.Messages) != 6 || req.Messages[2].ToolCallID != "b" || req.Messages[3].ToolCallID != "a" {
			t.Fatal("historical batch changed during recovery")
		}
		return &llm.ChatResponse{Content: "new answer"}, nil
	}), tool.NewRegistry(), 4)
	if _, _, err := runner.Resume(context.Background(), cp, nil, nil); err != nil {
		t.Fatalf("rejected completed historical protocol: %v", err)
	}
}

func TestResumeCompletedAnswerNeedsNoModelOrTool(t *testing.T) {
	cp := Checkpoint{Version: 1, RunID: "completed", Phase: "done", Round: 1,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "question"}, {Role: llm.RoleAssistant, Content: "answer"}},
		Final:    &llm.ChatResponse{Content: "answer", UsageKnown: true}, Usage: llm.Usage{PromptTokens: 7}}
	runner := NewRunner(providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
		t.Fatal("completed checkpoint called model")
		return nil, nil
	}), tool.NewRegistry(), 4)
	_, response, err := runner.Resume(context.Background(), cp, nil, nil)
	if err != nil || response.Content != "answer" || response.Usage.PromptTokens != 7 {
		t.Fatalf("completed answer was not restored: %+v %v", response, err)
	}
	for _, mutate := range []func(*Checkpoint){
		func(cp *Checkpoint) { cp.Final = nil },
		func(cp *Checkpoint) { cp.Final = &llm.ChatResponse{Content: "different answer"} },
		func(cp *Checkpoint) { cp.Final = &llm.ChatResponse{} },
	} {
		invalid := cp
		mutate(&invalid)
		if _, _, err := runner.Resume(context.Background(), invalid, nil, nil); err == nil {
			t.Fatal("accepted inconsistent completed answer")
		}
	}
}

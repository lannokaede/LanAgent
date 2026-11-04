package personal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lannokaede/LanAgent/internal/harness"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/tool"
)

type sideTool struct {
	calls *int
	run   func() error
}

func (t sideTool) Spec() llm.ToolDef {
	return llm.ToolDef{Name: "effect", Parameters: map[string]any{"type": "object"}}
}
func (t sideTool) Execute(context.Context, string) (string, error) {
	*t.calls++
	if t.run != nil {
		if e := t.run(); e != nil {
			return "", e
		}
	}
	return "written", nil
}

func TestResumeKeepsCompletedToolResultAfterModelFailure(t *testing.T) {
	dir, work := t.TempDir(), t.TempDir()
	s, e := Open(dir, "resume", work)
	if e != nil {
		t.Fatal(e)
	}
	executions, models := 0, 0
	r := tool.NewRegistry()
	r.Register(sideTool{calls: &executions})
	runner := harness.NewRunner(providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		models++
		if models == 1 {
			return &llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "one", Name: "effect", ArgsJSON: "{}"}}}, nil
		}
		return nil, errors.New("model offline")
	}), r, 4)
	if _, e = s.Turn(context.Background(), runner, "write once", nil); e == nil {
		t.Fatal("expected outage")
	}
	if executions != 1 || s.Pending().Running != "" {
		t.Fatal("did not persist completed result")
	}
	s.Close()
	s, e = Open(dir, "resume", work)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	resumed := harness.NewRunner(providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		if req.Messages[len(req.Messages)-1].Content != "written" {
			t.Fatal("lost tool output")
		}
		return &llm.ChatResponse{Content: "done"}, nil
	}), r, 4)
	if _, e = s.Resume(context.Background(), resumed, nil); e != nil {
		t.Fatal(e)
	}
	if executions != 1 || s.HasPending() {
		t.Fatal("replayed tool or retained completed checkpoint")
	}
}

func TestUncertainEffectRequiresOperatorResolutionAndSurvivesProcessLock(t *testing.T) {
	dir, work := t.TempDir(), t.TempDir()
	s, e := Open(dir, "uncertain", work)
	if e != nil {
		t.Fatal(e)
	}
	executions := 0
	r := tool.NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	r.Register(sideTool{calls: &executions, run: func() error { cancel(); return nil }})
	runner := harness.NewRunner(providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
		return &llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "call", Name: "effect", ArgsJSON: "{}"}}}, nil
	}), r, 4)
	if _, e = s.Turn(ctx, runner, "do task", nil); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	pending := s.Pending()
	if pending.Running == "" || executions != 1 {
		t.Fatal("lost uncertain state")
	}
	if e = s.Discard(); !errors.Is(e, harness.ErrUncertainTool) {
		t.Fatal("discard bypassed uncertainty")
	}
	s.Close()
	s, e = Open(dir, "uncertain", work)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.Resume(context.Background(), runner, nil); !errors.Is(e, harness.ErrUncertainTool) {
		t.Fatal("replayed uncertain effect")
	}
	if e = s.Resolve("wrong", "observed write"); e == nil {
		t.Fatal("accepted wrong execution ID")
	}
	if e = s.Resolve(pending.Running, "write confirmed on disk"); e != nil {
		t.Fatal(e)
	}
	done := harness.NewRunner(providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		if req.Messages[len(req.Messages)-1].Role != llm.RoleTool {
			t.Fatal("missing reconciliation")
		}
		return &llm.ChatResponse{Content: "resolved"}, nil
	}), r, 4)
	if _, e = s.Resume(context.Background(), done, nil); e != nil {
		t.Fatal(e)
	}
	if executions != 1 {
		t.Fatal("operator resolution re-executed tool")
	}
	// Advisory lock files remain but are not stale process locks.
	if _, e = os.Stat(filepath.Join(dir, "uncertain.lock")); e != nil {
		t.Fatal(e)
	}
}

func TestTraceFailureAfterToolCompletionDoesNotReplay(t *testing.T) {
	s, e := Open(t.TempDir(), "trace", t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	executions := 0
	r := tool.NewRegistry()
	r.Register(sideTool{calls: &executions})
	runner := harness.NewRunner(providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		if req.Messages[len(req.Messages)-1].Role == llm.RoleTool {
			return &llm.ChatResponse{Content: "done"}, nil
		}
		return &llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "a", Name: "effect", ArgsJSON: "{}"}}}, nil
	}), r, 3)
	_, e = s.Turn(context.Background(), runner, "task", func(event harness.Event) error {
		if event.Type == "tool_result" {
			return errors.New("UI disconnected")
		}
		return nil
	})
	if e == nil || s.Pending().Running != "" {
		t.Fatal("failed to persist result before UI callback")
	}
	if _, e = s.Resume(context.Background(), runner, nil); e != nil {
		t.Fatal(e)
	}
	if executions != 1 {
		t.Fatal("trace error caused duplicate effect")
	}
}

func TestPendingTaskCannotMoveToAnotherWorkspaceOrToolProfile(t *testing.T) {
	dir, work := t.TempDir(), t.TempDir()
	s, e := Open(dir, "bound", work)
	if e != nil {
		t.Fatal(e)
	}
	offline := harness.NewRunner(providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) { return nil, errors.New("offline") }), tool.NewRegistry(), 2)
	if _, e = s.Turn(context.Background(), offline, "question", nil); e == nil {
		t.Fatal("expected failure")
	}
	s.Close()
	if other, e := Open(dir, "bound", t.TempDir()); e == nil {
		other.Close()
		t.Fatal("pending task escaped workspace binding")
	}
	s, e = Open(dir, "bound", work)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	registry := tool.NewRegistry()
	count := 0
	registry.Register(sideTool{calls: &count})
	different := harness.NewRunner(providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
		t.Fatal("model invoked with mismatched profile")
		return nil, nil
	}), registry, 2)
	if _, e = s.Resume(context.Background(), different, nil); e == nil {
		t.Fatal("resumed with different capabilities")
	}
}

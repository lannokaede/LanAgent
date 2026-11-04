package personal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lannokaede/LanAgent/internal/harness"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/tool"
)

type providerFunc func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error)

func (f providerFunc) Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	return f(ctx, req)
}

func TestSessionResumeFailureRollbackAndWorkspaceIsolation(t *testing.T) {
	dir := t.TempDir()
	workspace := t.TempDir()
	s, err := Open(dir, "conversation", workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, s.ID, workspace); err == nil {
		t.Fatal("concurrent session was accepted")
	}
	runner := harness.NewRunner(providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		if len(req.Messages) != 2 {
			t.Fatalf("unexpected initial history: %+v", req.Messages)
		}
		return &llm.ChatResponse{Content: "first answer"}, nil
	}), tool.NewRegistry(), 2)
	if _, err := s.Turn(context.Background(), runner, "first question", nil); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, s.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	broken := harness.NewRunner(providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) { return nil, errors.New("offline") }), tool.NewRegistry(), 2)
	if _, err := s.Turn(context.Background(), broken, "failed question", nil); err == nil {
		t.Fatal("expected failure")
	}
	after, _ := os.ReadFile(filepath.Join(dir, s.ID+".json"))
	if string(after) != string(before) {
		t.Fatal("failed turn changed snapshot")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "conversation", t.TempDir()); err == nil {
		t.Fatal("wrong workspace accepted")
	}
	s, err = Open(dir, "conversation", workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.HasPending() {
		t.Fatal("failed task lost its checkpoint")
	}
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}
	resumed := harness.NewRunner(providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		if len(req.Messages) != 4 || req.Messages[1].Content != "first question" || req.Messages[2].Content != "first answer" || req.Messages[3].Content != "second question" {
			t.Fatalf("corrupt resumed history: %+v", req.Messages)
		}
		return &llm.ChatResponse{Content: "second answer"}, nil
	}), tool.NewRegistry(), 2)
	if _, err := s.Turn(context.Background(), resumed, "second question", nil); err != nil {
		t.Fatal(err)
	}
	trace, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"run_started", "run_failed", "run_completed"} {
		if !strings.Contains(string(trace), name) {
			t.Fatalf("missing %s", name)
		}
	}
}

func TestSessionStopsWhenEventSinkFails(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "failure", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	calls := 0
	runner := harness.NewRunner(providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
		calls++
		return &llm.ChatResponse{Content: "done"}, nil
	}), tool.NewRegistry(), 2)
	stop := errors.New("sink disconnected")
	_, err = s.Turn(context.Background(), runner, "question", func(harness.Event) error { return stop })
	if !errors.Is(err, stop) || calls != 0 {
		t.Fatalf("continued after sink failure: calls=%d err=%v", calls, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "failure.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("committed failed turn")
	}
}

func TestSessionRejectsUnpairedToolHistory(t *testing.T) {
	for _, messages := range [][]llm.Message{
		{{Role: llm.RoleUser, Content: "x"}},
		{{Role: llm.RoleUser, Content: "x"}, {Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "a", Name: "add"}}}, {Role: llm.RoleAssistant, Content: "done"}},
		{{Role: llm.RoleUser, Content: "x"}, {Role: llm.RoleTool, ToolCallID: "a", Content: "result"}, {Role: llm.RoleAssistant, Content: "done"}},
	} {
		if err := validateHistory(messages); err == nil {
			t.Fatalf("accepted malformed history: %+v", messages)
		}
	}
}

func TestBoundUserInputReferencesDistinguishTurnsAndSurviveResume(t *testing.T) {
	s := &Session{ID: "conversation"}
	first, _ := s.BoundInput("相同答案")
	s.state.LastRun = "completed-run"
	second, text := s.BoundInput("相同答案")
	if first == second || text != "相同答案" {
		t.Fatal("input refs collapsed across turns")
	}
	s.pending = &harness.Checkpoint{Messages: []llm.Message{{Role: llm.RoleUser, Content: "相同答案"}, {Role: llm.RoleTool, Content: "tool text"}}}
	resumed, original := s.BoundInput("/resume")
	if resumed != second || original != text {
		t.Fatal("resume lost original host input")
	}
}

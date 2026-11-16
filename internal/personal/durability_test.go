package personal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lannokaede/LanAgent/internal/harness"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/tool"
)

func TestAtomicWriteSyncsReplacementDirectory(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "durable", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	called := false
	realSync := s.syncDir
	s.syncDir = func() error {
		called = true
		body, err := os.ReadFile(filepath.Join(dir, "committed.json"))
		if err != nil || string(body) != "replacement" {
			t.Fatalf("directory sync preceded rename: body=%q error=%v", body, err)
		}
		return realSync()
	}
	if err := s.writeAtomic("committed.json", []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("replacement returned before directory persistence")
	}
}

func TestRunningCheckpointDirectoryFailureStopsBeforeTool(t *testing.T) {
	dir, work := t.TempDir(), t.TempDir()
	s, err := Open(dir, "effect", work)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	syncFailure := errors.New("injected directory sync failure")
	realSync := s.syncDir
	s.syncDir = func() error {
		cp := readPendingForDurability(t, dir, s.ID)
		if cp.Running != "" {
			return syncFailure
		}
		return realSync()
	}
	executions, models := 0, 0
	registry := tool.NewRegistry()
	if err := registry.Register(sideTool{calls: &executions}); err != nil {
		t.Fatal(err)
	}
	runner := harness.NewRunner(providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
		models++
		return &llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "write", Name: "effect", ArgsJSON: "{}"}}}, nil
	}), registry, 3)
	if _, err := s.Turn(context.Background(), runner, "write once", nil); !errors.Is(err, syncFailure) {
		t.Fatalf("directory failure was lost: %v", err)
	}
	if executions != 0 || models != 1 {
		t.Fatalf("effect crossed failed commit: executions=%d models=%d", executions, models)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Rename may already be visible even when its directory commit failed.
	// Reopening must conservatively retain the execution marker, not replay it.
	reopened, err := Open(dir, "effect", work)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.Resume(context.Background(), runner, nil); !errors.Is(err, harness.ErrUncertainTool) {
		t.Fatalf("restart replayed an uncertain boundary: %v", err)
	}
	if executions != 0 || models != 1 {
		t.Fatalf("restart called dependencies: executions=%d models=%d", executions, models)
	}
}

func TestFinalCheckpointDirectoryFailureRecoversWithoutModelReplay(t *testing.T) {
	dir, work := t.TempDir(), t.TempDir()
	s, err := Open(dir, "answer", work)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	syncFailure := errors.New("injected final checkpoint sync failure")
	realSync := s.syncDir
	s.syncDir = func() error {
		if readPendingForDurability(t, dir, s.ID).Phase == "done" {
			return syncFailure
		}
		return realSync()
	}
	models := 0
	runner := harness.NewRunner(providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
		models++
		return &llm.ChatResponse{Content: "completed answer"}, nil
	}), tool.NewRegistry(), 2)
	if _, err := s.Turn(context.Background(), runner, "answer", nil); !errors.Is(err, syncFailure) {
		t.Fatalf("final boundary failure was lost: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, "answer", work)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	response, err := reopened.Resume(context.Background(), runner, nil)
	if err != nil || response == nil || response.Content != "completed answer" {
		t.Fatalf("persisted final answer was not recoverable: response=%+v error=%v", response, err)
	}
	if models != 1 || reopened.HasPending() {
		t.Fatalf("recovery replayed the model or retained pending state: models=%d", models)
	}
}

func readPendingForDurability(t *testing.T, dir, id string) harness.Checkpoint {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, id+".pending.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state pendingState
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	return state.Checkpoint
}

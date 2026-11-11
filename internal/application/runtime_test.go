package application_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lannokaede/LanAgent/internal/application"
	"github.com/lannokaede/LanAgent/internal/personal"
	"go.opentelemetry.io/otel"
)

func runtimeOptions(t *testing.T, baseURL, extraConfig string) application.Options {
	t.Helper()
	cfg, work, state := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("LANAGENT_API_CONFIG", filepath.Join(cfg, "missing.local.json"))
	t.Setenv("LLM_BASE_URL", baseURL)
	t.Setenv("LLM_MODEL", "fixture-model")
	t.Setenv("LLM_API_KEY", "fixture-key-secret")
	t.Setenv("EMBEDDING_BASE_URL", baseURL+"/v1")
	t.Setenv("EMBEDDING_MODEL", "text-embedding-v4")
	t.Setenv("EMBEDDING_API_KEY", "fixture-embedding-key")
	t.Setenv("MILVUS_URL", baseURL)
	t.Setenv("MILVUS_TOKEN", "")
	for _, name := range []string{"ROUTER_BASE_URL", "ROUTER_MODEL", "ROUTER_API_KEY"} {
		t.Setenv(name, "")
	}
	body := "llm:\n  timeout: 5s\n  retry:\n    max_attempts: 1\nembedding:\n  dimensions: 2\n" + extraConfig
	if err := os.WriteFile(filepath.Join(cfg, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return application.Options{
		ConfigDir: cfg, Workspace: work, StateDir: state, SessionID: "lifecycle",
		Rounds: 3, Timeout: 5 * time.Second, WebPolicy: "off",
		MemoryDB: filepath.Join(state, "memory.db"),
	}
}

func streamAnswer(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "text/event-stream")
	chunk, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]string{"content": content}, "finish_reason": "stop",
		}},
		"usage": map[string]int{"prompt_tokens": 8, "completion_tokens": 2},
	})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
}

func TestOpenFailureReleasesAcquiredSession(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected model request", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	trace := filepath.Join(t.TempDir(), "trace.jsonl")
	o := runtimeOptions(t, server.URL, fmt.Sprintf("telemetry:\n  enabled: true\n  file: %q\n", trace))
	o.Memory = true
	o.Exercise = "invalid-exercise"
	previous := otel.GetTracerProvider()
	r, err := application.Open(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "exercise must be id/version") || r != nil {
		if r != nil {
			r.Close()
		}
		t.Fatalf("Open returned runtime=%v, error=%v", r, err)
	}
	// The journal is created when the session lock has already been acquired.
	if _, err := os.Stat(filepath.Join(o.StateDir, o.SessionID+".jsonl")); err != nil {
		t.Fatalf("failure occurred before acquiring the session: %v", err)
	}
	if otel.GetTracerProvider() != previous {
		t.Fatal("failed Open left its telemetry provider installed")
	}
	o.Exercise = ""
	r, err = application.Open(context.Background(), o)
	if err != nil {
		t.Fatalf("failed Open leaked the session lock: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("initialization unexpectedly called a model")
	}
}

func TestRepeatedOpenTurnClosePreservesConversation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct{ Role, Content string }
			Tools    []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		turn := int(calls.Add(1))
		var users, assistants int
		for _, message := range request.Messages {
			switch message.Role {
			case "user":
				users++
			case "assistant":
				assistants++
			}
		}
		if users != turn || assistants != turn-1 {
			t.Errorf("turn %d received %d users and %d assistants", turn, users, assistants)
		}
		if len(request.Tools) != 0 {
			t.Error("explicit direct reply exposed tools")
		}
		streamAnswer(w, fmt.Sprintf("answer-%d", turn))
	}))
	t.Cleanup(server.Close)
	o := runtimeOptions(t, server.URL, "")
	for turn := 1; turn <= 3; turn++ {
		r, err := application.Open(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close() })
		if info := r.Info(); info.SessionID != o.SessionID || info.Workspace != o.Workspace {
			t.Fatalf("unexpected conversation identity: %+v", info)
		}
		response, err := r.Turn(context.Background(), fmt.Sprintf("不使用工具：解释第 %d 个例子", turn), nil)
		if err != nil {
			t.Fatal(err)
		}
		if response.Content != fmt.Sprintf("answer-%d", turn) || r.Pending() != nil {
			t.Fatalf("turn did not commit: response=%+v, pending=%+v", response, r.Pending())
		}
		for close := 0; close < 2; close++ {
			if err := r.Close(); err != nil {
				t.Fatalf("Close %d failed: %v", close+1, err)
			}
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("model requests = %d, want 3", calls.Load())
	}
}

func TestCloseAfterCancellationFlushesTelemetry(t *testing.T) {
	requestStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(requestStarted)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	trace := filepath.Join(t.TempDir(), "trace.jsonl")
	o := runtimeOptions(t, server.URL, fmt.Sprintf("telemetry:\n  enabled: true\n  file: %q\n", trace))
	previous := otel.GetTracerProvider()
	r, err := application.Open(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	turnDone := make(chan error, 1)
	go func() {
		_, err := r.Turn(ctx, "不使用工具：解释 channel prompt-secret", nil)
		turnDone <- err
	}()
	select {
	case <-requestStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("model request did not start")
	}
	cancel()
	select {
	case err := <-turnDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Turn returned %v, want cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Turn did not stop after cancellation")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("canceled task prevented resource shutdown: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("repeated Close changed the result: %v", err)
	}
	if otel.GetTracerProvider() != previous {
		t.Fatal("Close left its telemetry provider installed")
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agent.task", "llm.call", "llm.attempt"} {
		if !bytes.Contains(data, []byte(name)) {
			t.Fatalf("Close did not flush %s after cancellation", name)
		}
	}
	for _, secret := range []string{"fixture-key-secret", "prompt-secret"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("telemetry contains %s", secret)
		}
	}
}

func TestMemoryCommandBypassesConfigurationAndConversationLock(t *testing.T) {
	work, state := t.TempDir(), t.TempDir()
	o := application.Options{
		Workspace: work, StateDir: state, SessionID: "held", Memory: true,
		ConfigDir: filepath.Join(t.TempDir(), "missing-config"),
	}
	held, err := personal.Open(state, o.SessionID, work)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { held.Close() })
	var output bytes.Buffer
	if err := application.MemoryCommand(context.Background(), o, "/remember user career.target GoBackend", &output); err != nil {
		t.Fatalf("local command required configuration or a conversation lock: %v", err)
	}
	output.Reset()
	if err := application.MemoryCommand(context.Background(), o, "/memory user", &output); err != nil {
		t.Fatal(err)
	}
	var records []struct{ Key, Content string }
	if err := json.Unmarshal(output.Bytes(), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Key != "career.target" || records[0].Content != "GoBackend" {
		t.Fatalf("local command did not persist memory: %s", output.String())
	}
}

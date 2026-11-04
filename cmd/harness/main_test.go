package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	assets "github.com/lannokaede/LanAgent/examples/practice"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersonalCLIReadsWorkspaceAndResumesWithoutDatabase(t *testing.T) {
	work := t.TempDir()
	state := t.TempDir()
	cfg := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "note.md"), []byte("workspace evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "config.yaml"), []byte("llm:\n  timeout: 10s\n"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"read-1\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"note.md\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		if calls == 2 {
			last := request.Messages[len(request.Messages)-1]
			if last.Role != "tool" || !strings.Contains(last.Content, "workspace evidence") {
				t.Errorf("no real file observation: %+v", request)
			}
		} else if calls == 3 {
			if len(request.Messages) != 6 {
				t.Errorf("history not restored: %+v", request)
			}
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"observed note.md\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("LLM_API_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_BASE_URL", server.URL+"/v1")
	args := []string{"-workspace", work, "-state-dir", state, "-config", cfg, "-session", "test-session", "-prompt", "read note.md"}
	for i := 0; i < 2; i++ {
		var out, status bytes.Buffer
		if err := run(context.Background(), args, strings.NewReader(""), &out, &status); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "observed note.md") {
			t.Fatalf("missing answer: %s", out.String())
		}
	}
	if calls != 3 {
		t.Fatalf("unexpected model calls %d", calls)
	}
}

// Local model stub + real Docker verifies the new personal entry all the way
// through a version-bound sandbox result. No paid model calls are made.
func TestPersonalExerciseDockerPath(t *testing.T) {
	if os.Getenv("PRACTICE_DOCKER_TEST") != "1" {
		t.Skip("set PRACTICE_DOCKER_TEST=1 with practice image")
	}
	work, state, cfg := t.TempDir(), t.TempDir(), t.TempDir()
	if e := os.WriteFile(filepath.Join(cfg, "config.yaml"), []byte("llm:\n  timeout: 10s\n"), 0600); e != nil {
		t.Fatal(e)
	}
	code, e := assets.Files.ReadFile("context-cancel/v1/correct.go.txt")
	if e != nil {
		t.Fatal(e)
	}
	encoded, _ := json.Marshal(string(code)) // existing practice hash contract
	digest := sha256.Sum256(encoded)
	hash := fmt.Sprintf("%x", digest)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req struct {
			Messages []struct{ Role, Content string }
		}
		if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
			t.Error(e)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		var delta map[string]any
		if calls == 1 {
			args, _ := json.Marshal(map[string]string{"code": string(code)})
			delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "docker-1", "type": "function", "function": map[string]any{"name": "run_go_exercise", "arguments": string(args)}}}}
		} else {
			observation := req.Messages[len(req.Messages)-1]
			if observation.Role != "tool" || !strings.Contains(observation.Content, hash) || !strings.Contains(observation.Content, `"status":"passed"`) {
				t.Errorf("missing real sandbox evidence: %+v", observation)
			}
			delta = map[string]any{"content": "verified " + hash}
		}
		finish := "tool_calls"
		if calls > 1 {
			finish = "stop"
		}
		raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
	}))
	defer server.Close()
	t.Setenv("LLM_API_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_BASE_URL", server.URL+"/v1")
	var out, status bytes.Buffer
	e = run(context.Background(), []string{"-config", cfg, "-workspace", work, "-state-dir", state, "-session", "docker-session", "-exercise", "context-cancel/v1", "-prompt", "verify this code", "-timeout", "90s"}, strings.NewReader(""), &out, &status)
	if e != nil {
		t.Fatal(e)
	}
	if calls != 2 || !strings.Contains(out.String(), hash) {
		t.Fatal("personal sandbox path did not complete")
	}
	persisted, e := os.ReadFile(filepath.Join(state, "docker-session.json"))
	if e != nil || !strings.Contains(string(persisted), hash) {
		t.Fatal("sandbox evidence not committed", e)
	}
}

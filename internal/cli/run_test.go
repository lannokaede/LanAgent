package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestUnifiedConversationSelectsToolsAndPreservesRealAnswer(t *testing.T) {
	var mu sync.Mutex
	steps := map[string]int{}
	reviewCount := 0
	var server *httptest.Server
	answer := "发送与接收建立同步关系，这是我的原始回答。"
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/describe") {
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"description": "demo-rag-v1:" + server.URL + "/v1:text-embedding-v4:2:raw-text", "fields": []any{map[string]any{"name": "vector", "type": "FloatVector", "params": []any{map[string]any{"key": "dim", "value": "2"}}}}}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float32{1, 0}}}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/search") {
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": []any{map[string]any{"id": "evidence", "distance": 0.9, "source": "https://go.dev/ref/mem", "chunk_index": 0, "text": "channel synchronization"}}})
			return
		}
		var req struct {
			Messages []struct{ Role, Content string }
			Tools    []json.RawMessage
		}
		if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
			t.Error(e)
			http.Error(w, "bad", 400)
			return
		}
		delta := map[string]any{}
		finish := "stop"
		emitTool := func(name string, args any) {
			b, _ := json.Marshal(args)
			delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("call-%d", len(req.Messages)), "type": "function", "function": map[string]any{"name": name, "arguments": string(b)}}}}
			finish = "tool_calls"
		}
		if len(req.Tools) == 0 {
			reviewCount++
			if !strings.Contains(req.Messages[len(req.Messages)-1].Content, answer) {
				t.Error("not the original user answer")
			}
			delta["content"] = `{"score":8,"feedback":"已记录真实答案","references":[1]}`
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": delta["content"]}, "finish_reason": "stop"}}})
			return
		} else {
			user := ""
			for _, m := range req.Messages {
				if m.Role == "user" {
					user = m.Content
				}
			}
			n := steps[user]
			steps[user]++
			switch user {
			case "模拟并发面试":
				if n == 0 {
					emitTool("load_skill", map[string]string{"name": "go-interview"})
				} else if n == 1 {
					emitTool("interview_start", map[string]any{"topic": "concurrency", "questions": 3})
				} else if n == 2 {
					emitTool("interview_next", map[string]any{})
				} else {
					delta["content"] = "第一题等待回答"
				}
			case "不懂，先解释":
				if n == 0 {
					emitTool("knowledge_search", map[string]any{"query": "channel synchronization"})
				} else {
					delta["content"] = "解释完毕，保留原题"
				}
			case "继续面试":
				if n == 0 {
					emitTool("interview_next", map[string]any{})
				} else {
					last := req.Messages[len(req.Messages)-1].Content
					if !strings.Contains(last, "channel-sync") {
						t.Error("pending question changed", last)
					}
					delta["content"] = "继续原题"
				}
			case answer:
				if n == 0 {
					emitTool("interview_status", map[string]any{})
				} else if n == 1 {
					var state map[string]any
					if e := json.Unmarshal([]byte(req.Messages[len(req.Messages)-1].Content), &state); e != nil {
						t.Error(e)
					}
					emitTool("interview_submit_answer", map[string]any{"question_id": "channel-sync", "input_ref": state["input_ref"]})
				} else {
					if !strings.Contains(req.Messages[len(req.Messages)-1].Content, `"status":"reviewed"`) {
						t.Error("review was not committed", req.Messages[len(req.Messages)-1].Content)
					}
					delta["content"] = "点评完成"
				}
			default:
				t.Error("unexpected user input", user)
				delta["content"] = "unexpected"
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	}))
	defer server.Close()
	dir, work, state := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("LANAGENT_API_CONFIG", filepath.Join(dir, "missing.local.json"))
	t.Setenv("LLM_BASE_URL", server.URL+"/v1")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_API_KEY", "test-key")
	t.Setenv("EMBEDDING_BASE_URL", server.URL+"/v1")
	t.Setenv("EMBEDDING_API_KEY", "embedding-test")
	t.Setenv("MILVUS_URL", server.URL)
	cfg := fmt.Sprintf("llm:\n  timeout: 10s\nembedding:\n  base_url: %q\n  model: text-embedding-v4\n  dimensions: 2\nrag:\n  enabled: true\n  milvus_url: %q\n  collection: test_collection\n  knowledge_base: learning\n", server.URL+"/v1", server.URL)
	if e := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	input := "模拟并发面试\n不懂，先解释\n继续面试\n" + answer + "\n/exit\n"
	var out, status bytes.Buffer
	if e := Run(context.Background(), []string{"-config", dir, "-workspace", work, "-state-dir", state, "-session", "unified"}, strings.NewReader(input), &out, &status); e != nil {
		t.Fatal(e, status.String())
	}
	if reviewCount != 1 || !strings.Contains(out.String(), "解释完毕") || !strings.Contains(out.String(), "点评完成") {
		t.Fatal(out.String(), reviewCount)
	}
	b, e := os.ReadFile(filepath.Join(state, "unified.interview.json"))
	if e != nil {
		t.Fatal(e)
	}
	var saved struct {
		Trainings map[string]struct {
			Coach struct{ History []struct{ Answer string } }
		}
	}
	if json.Unmarshal(b, &saved) != nil {
		t.Fatal("bad training state")
	}
	for _, v := range saved.Trainings {
		if len(v.Coach.History) != 1 || v.Coach.History[0].Answer != answer {
			t.Fatal("answer record differs", v)
		}
	}
}

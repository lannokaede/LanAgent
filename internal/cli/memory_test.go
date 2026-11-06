package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/lannokaede/LanAgent/internal/rag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersonalMemoryAcrossNewSessionsCorrectionDeletionAndOfflineCommands(t *testing.T) {
	t.Setenv("LANAGENT_API_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	work, otherWork, state, cfg := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg, "config.yaml"), []byte("merchant:\n  enabled: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	base := []string{"-workspace", work, "-state-dir", state, "-config", cfg}
	execute := func(args []string) string {
		t.Helper()
		var out, status bytes.Buffer
		if err := Run(context.Background(), args, strings.NewReader(""), &out, &status); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	// Explicit local edits bypass config loading: no API key, service or model.
	t.Setenv("LLM_API_KEY", "")
	execute(append(append([]string{}, base...), "-config", filepath.Join(cfg, "does-not-exist"), "-prompt", "/remember user career.target GoBackend"))
	execute(append(append([]string{}, base...), "-prompt", "/remember project stack ProjectOne"))
	if text := execute(append(append([]string{}, base...), "-prompt", "/memory user")); !strings.Contains(text, "GoBackend") {
		t.Fatal(text)
	}
	want, project := "GoBackend", true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var facts strings.Builder
		for _, m := range request.Messages {
			if strings.HasPrefix(m.Content, "[Current saved personal memory;") {
				facts.WriteString(m.Content)
			}
		}
		text := facts.String()
		if want != "" && !strings.Contains(text, want) {
			t.Errorf("missing current memory: %s", text)
		}
		if want != "GoBackend" && strings.Contains(text, "GoBackend") {
			t.Errorf("recalled obsolete memory: %s", text)
		}
		if want == "" && strings.Contains(text, "career.target") {
			t.Errorf("recalled deleted memory: %s", text)
		}
		if strings.Contains(text, "ProjectOne") != project {
			t.Errorf("workspace isolation failed: %s", text)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ack\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("LLM_API_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_BASE_URL", server.URL+"/v1")
	execute(append(append([]string{}, base...), "-session", "memory-first", "-prompt", "我的求职目标是什么"))
	execute(append(append([]string{}, base...), "-prompt", "/remember user career.target AgentBackend"))
	want = "AgentBackend"
	execute(append(append([]string{}, base...), "-session", "memory-second", "-prompt", "继续学习"))
	// Recall is transient, preventing stale facts copied into saved run messages.
	data, err := os.ReadFile(filepath.Join(state, "memory-second.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Current saved personal memory") || strings.Contains(string(data), "AgentBackend") {
		t.Fatal("recall material persisted into history")
	}
	project = false
	execute([]string{"-workspace", otherWork, "-state-dir", state, "-config", cfg, "-session", "other-project", "-prompt", "我的目标是什么"})
	execute(append(append([]string{}, base...), "-prompt", "/forget user career.target"))
	want = ""
	project = true
	execute(append(append([]string{}, base...), "-session", "memory-third", "-prompt", "继续"))
}

func TestPersonalCLIMemoryVectorSyncFlagUsesSeparateIndex(t *testing.T) {
	t.Setenv("LANAGENT_API_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	work, state, cfg := t.TempDir(), t.TempDir(), t.TempDir()
	identity := ""
	upserts, deletes, embeddings := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			embeddings++
			fmt.Fprint(w, `{"data":[{"index":0,"embedding":[1,0]}]}`)
			return
		}
		if body["collectionName"] != "lanagent_personal_memory_v1" {
			t.Error("wrong memory collection", body)
		}
		data := any(map[string]any{})
		switch {
		case strings.HasSuffix(r.URL.Path, "/has"):
			data = map[string]any{"has": true}
		case strings.HasSuffix(r.URL.Path, "/describe"):
			data = map[string]any{"description": identity, "fields": []any{map[string]any{"name": "vector", "type": "FloatVector", "params": []any{map[string]any{"key": "dim", "value": "2"}}}}}
		case strings.HasSuffix(r.URL.Path, "/upsert"):
			upserts++
		case strings.HasSuffix(r.URL.Path, "/delete"):
			deletes++
		default:
			t.Error("unexpected service request", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
	}))
	defer server.Close()
	emb, err := rag.NewEmbeddingClient(server.URL+"/v1", "text-embedding-v4", "test-key", 2)
	if err != nil {
		t.Fatal(err)
	}
	identity = "lanagent-personal-memory-v1:" + emb.Identity()
	emb.Close()
	config := fmt.Sprintf("merchant:\n  enabled: false\nllm:\n  base_url: %q\n  model: test-model\nembedding:\n  base_url: %q\n  model: text-embedding-v4\n  dimensions: 2\nrag:\n  enabled: false\n  milvus_url: %q\n", server.URL+"/v1", server.URL+"/v1", server.URL)
	if err = os.WriteFile(filepath.Join(cfg, "config.yaml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLM_API_KEY", "test-key")
	t.Setenv("EMBEDDING_API_KEY", "test-key")
	t.Setenv("LLM_BASE_URL", server.URL+"/v1")
	t.Setenv("LLM_MODEL", "test-model")
	base := []string{"-config", cfg, "-workspace", work, "-state-dir", state}
	for _, extra := range [][]string{{"-prompt", "/remember user career.target Go"}, {"-memory-vectors", "-memory-init", "-prompt", "/memory sync"}} {
		var out, status bytes.Buffer
		if err = Run(context.Background(), append(append([]string{}, base...), extra...), strings.NewReader(""), &out, &status); err != nil {
			t.Fatal(err)
		}
		if extra[0] == "-memory-vectors" && !strings.Contains(out.String(), `"done":1`) {
			t.Fatal("sync did not acknowledge task", out.String())
		}
	}
	if upserts != 1 || deletes != 1 || embeddings != 1 {
		t.Fatal("CLI did not synchronize the persisted fact", upserts, deletes, embeddings)
	}
}

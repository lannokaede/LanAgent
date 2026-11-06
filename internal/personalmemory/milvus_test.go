package personalmemory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lannokaede/LanAgent/internal/rag"
)

func TestSQLiteOutboxMilvusProtocolAndStaleCandidateFiltering(t *testing.T) {
	ctx := context.Background()
	store := openTest(t, filepath.Join(t.TempDir(), "memory.db"))
	rows := map[string]map[string]any{}
	var filters []string
	var index *Milvus
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float32{1, 0}}}})
			return
		}
		if body["collectionName"] != "test_personal_memory" {
			t.Error("wrong collection", body["collectionName"])
		}
		result := any(map[string]any{})
		switch {
		case strings.HasSuffix(r.URL.Path, "/has"):
			result = map[string]any{"has": true}
		case strings.HasSuffix(r.URL.Path, "/describe"):
			result = map[string]any{"description": index.identity, "fields": []any{map[string]any{"name": "vector", "type": "FloatVector", "params": []any{map[string]any{"key": "dim", "value": "2"}}}}}
		case strings.HasSuffix(r.URL.Path, "/upsert"):
			for _, v := range body["data"].([]any) {
				row := v.(map[string]any)
				rows[row["id"].(string)] = row
			}
		case strings.HasSuffix(r.URL.Path, "/delete"):
			// Deliberately retain obsolete rows to emulate delayed cleanup/stale reads.
			filters = append(filters, body["filter"].(string))
		case strings.HasSuffix(r.URL.Path, "/search"):
			if !strings.Contains(body["filter"].(string), store.UserScope()) || body["consistencyLevel"] != "Strong" {
				t.Error("missing scoped strong query", body)
			}
			hits := []any{}
			for id := range rows {
				hits = append(hits, map[string]any{"id": id, "text": "untrusted-vector-content"})
			}
			result = hits
		default:
			t.Error("unexpected route", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": result})
	}))
	defer server.Close()
	emb, err := rag.NewEmbeddingClient(server.URL+"/v1", "text-embedding-v4", "test-key", 2)
	if err != nil {
		t.Fatal(err)
	}
	index, err = NewMilvus(server.URL, "", "test_personal_memory", emb, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if err = index.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.BindIndex(ctx, index.Identity()); err != nil {
		t.Fatal(err)
	}
	svc := New(store, "/one", index)
	old := putTest(t, store, "1", store.UserScope(), "career.target", "old-Java", 0)
	if _, err = svc.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	current := putTest(t, store, "2", store.UserScope(), "career.target", "current-Go", 1)
	if _, err = svc.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[current.VectorID]["chunk_index"] != float64(2) {
		t.Fatal("missing immutable versions", rows)
	}
	if !strings.Contains(filters[1], "chunk_index < 2") {
		t.Fatal("cleanup could remove newer writes", filters)
	}
	text, err := svc.Recall(ctx, "career")
	if err != nil || !strings.Contains(text, "current-Go") || strings.Contains(text, "old-Java") || strings.Contains(text, "untrusted-vector-content") {
		t.Fatal(text, err)
	}
	if _, err = store.Mutate(ctx, "3", store.UserScope(), "career.target", "", "", 2, true); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filters[2], "chunk_index <= 3") {
		t.Fatal(filters)
	}
	// Simulate a very late old worker after the deletion. SQLite still rejects it.
	if err = index.Sync(ctx, old); err != nil {
		t.Fatal(err)
	}
	text, err = svc.Recall(ctx, "career")
	if err != nil || text != "" {
		t.Fatal("late write resurrected memory", text, err)
	}
	status, err := store.Status(ctx)
	if err != nil || status["done"] != 3 {
		t.Fatal(status, err)
	}
}

func TestMilvusFailureDoesNotExposeCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, "sensitive-token upstream body")
	}))
	defer server.Close()
	emb, err := rag.NewEmbeddingClient(server.URL, "text-embedding-v4", "sensitive-token", 2)
	if err != nil {
		t.Fatal(err)
	}
	index, err := NewMilvus(server.URL, "sensitive-token", "test_personal", emb, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	err = index.call(context.Background(), "entities/delete", map[string]any{}, nil)
	if err == nil || strings.Contains(err.Error(), "sensitive-token") {
		t.Fatal(err)
	}
}

package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMilvusIsolationAndErrors(t *testing.T) {
	identity := "test-model"
	var searched bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/describe"):
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"description": identity, "fields": []any{map[string]any{"name": "vector", "type": "FloatVector", "params": []any{map[string]any{"key": "dim", "value": "2"}}}}}})
		case strings.HasSuffix(r.URL.Path, "/search"):
			searched = true
			if !strings.Contains(body["filter"].(string), `kb == "learning"`) {
				t.Error("missing knowledge base filter")
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": []any{map[string]any{"id": "c", "distance": 0.9, "source": "a.md", "text": "结果", "chunk_index": 0}}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"code": 123, "message": "raw service error"})
		}
	}))
	defer server.Close()
	store, err := NewMilvusStore(server.URL, "", "test_collection", 2, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	hits, err := store.Search(context.Background(), "learning", []float32{1, 0}, 3)
	if err != nil || len(hits) != 1 || hits[0].Score != 0.9 || !searched {
		t.Fatalf("bad search: %v %+v", err, hits)
	}
	if _, err := store.Search(context.Background(), `x" || true`, []float32{1, 0}, 3); err == nil {
		t.Fatal("accepted filter injection")
	}
	identity = "different-model"
	if err := store.Check(context.Background()); err == nil {
		t.Fatal("accepted incompatible collection")
	}
}

func TestMilvusImportDoesNotDeleteOldVersionOnPartialFailure(t *testing.T) {
	calls, deletes := 0, 0
	fail := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/upsert") {
			calls++
			if fail && calls == 2 {
				w.Write([]byte(`{"code":123}`))
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/delete") {
			deletes++
		}
		w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer server.Close()
	store, _ := NewMilvusStore(server.URL, "", "test_collection", 2, "test-model")
	preparer, _ := NewPreparer(SplitConfig{4, 0})
	doc, err := preparer.Prepare(context.Background(), "learning", "a.md", strings.NewReader(strings.Repeat("字", 260)))
	if err != nil {
		t.Fatal(err)
	}
	vectors := make([][]float32, len(doc.Chunks))
	for i := range vectors {
		vectors[i] = []float32{1, 0}
	}
	if err := store.Write(context.Background(), doc, vectors); err == nil || deletes != 0 {
		t.Fatal("must preserve old version after partial failure")
	}
	calls = 0
	fail = false
	if err := store.Write(context.Background(), doc, vectors); err != nil || calls != 2 || deletes != 1 {
		t.Fatalf("retry failed: %v", err)
	}
}

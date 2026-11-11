package acceptance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/lannokaede/LanAgent/internal/config"
)

func TestCollectionCleanupOnlyTouchesItsOwnAllocation(t *testing.T) {
	var requested []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ CollectionName string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		requested = append(requested, body.CollectionName)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()
	cfg := &config.Config{}
	cfg.RAG.MilvusURL = server.URL
	owned, another := newCollections(), newCollections()
	for _, name := range []string{"production", "lanagent_personal_memory_v1", "", another.memory, another.rag} {
		if err := owned.drop(context.Background(), cfg, name); err == nil {
			t.Fatal("accepted unowned collection", name)
		}
	}
	if err := owned.cleanup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(requested, []string{owned.memory, owned.rag}) {
		t.Fatalf("cleanup escaped its allocation: %v", requested)
	}
}

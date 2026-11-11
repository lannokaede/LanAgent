package application_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lannokaede/LanAgent/internal/application"
)

func TestCloseJoinsMemoryWorkerBeforeClosingSQLite(t *testing.T) {
	upsertStarted, upsertCanceled := make(chan struct{}), make(chan struct{})
	var started, canceled sync.Once
	var upserts atomic.Int32
	var identity string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			streamAnswer(w, "answer")
			return
		case strings.HasSuffix(r.URL.Path, "/embeddings"):
			fmt.Fprint(w, `{"data":[{"index":0,"embedding":[1,0]}]}`)
			return
		}
		if body["collectionName"] != "lifecycle_memory" {
			t.Errorf("unexpected memory collection: %v", body["collectionName"])
		}
		var data any
		switch {
		case strings.HasSuffix(r.URL.Path, "/describe"):
			data = map[string]any{
				"description": identity,
				"fields": []any{map[string]any{
					"name": "vector", "type": "FloatVector",
					"params": []any{map[string]string{"key": "dim", "value": "2"}},
				}},
			}
		case strings.HasSuffix(r.URL.Path, "/search"):
			data = []any{}
		case strings.HasSuffix(r.URL.Path, "/upsert"):
			upserts.Add(1)
			started.Do(func() { close(upsertStarted) })
			<-r.Context().Done()
			canceled.Do(func() { close(upsertCanceled) })
			return
		default:
			t.Errorf("unexpected index request: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
	}))
	t.Cleanup(server.Close)
	identity = "lanagent-personal-memory-v1:demo-rag-v1:" + server.URL + "/v1:text-embedding-v4:2:raw-text"
	o := runtimeOptions(t, server.URL, "")
	o.Memory, o.MemoryVectors = true, true
	o.MemoryCollection = "lifecycle_memory"
	if err := application.MemoryCommand(context.Background(), o, "/remember user career.target GoBackend", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r, err := application.Open(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); r.Close() })
	// A normal turn starts the worker. The model can finish while its independent
	// outbox synchronization remains blocked at the external side-effect boundary.
	if _, err := r.Turn(ctx, "不使用工具：解释 channel", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-upsertStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("memory worker did not reach the pending upsert")
	}
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not cancel and join the memory worker")
	}
	select {
	case <-upsertCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("Close left an in-flight memory request running")
	}
	// ProcessOne finishes its lease under a fresh context after cancellation. A
	// store closed before joining the worker would leave this job stuck running.
	var output bytes.Buffer
	if err := application.MemoryCommand(context.Background(), o, "/memory status", &output); err != nil {
		t.Fatal(err)
	}
	var status map[string]int
	if err := json.Unmarshal(output.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status["pending"] != 1 || status["running"] != 0 || status["done"] != 0 {
		t.Fatalf("worker did not release its lease before SQLite closed: %v", status)
	}
	if upserts.Load() != 1 {
		t.Fatalf("upsert attempts = %d, want 1", upserts.Load())
	}
}

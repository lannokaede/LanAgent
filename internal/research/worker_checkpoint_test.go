package research

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkerCheckpointFailureStopsAndRemainsRecoverable(t *testing.T) {
	dir := t.TempDir()
	index := &fakeIndex{}
	s := newService(t, dir, index)
	closed := false
	t.Cleanup(func() {
		if !closed {
			s.Close()
		}
	})
	c := candidate(t, s)
	var id string
	// Commit a real queued task while holding its execution gate, then make the
	// checkpoint's temporary pathname unwritable before the worker can start it.
	func() {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		id = commit(t, s, c, hash([]byte("checkpoint failure")))
		if err := os.Mkdir(filepath.Join(dir, "state.json.tmp"), 0700); err != nil {
			t.Fatal(err)
		}
	}()
	stopped := make(chan struct{})
	go func() { s.wg.Wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("worker kept retrying an unwritable checkpoint instead of stopping")
	}
	j, err := s.Job(id)
	if err != nil || j.Status != "queued" || !strings.Contains(j.Error, "worker_checkpoint_failed") {
		t.Fatalf("storage failure was not observable: job=%+v, error=%v", j, err)
	}
	index.mu.Lock()
	calls := index.count
	index.mu.Unlock()
	if calls != 0 {
		t.Fatal("worker called the index before persisting its running checkpoint")
	}
	// Runtime failure metadata must not rewrite a task that never started.
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk state
	if err := json.Unmarshal(raw, &disk); err != nil || disk.Jobs[id].Status != "queued" || disk.Jobs[id].Error != "" {
		t.Fatalf("queued durable task was changed by a failed checkpoint: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "state.json.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	reopened := newService(t, dir, index)
	defer reopened.Close()
	waitJob(t, reopened, id, "succeeded")
	j, err = reopened.Job(id)
	if err != nil || j.Error != "" {
		t.Fatalf("recovery retained a stale worker failure: %+v %v", j, err)
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	if index.count != 1 {
		t.Fatalf("restart indexed %d times, want one", index.count)
	}
}

func TestCanceledSelectionDoesNotStopWorker(t *testing.T) {
	s := newService(t, t.TempDir(), &fakeIndex{})
	defer s.Close()
	c := candidate(t, s)
	var selected Job
	func() {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		id := commit(t, s, c, hash([]byte("cancel selection")))
		var err error
		selected, err = s.Job(id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.CancelJob(id); err != nil {
			t.Fatal(err)
		}
	}()
	// Simulate the worker having selected a queued snapshot just before cancel.
	if !s.runJob(selected) {
		t.Fatal("a canceled selection was treated as a storage failure")
	}
	if _, err := s.SearchKnowledge(context.Background(), "context", 1, "official"); err != nil {
		t.Fatalf("canceled task unexpectedly blocked reads: %v", err)
	}
	id := commit(t, s, c, hash([]byte("after cancellation")))
	waitJob(t, s, id, "succeeded")
	j, err := s.Job(id)
	if err != nil || j.Error != "" {
		t.Fatalf("worker did not continue after cancellation: %+v %v", j, err)
	}
}

package personalmemory

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

type fakeIndex struct {
	ids    []string
	writes []Record
	err    error
	search func()
}

func (f *fakeIndex) Identity() string            { return "fake-index" }
func (f *fakeIndex) Check(context.Context) error { return f.err }
func (f *fakeIndex) Sync(_ context.Context, r Record) error {
	f.writes = append(f.writes, r)
	return f.err
}
func (f *fakeIndex) Search(context.Context, []string, string, int) ([]string, error) {
	if f.search != nil {
		f.search()
	}
	return f.ids, f.err
}

func TestRecallFiltersStaleDeletedAndOtherProjectCandidates(t *testing.T) {
	ctx := context.Background()
	store := openTest(t, filepath.Join(t.TempDir(), "memory.db"))
	index := &fakeIndex{}
	svc := New(store, "/project-one", index)
	old := putTest(t, store, "1", svc.scopes[0], "target", "obsolete-Java", 0)
	putTest(t, store, "2", svc.scopes[0], "target", "current-Go", 1)
	deleted := putTest(t, store, "3", svc.scopes[0], "secret", "deleted-value", 0)
	if _, err := store.Mutate(ctx, "4", svc.scopes[0], "secret", "", "", 1, true); err != nil {
		t.Fatal(err)
	}
	other := putTest(t, store, "5", store.ProjectScope("/other"), "plan", "other-project", 0)
	index.ids = []string{old.VectorID, deleted.VectorID, other.VectorID}
	text, err := svc.Recall(ctx, "career")
	if err != nil || !strings.Contains(text, "current-Go") || strings.Contains(text, "obsolete-Java") || strings.Contains(text, "deleted-value") || strings.Contains(text, "other-project") {
		t.Fatal(text, err)
	}
	index.err = errors.New("vector offline")
	if text, err = svc.Recall(ctx, "career"); err != nil || !strings.Contains(text, "current-Go") {
		t.Fatal("local fallback failed", text, err)
	}
	// Correct a fact while remote search is in flight. The snapshot assembled
	// before the search must not leak the now-obsolete content.
	index.err = nil
	index.search = func() { putTest(t, store, "6", svc.scopes[0], "target", "newest-Go", 2) }
	text, err = svc.Recall(ctx, "career")
	if err != nil || strings.Contains(text, "current-Go") {
		t.Fatal("raced correction leaked", text, err)
	}
}

func TestOutboxSkipsObsoleteAndRetriesIdempotentVersions(t *testing.T) {
	ctx := context.Background()
	store := openTest(t, filepath.Join(t.TempDir(), "memory.db"))
	index := &fakeIndex{}
	svc := New(store, "/one", index)
	putTest(t, store, "1", store.UserScope(), "target", "old", 0)
	r := putTest(t, store, "2", store.UserScope(), "target", "new", 1)
	if _, err := svc.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if len(index.writes) != 0 {
		t.Fatal("obsolete version indexed")
	}
	index.err = errors.New("offline")
	if _, err := svc.ProcessOne(ctx); err == nil {
		t.Fatal("expected outage")
	}
	status, _ := store.Status(ctx)
	if status["pending"] != 1 || status["done"] != 1 {
		t.Fatal(status)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE pm_outbox SET available_at=0"); err != nil {
		t.Fatal(err)
	}
	index.err = nil
	if _, err := svc.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if len(index.writes) != 1 || index.writes[0].VectorID != r.VectorID {
		t.Fatal(index.writes)
	}
	// Simulate a lost acknowledgement after successful external upsert. Replaying
	// the immutable version uses the exact same vector key.
	if _, err := store.db.ExecContext(ctx, "UPDATE pm_outbox SET status='running',lease_until=0 WHERE version=2"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if len(index.writes) != 2 || index.writes[1].VectorID != r.VectorID {
		t.Fatal("retry changed vector identity")
	}
}

func TestExplicitCommandsCorrectAndForget(t *testing.T) {
	ctx := context.Background()
	s := openTest(t, filepath.Join(t.TempDir(), "memory.db"))
	svc := New(s, "/one", nil)
	var out bytes.Buffer
	for _, input := range []string{"/remember user career.target Go 后端实习", "/remember user career.target Agent 开发实习", "/remember project stack Go + SQLite", "/memory user", "/memory status", "/forget user career.target"} {
		handled, err := svc.Command(ctx, input, "session:test", &out)
		if !handled || err != nil {
			t.Fatal(input, err)
		}
	}
	if handled, err := svc.Command(ctx, "普通提问，不保存为事实", "", &out); handled || err != nil {
		t.Fatal("ordinary query became memory")
	}
	r, err := s.Get(ctx, s.UserScope(), "career.target")
	if err != nil || !r.Deleted || r.Version != 3 {
		t.Fatal(r, err)
	}
	text, err := svc.Recall(ctx, "preferences")
	if err != nil || strings.Contains(text, "career.target") || !strings.Contains(text, "Go + SQLite") {
		t.Fatal(text, err)
	}
}

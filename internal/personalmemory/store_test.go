package personalmemory

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func openTest(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func putTest(t *testing.T, s *Store, op, scope, key, text string, version int64) Record {
	t.Helper()
	r, err := s.Mutate(context.Background(), op, scope, key, text, "explicit-user", version, false)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDurableVersionsReceiptsAndIsolation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "memory.db")
	s := openTest(t, path)
	scope := s.UserScope()
	project := s.ProjectScope("/one")
	r := putTest(t, s, "operation-1", scope, "career.target", "Java", 0)
	repeated := putTest(t, s, "operation-1", scope, "career.target", "Java", 0)
	if repeated.Version != 1 || repeated.ID != r.ID {
		t.Fatal("duplicate operation mutated memory")
	}
	if _, err := s.Mutate(ctx, "operation-1", scope, "career.target", "different", "explicit-user", 0, false); !errors.Is(err, ErrOperation) {
		t.Fatal("operation collision", err)
	}
	corrected := putTest(t, s, "operation-2", scope, "career.target", "Go", 1)
	if _, err := s.Resolve(ctx, r.VectorID, []string{scope, project}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("old vector recalled", err)
	}
	if _, err := s.Mutate(ctx, "conflict", scope, "career.target", "stale", "", 1, false); !errors.Is(err, ErrConflict) {
		t.Fatal("stale update accepted", err)
	}
	p := putTest(t, s, "project-op", project, "stack", "Go", 0)
	if _, err := s.Resolve(ctx, p.VectorID, []string{scope, s.ProjectScope("/two")}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("other workspace leaked", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, path)
	if s.UserScope() != scope {
		t.Fatal("profile changed on reopen")
	}
	actual, err := s.Get(ctx, scope, "career.target")
	if err != nil || actual.Content != "Go" || actual.Version != 2 {
		t.Fatal("not durable", actual, err)
	}
	tombstone, err := s.Mutate(ctx, "delete", scope, "career.target", "", "explicit-delete", 2, true)
	if err != nil || !tombstone.Deleted || tombstone.Version != 3 {
		t.Fatal(tombstone, err)
	}
	if _, err = s.Resolve(ctx, corrected.VectorID, []string{scope, project}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("deleted memory recalled", err)
	}
	revived := putTest(t, s, "revive", scope, "career.target", "Go backend", 3)
	if revived.Version != 4 {
		t.Fatal("delete reset version")
	}
	status, err := s.Status(ctx)
	if err != nil || status["pending"] != 5 {
		t.Fatal("outbox not atomic/idempotent", status, err)
	}
	// Receipt remains stable after subsequent edits and deletion/recreation.
	old, err := s.Mutate(ctx, "operation-1", scope, "career.target", "Java", "explicit-user", 0, false)
	if err != nil || old.Version != 1 {
		t.Fatal("lost receipt", old, err)
	}
}

func TestMutationRollsBackFactAndReceiptWhenOutboxFails(t *testing.T) {
	ctx := context.Background()
	s := openTest(t, filepath.Join(t.TempDir(), "memory.db"))
	_, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_outbox BEFORE INSERT ON pm_outbox BEGIN SELECT RAISE(ABORT,'simulated disk write failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Mutate(ctx, "retry-me", s.UserScope(), "preference", "中文", "", 0, false); err == nil {
		t.Fatal("expected failure")
	}
	if _, err = s.Get(ctx, s.UserScope(), "preference"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("partially committed fact", err)
	}
	var n int
	if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM pm_operations").Scan(&n); err != nil || n != 0 {
		t.Fatal("partially committed receipt", n, err)
	}
	if _, err = s.db.ExecContext(ctx, "DROP TRIGGER reject_outbox"); err != nil {
		t.Fatal(err)
	}
	putTest(t, s, "retry-me", s.UserScope(), "preference", "中文", 0)
}

func TestIndependentConnectionsCASAndExclusiveClaims(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")
	a := openTest(t, path)
	b := openTest(t, path)
	scope := a.UserScope()
	putTest(t, a, "create", scope, "target", "initial", 0)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, s := range []*Store{a, b} {
		wg.Add(1)
		go func(i int, s *Store) {
			defer wg.Done()
			_, err := s.Mutate(ctx, []string{"a", "b"}[i], scope, "target", "update", "", 1, false)
			errs <- err
		}(i, s)
	}
	wg.Wait()
	close(errs)
	success, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatal(success, conflicts)
	}
	jobs := make(chan *Job, 2)
	for _, s := range []*Store{a, b} {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			j, err := s.claim(ctx)
			if err != nil {
				t.Error(err)
			}
			jobs <- j
		}(s)
	}
	wg.Wait()
	close(jobs)
	seen := map[int64]bool{}
	for j := range jobs {
		if j == nil || seen[j.ID] {
			t.Fatal("duplicate job claim", j)
		}
		seen[j.ID] = true
	}
	// An expired claim is recoverable; the previous owner cannot finish its lease.
	old, err := a.claim(ctx)
	if err != nil || old != nil {
		t.Fatal("running jobs were claimed", old, err)
	}
	var id int64
	var owner string
	if err = a.db.QueryRowContext(ctx, "SELECT id,owner FROM pm_outbox LIMIT 1").Scan(&id, &owner); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.ExecContext(ctx, "UPDATE pm_outbox SET lease_until=0 WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	recovered, err := b.claim(ctx)
	if err != nil || recovered == nil || recovered.ID != id || recovered.Owner == owner {
		t.Fatal("lease not recovered", recovered, err)
	}
	if err = a.finish(ctx, &Job{ID: id, Owner: owner}, nil); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = a.db.QueryRowContext(ctx, "SELECT status FROM pm_outbox WHERE id=?", id).Scan(&status); err != nil || status != "running" {
		t.Fatal("stale worker finished new claim", status, err)
	}
}

func TestIndexIdentityCannotSilentlyChange(t *testing.T) {
	ctx := context.Background()
	s := openTest(t, filepath.Join(t.TempDir(), "memory.db"))
	if err := s.BindIndex(ctx, "collection-A:model-A"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindIndex(ctx, "collection-B:model-B"); err == nil {
		t.Fatal("accepted index switch without rebuilding")
	}
}

package research

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lannokaede/LanAgent/internal/rag"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeWeb struct{ body string }

func (w fakeWeb) Search(context.Context, string, int) ([]Page, error) {
	return []Page{{URL: "https://go.dev/blog/context", Title: "Context", Text: "must not include"}, {URL: "https://evil.invalid/no", Title: "Filtered"}}, nil
}
func (w fakeWeb) Fetch(_ context.Context, u string) (Page, error) {
	return Page{URL: u, Title: "Context", Text: w.body}, nil
}

type fakeIndex struct {
	mu      sync.Mutex
	count   int
	fail    bool
	started chan struct{}
	wait    bool
}

func (f *fakeIndex) Index(ctx context.Context, _ *rag.PreparedDocument) error {
	f.mu.Lock()
	f.count++
	failed, wait, started := f.fail, f.wait, f.started
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if wait {
		<-ctx.Done()
		return ctx.Err()
	}
	if failed {
		return errors.New("private-key-must-not-leak")
	}
	return nil
}
func (*fakeIndex) Search(context.Context, string, string, int) ([]rag.Hit, error) {
	return []rag.Hit{}, nil
}
func newService(t *testing.T, dir string, index *fakeIndex) *Service {
	t.Helper()
	s, e := Open(context.Background(), Options{Directory: dir, Namespace: "test-go", Binding: "test-index", AllowWrite: true, Web: fakeWeb{strings.Repeat("上下文取消 context goroutine\n", 1000)}, Indexer: index})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func candidate(t *testing.T, s *Service) Candidate {
	t.Helper()
	p, e := s.Fetch(context.Background(), "https://go.dev/blog/context?utm_source=x")
	if e != nil {
		t.Fatal(e)
	}
	c, e := s.Stage(context.Background(), []string{p.ArtifactID}, "official")
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func commit(t *testing.T, s *Service, c Candidate, id string) string {
	t.Helper()
	args := map[string]any{"candidate_id": c.ID, "manifest_hash": c.Manifest}
	b, e := s.Commit(c.ID, c.Manifest, id, hashJSON(args))
	if e != nil {
		t.Fatal(e)
	}
	var out struct {
		JobID string `json:"job_id"`
	}
	if json.Unmarshal(b, &out) != nil || out.JobID == "" {
		t.Fatal(string(b))
	}
	return out.JobID
}
func waitJob(t *testing.T, s *Service, id, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, e := s.Job(id)
		if e != nil {
			t.Fatal(e)
		}
		if j.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	j, _ := s.Job(id)
	t.Fatalf("job status %s, want %s", j.Status, want)
}
func TestArtifactsAndStageIntegrity(t *testing.T) {
	s := newService(t, t.TempDir(), &fakeIndex{})
	defer s.Close()
	src, e := s.Fetch(context.Background(), "https://go.dev/blog/context?utm_source=x")
	if e != nil {
		t.Fatal(e)
	}
	var b strings.Builder
	offset := 0
	for {
		p, e := s.ReadArtifact(src.ArtifactID, offset)
		if e != nil {
			t.Fatal(e)
		}
		b.WriteString(p.Content)
		if !p.Truncated {
			break
		}
		if p.NextOffset <= offset {
			t.Fatal("offset stalled")
		}
		offset = p.NextOffset
	}
	raw, _ := s.body(src.ArtifactID)
	if b.String() != string(raw) {
		t.Fatal("pagination corrupted source")
	}
	found, e := s.SearchArtifact(src.ArtifactID, "取消")
	if e != nil || len(found) == 0 {
		t.Fatal(found, e)
	}
	if _, e = s.Fetch(context.Background(), "https://go.dev.evil.invalid/page"); e == nil {
		t.Fatal("host suffix escape")
	}
	if _, e = s.Stage(context.Background(), []string{src.ArtifactID}, "interview_experience"); e == nil {
		t.Fatal("source kind spoof")
	}
	os.WriteFile(filepath.Join(s.opts.Directory, "artifacts", src.ArtifactID+".md"), []byte("changed"), 0600)
	if _, e = s.Stage(context.Background(), []string{src.ArtifactID}, "official"); e == nil {
		t.Fatal("tampered artifact accepted")
	}
}
func TestDurableCommitDedupAndReopen(t *testing.T) {
	dir := t.TempDir()
	index := &fakeIndex{}
	s := newService(t, dir, index)
	c := candidate(t, s)
	id := hash([]byte("execution"))
	job := commit(t, s, c, id)
	waitJob(t, s, job, "succeeded")
	if again := commit(t, s, c, id); again != job {
		t.Fatal("duplicated same execution")
	}
	if again := commit(t, s, c, hash([]byte("other execution"))); again != job {
		t.Fatal("duplicated imported manifest")
	}
	if _, e := s.Commit(c.ID, c.Manifest, id, hash([]byte("different args"))); e == nil {
		t.Fatal("id collision accepted")
	}
	s.Close()
	s = newService(t, dir, index)
	defer s.Close()
	op, e := s.Operation(id)
	if e != nil || !strings.Contains(string(op.Result), job) {
		t.Fatal(op, e)
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	if index.count != 1 {
		t.Fatalf("indexed %d times", index.count)
	}
}
func TestShutdownAndFailedImportMustRepair(t *testing.T) {
	dir := t.TempDir()
	index := &fakeIndex{wait: true, started: make(chan struct{}, 1)}
	s := newService(t, dir, index)
	c := candidate(t, s)
	job := commit(t, s, c, hash([]byte("start")))
	select {
	case <-index.started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker never started")
	}
	if _, e := s.SearchKnowledge(context.Background(), "context", 1, "official"); !errors.Is(e, ErrBusy) {
		t.Fatal("search allowed partial import", e)
	}
	s.Close()
	index.mu.Lock()
	index.wait = false
	index.fail = true
	index.mu.Unlock()
	s = newService(t, dir, index)
	waitJob(t, s, job, "failed")
	if _, e := s.SearchKnowledge(context.Background(), "context", 1, "official"); !errors.Is(e, ErrBusy) {
		t.Fatal("failed partial import visible")
	}
	status, _ := s.Job(job)
	if strings.Contains(status.Error, "private-key") {
		t.Fatal("index error leaked")
	}
	index.mu.Lock()
	index.fail = false
	index.mu.Unlock()
	repair := commit(t, s, c, hash([]byte("repair")))
	waitJob(t, s, repair, "succeeded")
	s.Close()
}
func TestMCPProtocolSchemaAndMetadata(t *testing.T) {
	s := newService(t, t.TempDir(), &fakeIndex{})
	defer s.Close()
	ctx := context.Background()
	a, b := mcp.NewInMemoryTransports()
	ss, e := s.Server().Connect(ctx, a, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, e := client.Connect(ctx, b, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer cs.Close()
	count := 0
	for _, e := range cs.Tools(ctx, nil) {
		if e != nil {
			t.Fatal(e)
		}
		count++
	}
	if count != 12 {
		t.Fatalf("tool count %d", count)
	}
	bad, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "web_fetch", Arguments: map[string]any{"url": 42}})
	if e == nil && !bad.IsError {
		t.Fatal("invalid schema accepted")
	}
	c := candidate(t, s)
	args := map[string]any{"candidate_id": c.ID, "manifest_hash": c.Manifest}
	out, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "knowledge_commit", Arguments: args})
	if e == nil && !out.IsError {
		t.Fatal("missing host metadata accepted")
	}
	id := hash([]byte("mcp execution"))
	out, e = cs.CallTool(ctx, &mcp.CallToolParams{Name: "knowledge_commit", Arguments: args, Meta: mcp.Meta{"io.lanagent/execution_id": id}})
	if e != nil || out.IsError {
		t.Fatal(out, e)
	}
	op, e := s.Operation(id)
	if e != nil || op.ArgumentsHash != hashJSON(args) {
		t.Fatal(op, e)
	}
}
func TestReadOnlyAndDirectoryOwnership(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(context.Background(), Options{Directory: dir, Namespace: "test-go"})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e := Open(context.Background(), Options{Directory: dir, Namespace: "test-go"}); e == nil {
		t.Fatal("two owners")
	}
	if _, e := s.Commit("", "", hash([]byte("x")), hash([]byte("args"))); e == nil {
		t.Fatal("read-only write allowed")
	}
}

func TestRequestBudgetSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	o := Options{Directory: dir, Namespace: "budget", Web: fakeWeb{"test"}, MaxSearchCalls: 1}
	s, e := Open(context.Background(), o)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.SearchWeb(context.Background(), "context", 1); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(context.Background(), o)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.SearchWeb(context.Background(), "context", 1); e == nil {
		t.Fatal("budget reset on restart")
	}
}

func TestArtifactPageFitsModelObservationIncludingEscaping(t *testing.T) {
	s := newService(t, t.TempDir(), &fakeIndex{})
	defer s.Close()
	s.opts.Web = fakeWeb{strings.Repeat("<>&中文", 2000)}
	src, e := s.Fetch(context.Background(), "https://go.dev/ref/mem")
	if e != nil {
		t.Fatal(e)
	}
	var b strings.Builder
	offset := 0
	for {
		page, e := s.ReadArtifact(src.ArtifactID, offset)
		if e != nil {
			t.Fatal(e)
		}
		encoded, _ := json.Marshal(page)
		if len(encoded) > 3500 {
			t.Fatal("model observation would be compacted")
		}
		b.WriteString(page.Content)
		if !page.Truncated {
			break
		}
		if page.NextOffset <= offset {
			t.Fatal("pagination stalled")
		}
		offset = page.NextOffset
	}
	raw, _ := s.body(src.ArtifactID)
	if b.String() != string(raw) {
		t.Fatal("escaped page lost content")
	}
}

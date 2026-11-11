package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/lannokaede/LanAgent/internal/corpus"
	"github.com/lannokaede/LanAgent/internal/filelock"
)

type Service struct {
	siteIndex    *SiteIndex
	crawler      *corpus.Crawler
	fetchMu      sync.Mutex
	opts         Options
	root         *os.Root
	lock         *os.File
	mu           sync.Mutex
	data         state
	writeMu      sync.RWMutex
	artifactMu   sync.Mutex
	ctx          context.Context
	cancel       context.CancelFunc
	activeCancel context.CancelFunc
	activeJob    string
	wake         chan struct{}
	wg           sync.WaitGroup
}

func Open(ctx context.Context, o Options) (*Service, error) {
	if o.Namespace == "" || len(o.Namespace) > 40 {
		return nil, errors.New("namespace must contain 1..40 lowercase letters, digits or hyphens")
	}
	for _, c := range o.Namespace {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return nil, errors.New("invalid namespace")
		}
	}
	if o.Namespace[0] < 'a' || o.Namespace[0] > 'z' {
		return nil, errors.New("namespace must start with a letter")
	}
	if o.MaxSearchCalls <= 0 {
		o.MaxSearchCalls = 20
	}
	if o.MaxFetchCalls <= 0 {
		o.MaxFetchCalls = 100
	}
	if len(o.AllowedHosts) == 0 {
		o.AllowedHosts = []string{"go.dev", "redis.io", "www.postgresql.org", "nowcoder.com", "juejin.cn"}
	}
	if e := os.MkdirAll(filepath.Join(o.Directory, "artifacts"), 0700); e != nil {
		return nil, e
	}
	root, e := os.OpenRoot(o.Directory)
	if e != nil {
		return nil, e
	}
	lock, e := root.OpenFile("owner.lock", os.O_RDWR|os.O_CREATE, 0600)
	if e != nil {
		root.Close()
		return nil, e
	}
	if e = filelock.Lock(lock); e != nil {
		lock.Close()
		root.Close()
		return nil, errors.New("research data directory is already in use")
	}
	c, cancel := context.WithCancel(ctx)
	s := &Service{crawler: corpus.NewCrawler(), opts: o, root: root, lock: lock, ctx: c, cancel: cancel, wake: make(chan struct{}, 1)}
	fail := func(e error) (*Service, error) { s.Close(); return nil, e }
	s.siteIndex, e = OpenSiteIndex(o.Directory)
	if e != nil {
		return fail(e)
	}
	binding := hashJSON([]any{o.Namespace, o.AllowedHosts})
	s.data = state{Version: 1, Binding: binding, Sources: map[string]Source{}, Artifacts: map[string]Source{}, Candidates: map[string]Candidate{}, Jobs: map[string]Job{}, Operations: map[string]Operation{}}
	f, e := root.Open("state.json")
	if e == nil {
		b, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
		f.Close()
		if err != nil || len(b) > 16<<20 {
			return fail(errors.New("invalid research state size"))
		}
		if e = json.Unmarshal(b, &s.data); e != nil {
			return fail(errors.New("invalid research state"))
		}
		if s.data.Version != 1 || s.data.Binding != binding || s.data.Sources == nil || s.data.Artifacts == nil || s.data.Candidates == nil || s.data.Jobs == nil || s.data.Operations == nil {
			return fail(errors.New("research data profile differs or is corrupt"))
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return fail(e)
	}
	if o.Binding != "" {
		indexBinding := hashJSON(o.Binding)
		if s.data.IndexBinding != "" && s.data.IndexBinding != indexBinding {
			return fail(errors.New("knowledge index binding differs from persistent jobs"))
		}
		if e = s.update(func(st *state) error { st.IndexBinding = indexBinding; return nil }); e != nil {
			return fail(e)
		}
	}
	if o.AllowWrite && o.Indexer == nil {
		return fail(errors.New("knowledge write requires an indexer"))
	}
	if o.AllowWrite {
		if e = s.update(func(st *state) error {
			for id, j := range st.Jobs {
				if j.Status == "running" {
					j.Status = "queued"
					st.Jobs[id] = j
				}
			}
			return nil
		}); e != nil {
			return fail(e)
		}
		s.wg.Add(1)
		go s.worker()
		s.signal()
	}
	return s, nil
}
func (s *Service) Close() error {
	s.cancel()
	s.wg.Wait()
	filelock.Unlock(s.lock)
	s.lock.Close()
	var indexErr error
	if s.siteIndex != nil {
		indexErr = s.siteIndex.Close()
	}
	return errors.Join(indexErr, s.root.Close())
}
func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Service) update(fn func(*state) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, e := json.Marshal(s.data)
	if e != nil {
		return e
	}
	var next state
	if e = json.Unmarshal(b, &next); e != nil {
		return e
	}
	if e = fn(&next); e != nil {
		return e
	}
	b, e = json.Marshal(next)
	if e != nil {
		return e
	}
	if len(b) > 16<<20 {
		return errors.New("research catalog capacity reached")
	}
	if e = s.atomic("state.json", b); e != nil {
		return e
	}
	s.data = next
	return nil
}
func (s *Service) atomic(name string, b []byte) error {
	tmp := name + ".tmp"
	f, e := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		s.root.Remove(tmp)
		return e
	}
	if e = s.root.Rename(tmp, name); e != nil {
		return e
	}
	// Sync the containing directory too, not only the replacement file.
	d, e := s.root.Open(filepath.Dir(name))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func (s *Service) body(id string) ([]byte, error) {
	if !validID(id) {
		return nil, errors.New("invalid artifact id")
	}
	s.mu.Lock()
	_, ok := s.data.Artifacts[id]
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("artifact not found")
	}
	f, e := s.root.Open("artifacts/" + id + ".md")
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, MaxBody+1))
	if e != nil {
		return nil, e
	}
	if len(b) > MaxBody {
		return nil, errors.New("artifact too large")
	}
	s.mu.Lock()
	src := s.data.Artifacts[id]
	s.mu.Unlock()
	if hash(b) != src.Hash {
		return nil, fmt.Errorf("artifact integrity mismatch")
	}
	return b, nil
}

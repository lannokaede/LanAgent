package research

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/lannokaede/LanAgent/internal/rag"
)

func (s *Service) prepare(ctx context.Context, id, kind string) (*rag.PreparedDocument, Source, error) {
	b, e := s.body(id)
	if e != nil {
		return nil, Source{}, e
	}
	s.mu.Lock()
	src := s.data.Artifacts[id]
	s.mu.Unlock()
	if src.Kind != kind {
		return nil, src, errors.New("source kind differs from host provenance")
	}
	p, e := rag.NewPreparer(rag.SplitConfig{ChunkSize: 512, ChunkOverlap: 64})
	if e != nil {
		return nil, src, e
	}
	suffix := "-official"
	if kind == "interview_experience" {
		suffix = "-experience"
	}
	doc, e := p.Prepare(ctx, s.opts.Namespace+suffix, src.ID+".md", bytes.NewReader(b))
	if e != nil {
		return nil, src, e
	}
	if len(doc.Chunks) > 2048 {
		return nil, src, errors.New("document exceeds 2048 chunks")
	}
	for i := range doc.Chunks {
		doc.Chunks[i].Source = src.URL
		if len(src.URL) > 255 {
			doc.Chunks[i].Source = src.ID + ".md"
		}
	}
	src.DocumentID = doc.Document.ID
	src.RevisionID = doc.Document.RevisionID
	return doc, src, nil
}
func (s *Service) Stage(ctx context.Context, ids []string, kind string) (Candidate, error) {
	if len(ids) < 1 || len(ids) > 8 || kind != "official" && kind != "interview_experience" {
		return Candidate{}, errors.New("choose 1..8 artifacts and official or interview_experience")
	}
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	c := Candidate{Artifacts: []string{}, Kind: kind, CreatedAt: now()}
	seenSource := map[string]bool{}
	seenContent := map[string]bool{}
	for _, id := range ids {
		doc, src, e := s.prepare(ctx, id, kind)
		if e != nil {
			return Candidate{}, e
		}
		if seenSource[src.ID] {
			return Candidate{}, errors.New("candidate contains multiple artifacts for one source")
		}
		seenSource[src.ID] = true
		if seenContent[src.Hash] {
			continue
		}
		seenContent[src.Hash] = true
		c.Artifacts = append(c.Artifacts, id)
		c.Chunks += len(doc.Chunks)
	}
	if c.Chunks > 4096 {
		return Candidate{}, errors.New("candidate exceeds 4096 chunk budget")
	}
	c.Manifest = hashJSON([]any{c.Artifacts, c.Kind, s.opts.Namespace, "stage-v1"})
	c.ID = c.Manifest
	e := s.update(func(st *state) error {
		if _, ok := st.Candidates[c.ID]; ok {
			c = st.Candidates[c.ID]
			return nil
		}
		if len(st.Candidates) >= 512 {
			return errors.New("candidate catalog capacity reached")
		}
		st.Candidates[c.ID] = c
		return nil
	})
	return c, e
}
func (s *Service) Commit(candidateID, manifest, executionID, argsHash string) (json.RawMessage, error) {
	if !s.opts.AllowWrite {
		return nil, errors.New("knowledge write is disabled")
	}
	if !validID(executionID) || !validID(argsHash) {
		return nil, errors.New("commit requires host execution metadata; use LanAgent or supply a stable MCP _meta execution id")
	}
	var result json.RawMessage
	e := s.update(func(st *state) error {
		if op, ok := st.Operations[executionID]; ok {
			if op.Tool != "knowledge_commit" || op.ArgumentsHash != argsHash {
				return errors.New("execution id argument conflict")
			}
			result = op.Result
			return nil
		}
		c, ok := st.Candidates[candidateID]
		if !ok || c.Manifest != manifest {
			return errors.New("candidate manifest mismatch")
		}
		if len(st.Operations) >= 1024 || len(st.Jobs) >= 512 {
			return errors.New("operation catalog capacity reached")
		}
		if st.RepairCandidate != "" && st.RepairCandidate != candidateID {
			return ErrBusy
		}
		jobID := ""
		for _, j := range st.Jobs {
			if j.CandidateID == candidateID && j.Status == "succeeded" {
				jobID = j.ID
			}
			if j.Status == "queued" || j.Status == "running" {
				if j.CandidateID != candidateID {
					return ErrBusy
				}
				jobID = j.ID
			}
		}
		if jobID == "" {
			jobID = hashJSON([]string{candidateID, executionID})
			st.Jobs[jobID] = Job{ID: jobID, CandidateID: candidateID, Status: "queued", Total: len(c.Artifacts), CreatedAt: now()}
		}
		result, _ = json.Marshal(map[string]any{"status": "accepted", "job_id": jobID})
		st.Operations[executionID] = Operation{"found", executionID, "knowledge_commit", argsHash, result}
		return nil
	})
	if e == nil {
		s.signal()
	}
	return result, e
}
func (s *Service) Job(id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.data.Jobs[id]
	if !ok {
		return Job{}, errors.New("job not found")
	}
	return j, nil
}
func (s *Service) Operation(id string) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.data.Operations[id]
	if !ok {
		return Operation{}, errors.New("operation not found; outcome remains uncertain")
	}
	return o, nil
}
func (s *Service) CancelJob(id string) (Job, error) {
	if !s.opts.AllowWrite {
		return Job{}, errors.New("knowledge write is disabled")
	}
	e := s.update(func(st *state) error {
		j, ok := st.Jobs[id]
		if !ok {
			return errors.New("job not found")
		}
		if j.Status == "queued" {
			j.Status = "cancelled"
		}
		if j.Status == "running" || j.Status == "cancelled" {
			j.CancelRequested = true
		}
		st.Jobs[id] = j
		return nil
	})
	if e != nil {
		return Job{}, e
	}
	s.mu.Lock()
	if s.activeJob == id && s.activeCancel != nil {
		s.activeCancel()
	}
	s.mu.Unlock()
	return s.Job(id)
}
func (s *Service) Sources(kind string, offset int) (map[string]any, error) {
	if kind != "" && kind != "official" && kind != "interview_experience" || offset < 0 {
		return nil, errors.New("invalid source filter")
	}
	s.mu.Lock()
	all := []Source{}
	for _, src := range s.data.Sources {
		if kind == "" || src.Kind == kind {
			all = append(all, src)
		}
	}
	s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	if offset > len(all) {
		return nil, errors.New("source offset exceeds count")
	}
	end := min(len(all), offset+10)
	return map[string]any{"sources": all[offset:end], "next_offset": end, "truncated": end < len(all)}, nil
}
func (s *Service) Document(id string) (Source, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, src := range s.data.Sources {
		if src.DocumentID == id || src.ID == id {
			return src, nil
		}
	}
	return Source{}, errors.New("document not found")
}
func (s *Service) SearchKnowledge(ctx context.Context, q string, n int, kind string) ([]rag.Hit, error) {
	if s.opts.Indexer == nil {
		return nil, errors.New("knowledge indexer not configured")
	}
	if strings.TrimSpace(q) == "" || len(q) > 3072 || n < 1 || n > 5 || kind != "official" && kind != "interview_experience" {
		return nil, errors.New("invalid knowledge query or source kind")
	}
	if !s.writeMu.TryRLock() {
		return nil, ErrBusy
	}
	defer s.writeMu.RUnlock()
	s.mu.Lock()
	blocked := s.data.RepairCandidate != ""
	s.mu.Unlock()
	if blocked {
		return nil, ErrBusy
	}
	suffix := "-official"
	if kind == "interview_experience" {
		suffix = "-experience"
	}
	hits, err := s.opts.Indexer.Search(ctx, s.opts.Namespace+suffix, q, n)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range hits {
		for _, source := range s.data.Sources {
			if source.DocumentID == hits[i].DocumentID {
				hits[i].Source = source.URL
				break
			}
		}
	}
	return hits, nil
}
func (s *Service) worker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		}
		for {
			s.mu.Lock()
			var next Job
			for _, j := range s.data.Jobs {
				if j.Status == "queued" {
					next = j
					break
				}
			}
			s.mu.Unlock()
			if next.ID == "" {
				break
			}
			if !s.runJob(next) {
				return
			}
		}
	}
}
func (s *Service) runJob(job Job) bool {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.ctx.Err() != nil {
		return false
	}
	e := s.update(func(st *state) error {
		j := st.Jobs[job.ID]
		if j.Status != "queued" {
			return errors.New("job no longer queued")
		}
		j.Status = "running"
		st.Jobs[j.ID] = j
		st.RepairCandidate = j.CandidateID
		return nil
	})
	if e != nil {
		return true
	}
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Minute)
	defer cancel()
	s.mu.Lock()
	s.activeJob = job.ID
	s.activeCancel = cancel
	c := s.data.Candidates[job.CandidateID]
	if s.data.Jobs[job.ID].CancelRequested {
		cancel()
	}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.activeJob = ""; s.activeCancel = nil; s.mu.Unlock() }()
	for i, id := range c.Artifacts {
		if ctx.Err() != nil {
			e = ctx.Err()
			break
		}
		var doc *rag.PreparedDocument
		var src Source
		doc, src, e = s.prepare(ctx, id, c.Kind)
		if e != nil {
			break
		}
		s.mu.Lock()
		old := s.data.Sources[src.ID]
		s.mu.Unlock()
		if old.IndexedRevision != doc.Document.RevisionID {
			e = s.opts.Indexer.Index(ctx, doc)
			if e != nil {
				break
			}
		}
		e = s.update(func(st *state) error {
			current := st.Sources[src.ID]
			current.DocumentID = doc.Document.ID
			current.IndexedArtifact = id
			current.IndexedRevision = doc.Document.RevisionID
			st.Sources[src.ID] = current
			j := st.Jobs[job.ID]
			j.Completed = i + 1
			st.Jobs[j.ID] = j
			return nil
		})
		if e != nil {
			break
		}
	}
	if s.ctx.Err() != nil {
		return false
	} // Leave running for restart reconciliation.
	if e2 := s.update(func(st *state) error {
		j := st.Jobs[job.ID]
		switch {
		case e == nil:
			j.Status = "succeeded"
			j.Error = ""
			st.RepairCandidate = ""
		case j.CancelRequested:
			j.Status = "cancelled"
			j.Error = "partial import may exist; commit the same candidate to repair"
		default:
			j.Status = "failed"
			j.Error = "index_failed; inspect configuration and commit the same candidate to repair"
		}
		st.Jobs[j.ID] = j
		return nil
	}); e2 != nil {
		return false
	}
	return true
}

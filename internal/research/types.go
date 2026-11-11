// Package research owns source provenance, staged imports and durable jobs.
// MCP is only an adapter; state and indexing do not depend on a connection.
package research

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/lannokaede/LanAgent/internal/rag"
)

const MaxBody = 4 << 20

var ErrBusy = errors.New("knowledge namespace busy or needs repair; inspect job_status")

type Page struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Text        string `json:"text,omitempty"`
	Description string `json:"description,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
	FetchedAt   string `json:"fetched_at,omitempty"`
	Origin      string `json:"origin,omitempty"`
}
type Web interface {
	Search(context.Context, string, int) ([]Page, error)
	Fetch(context.Context, string) (Page, error)
}
type Indexer interface {
	Index(context.Context, *rag.PreparedDocument) error
	Search(context.Context, string, string, int) ([]rag.Hit, error)
}
type Options struct {
	MaxSearchCalls                int
	MaxFetchCalls                 int
	Directory, Namespace, Binding string
	AllowedHosts                  []string
	AllowWrite                    bool
	Web                           Web
	Indexer                       Indexer
}
type Source struct {
	ID              string `json:"source_id"`
	URL             string `json:"source_url"`
	Title           string `json:"title"`
	Kind            string `json:"source_kind"`
	ArtifactID      string `json:"artifact_id"`
	Hash            string `json:"content_hash"`
	FetchedAt       string `json:"fetched_at"`
	DocumentID      string `json:"document_id,omitempty"`
	RevisionID      string `json:"revision_id,omitempty"`
	IndexedArtifact string `json:"indexed_artifact,omitempty"`
	IndexedRevision string `json:"indexed_revision,omitempty"`
}
type Candidate struct {
	ID        string   `json:"candidate_id"`
	Manifest  string   `json:"manifest_hash"`
	Artifacts []string `json:"artifact_ids"`
	Kind      string   `json:"source_kind"`
	Chunks    int      `json:"estimated_chunks"`
	CreatedAt string   `json:"created_at"`
}
type Job struct {
	ID              string `json:"job_id"`
	CandidateID     string `json:"candidate_id"`
	Status          string `json:"status"`
	Completed       int    `json:"completed"`
	Total           int    `json:"total"`
	Error           string `json:"error,omitempty"`
	CancelRequested bool   `json:"cancel_requested"`
	CreatedAt       string `json:"created_at"`
}
type Operation struct {
	Status        string          `json:"status"`
	ExecutionID   string          `json:"execution_id"`
	Tool          string          `json:"tool"`
	ArgumentsHash string          `json:"arguments_sha256"`
	Result        json.RawMessage `json:"result"`
}
type state struct {
	SearchCalls     int                  `json:"search_calls"`
	FetchCalls      int                  `json:"fetch_calls"`
	IndexBinding    string               `json:"index_binding,omitempty"`
	Version         int                  `json:"version"`
	Binding         string               `json:"binding"`
	Sources         map[string]Source    `json:"sources"`
	Artifacts       map[string]Source    `json:"artifacts"`
	Candidates      map[string]Candidate `json:"candidates"`
	Jobs            map[string]Job       `json:"jobs"`
	Operations      map[string]Operation `json:"operations"`
	RepairCandidate string               `json:"repair_candidate,omitempty"`
}

func hash(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func hashJSON(v any) string   { b, _ := json.Marshal(v); return hash(b) }
func now() string             { return time.Now().UTC().Format(time.RFC3339Nano) }
func validID(id string) bool {
	if len(id) != 64 {
		return false
	}
	b, e := hex.DecodeString(id)
	return e == nil && len(b) == 32
}
func ArgumentsHash(raw []byte) (string, error) {
	var x any
	if e := json.Unmarshal(raw, &x); e != nil {
		return "", e
	}
	return hashJSON(x), nil
}

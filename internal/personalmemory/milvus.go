package personalmemory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/lannokaede/LanAgent/internal/rag"
)

// Milvus uses the existing RAG schema in a dedicated collection with a distinct
// identity. Each memory version has a separate immutable vector primary key.
type Milvus struct {
	store                           *rag.MilvusStore
	embed                           *rag.EmbeddingClient
	base, key, collection, identity string
	client                          *http.Client
}

func NewMilvus(base, key, collection string, embed *rag.EmbeddingClient, dimensions int) (*Milvus, error) {
	if embed == nil {
		return nil, errors.New("memory embedding is required")
	}
	identity := "lanagent-personal-memory-v1:" + embed.Identity()
	store, err := rag.NewMilvusStore(base, key, collection, dimensions, identity)
	if err != nil {
		return nil, err
	}
	return &Milvus{store: store, embed: embed, base: strings.TrimRight(base, "/"), key: key, collection: collection, identity: identity, client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (m *Milvus) Identity() string                { return m.base + "/" + m.collection + ":" + m.identity }
func (m *Milvus) Init(ctx context.Context) error  { return m.store.Init(ctx) }
func (m *Milvus) Check(ctx context.Context) error { return m.store.Check(ctx) }
func (m *Milvus) Close()                          { m.store.Close(); m.embed.Close(); m.client.CloseIdleConnections() }
func (m *Milvus) call(ctx context.Context, route string, payload map[string]any, out any) error {
	payload["collectionName"] = m.collection
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.base+"/v2/vectordb/"+route, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.key != "" {
		req.Header.Set("Authorization", "Bearer "+m.key)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("personal memory index request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("personal memory index HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("personal memory index response too large")
	}
	var env struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &env) != nil || env.Code == nil {
		return errors.New("invalid personal memory index response")
	}
	if *env.Code != 0 {
		return fmt.Errorf("personal memory index code %d", *env.Code)
	}
	if out != nil {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}
func (m *Milvus) Sync(ctx context.Context, r Record) error {
	if !r.Deleted {
		vector, err := m.embed.EmbedQuery(ctx, r.Key+": "+r.Content)
		if err != nil {
			return err
		}
		row := map[string]any{"id": r.VectorID, "vector": vector, "kb": r.Scope, "document_id": r.ID, "revision_id": strconv.FormatInt(r.Version, 10), "source": r.Key, "text": r.Content, "chunk_index": r.Version}
		if err = m.call(ctx, "entities/upsert", map[string]any{"data": []any{row}}, nil); err != nil {
			return err
		}
	}
	// Never delete newer versions when an older worker completes late. Tombstones
	// remove <= version; updates clean only older versions. SQLite filters any
	// obsolete vector that a delayed writer might subsequently reintroduce.
	cmp := "<"
	if r.Deleted {
		cmp = "<="
	}
	filter := fmt.Sprintf("kb == %s and document_id == %s and chunk_index %s %d", strconv.Quote(r.Scope), strconv.Quote(r.ID), cmp, r.Version)
	return m.call(ctx, "entities/delete", map[string]any{"filter": filter}, nil)
}
func (m *Milvus) Search(ctx context.Context, scopes []string, q string, limit int) ([]string, error) {
	if len(scopes) != 2 || limit < 1 || limit > 20 {
		return nil, errors.New("invalid personal memory search")
	}
	if err := m.Check(ctx); err != nil {
		return nil, err
	}
	// Bound query bytes without splitting UTF-8; embedding input is <=8192 bytes.
	for len(q) > 8192 {
		_, size := utf8.DecodeLastRuneInString(q)
		q = q[:len(q)-size]
	}
	v, err := m.embed.EmbedQuery(ctx, q)
	if err != nil {
		return nil, err
	}
	filter := fmt.Sprintf("kb in [%s,%s]", strconv.Quote(scopes[0]), strconv.Quote(scopes[1]))
	var hits []struct {
		ID string `json:"id"`
	}
	err = m.call(ctx, "entities/search", map[string]any{"data": [][]float32{v}, "annsField": "vector", "filter": filter, "limit": limit, "outputFields": []string{"id"}, "consistencyLevel": "Strong", "searchParams": map[string]any{"metricType": "COSINE", "params": map[string]any{}}}, &hits)
	if err != nil {
		return nil, err
	}
	if len(hits) > limit {
		return nil, errors.New("personal memory result count exceeds limit")
	}
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		if len(h.ID) != 64 {
			return nil, errors.New("invalid memory vector ID")
		}
		ids = append(ids, h.ID)
	}
	return ids, nil
}

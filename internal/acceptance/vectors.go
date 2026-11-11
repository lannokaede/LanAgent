package acceptance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/lannokaede/LanAgent/internal/config"
	"github.com/lannokaede/LanAgent/internal/personalmemory"
	"github.com/lannokaede/LanAgent/internal/rag"
)

func (v *verifier) vectors(ctx context.Context, cfg *config.Config) (m map[string]any, resultErr error) {
	owned := newCollections()
	suffix, memoryName, ragName := owned.suffix, owned.memory, owned.rag
	m = map[string]any{"embedding_model": cfg.Embedding.Model, "dimensions": cfg.Embedding.Dimensions, "memory_collection": memoryName, "rag_collection": ragName}
	// Cleanup touches only the two names allocated by this invocation.
	defer func() {
		finish, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleanupErr := owned.cleanup(finish, cfg)
		m["isolated_collections_cleaned"] = cleanupErr == nil
		resultErr = errors.Join(resultErr, cleanupErr)
	}()
	emb, err := rag.NewEmbeddingClient(cfg.Embedding.BaseURL, cfg.Embedding.Model, cfg.Embedding.APIKey, cfg.Embedding.Dimensions)
	if err != nil {
		return m, err
	}
	index, err := personalmemory.NewMilvus(cfg.RAG.MilvusURL, cfg.RAG.MilvusToken, memoryName, emb, cfg.Embedding.Dimensions)
	if err != nil {
		emb.Close()
		return m, err
	}
	defer index.Close()
	if err = index.Init(ctx); err != nil {
		return m, err
	}
	store, err := personalmemory.Open(ctx, filepath.Join(v.work, "acceptance-memory.db"))
	if err != nil {
		return m, err
	}
	defer store.Close()
	if err = store.BindIndex(ctx, index.Identity()); err != nil {
		return m, err
	}
	service := personalmemory.New(store, v.work, index)
	oldValue, newValue := "old_goal_"+suffix, "new_goal_"+suffix
	old, err := store.Mutate(ctx, rand.Text(), store.UserScope(), "goal", oldValue, "acceptance", 0, false)
	if err != nil {
		return m, err
	}
	if ok, e := service.ProcessOne(ctx); e != nil || !ok {
		return m, errors.New("initial memory synchronization failed")
	}
	current, err := store.Mutate(ctx, rand.Text(), store.UserScope(), "goal", newValue, "acceptance", old.Version, false)
	if err != nil {
		return m, err
	}
	if ok, e := service.ProcessOne(ctx); e != nil || !ok {
		return m, errors.New("updated memory synchronization failed")
	}
	if err = index.Sync(ctx, old); err != nil {
		return m, err
	} // Reproduce a late obsolete vector writer.
	ids, err := index.Search(ctx, []string{store.UserScope(), store.ProjectScope(v.work)}, "goal", 20)
	if err != nil || !slices.Contains(ids, old.VectorID) || !slices.Contains(ids, current.VectorID) {
		return m, errors.New("real vector search failed")
	}
	recalled, err := service.Recall(ctx, "goal")
	if err != nil || !strings.Contains(recalled, newValue) || strings.Contains(recalled, oldValue) {
		return m, errors.New("stale memory survived authoritative version filtering")
	}
	if _, err = store.Mutate(ctx, rand.Text(), store.UserScope(), "goal", "", "acceptance", current.Version, true); err != nil {
		return m, err
	}
	if ok, e := service.ProcessOne(ctx); e != nil || !ok {
		return m, errors.New("deleted memory synchronization failed")
	}
	if err = index.Sync(ctx, old); err != nil {
		return m, err
	} // Reintroduce an obsolete vector after deletion.
	ids, err = index.Search(ctx, []string{store.UserScope(), store.ProjectScope(v.work)}, "goal", 20)
	if err != nil || !slices.Contains(ids, old.VectorID) {
		return m, errors.New("deleted stale vector was not present for filter verification")
	}
	recalled, err = service.Recall(ctx, "goal")
	if err != nil || strings.Contains(recalled, oldValue) || strings.Contains(recalled, newValue) {
		return m, errors.New("deleted memory was recalled")
	}
	m["stale_and_deleted_filtered"] = true
	modified := *cfg
	modified.RAG.Collection = ragName
	modified.RAG.KnowledgeBase = "harness-acceptance"
	engine, vectorStore, err := rag.FromConfig(&modified)
	if err != nil {
		return m, err
	}
	defer engine.Close()
	if err = vectorStore.Init(ctx); err != nil {
		return m, err
	}
	doc, err := engine.Import(ctx, modified.RAG.KnowledgeBase, "acceptance.md", strings.NewReader("Acceptance fixture: Go context cancellation allows workers to observe the Done channel and stop work. Call cancel to release resources."))
	if err != nil {
		return m, err
	}
	hits, err := engine.Search(ctx, modified.RAG.KnowledgeBase, "Go context cancellation", 3)
	if err != nil || len(hits) == 0 {
		return m, errors.New("real RAG import/search failed")
	}
	m["rag_chunks"] = len(doc.Chunks)
	m["rag_hits"] = len(hits)
	return m, nil
}

// collections records the exact resources allocated by one scenario. Prefixes
// alone are insufficient: another acceptance run may own a similarly named index.
type collections struct{ suffix, memory, rag string }

func newCollections() collections {
	suffix := strings.ToLower(rand.Text())
	return collections{suffix: suffix, memory: "harness_verify_memory_" + suffix, rag: "harness_verify_rag_" + suffix}
}

func (c collections) cleanup(ctx context.Context, cfg *config.Config) error {
	return errors.Join(c.drop(ctx, cfg, c.memory), c.drop(ctx, cfg, c.rag))
}

func (c collections) drop(ctx context.Context, cfg *config.Config, name string) error {
	if !strings.HasPrefix(name, "harness_verify_") || (name != c.memory && name != c.rag) {
		return errors.New("refusing cleanup of a collection not owned by this run")
	}
	raw, _ := json.Marshal(map[string]string{"collectionName": name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.RAG.MilvusURL, "/")+"/v2/vectordb/collections/drop", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.RAG.MilvusToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.RAG.MilvusToken)
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		return errors.New("acceptance collection cleanup unavailable")
	}
	defer response.Body.Close()
	var body struct{ Code *int }
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body) != nil || response.StatusCode != 200 || body.Code == nil || *body.Code != 0 {
		return errors.New("acceptance collection cleanup failed")
	}
	return nil
}

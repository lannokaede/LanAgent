package rag

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/lannokaede/LanAgent/internal/config"
	"github.com/tmc/langchaingo/embeddings"
)

type VectorStore interface {
	Check(context.Context) error
	Write(context.Context, *PreparedDocument, [][]float32) error
	Search(context.Context, string, []float32, int) ([]Hit, error)
}

// 模块 1：RAG 编排。复用 LangChainGo Embedder 边界，不包含 Agent 循环。
type Engine struct {
	Embedder       embeddings.Embedder
	Store          VectorStore
	closeResources func()
}

// FromConfig creates an Engine that owns both HTTP clients. Close the Engine,
// not only the returned store; the store remains exposed for collection setup.
func FromConfig(cfg *config.Config) (*Engine, *MilvusStore, error) {
	if cfg == nil || !knowledgeBaseName.MatchString(cfg.RAG.KnowledgeBase) {
		return nil, nil, fmt.Errorf("invalid configured knowledge base")
	}
	embedding, err := NewEmbeddingClient(cfg.Embedding.BaseURL, cfg.Embedding.Model, cfg.Embedding.APIKey, cfg.Embedding.Dimensions)
	if err != nil {
		return nil, nil, err
	}
	store, err := NewMilvusStore(cfg.RAG.MilvusURL, cfg.RAG.MilvusToken, cfg.RAG.Collection, cfg.Embedding.Dimensions, embedding.Identity())
	if err != nil {
		embedding.Close()
		return nil, nil, err
	}
	engine := &Engine{Embedder: embedding, Store: store, closeResources: sync.OnceFunc(func() {
		store.Close()
		embedding.Close()
	})}
	return engine, store, nil
}

// Close releases clients created by FromConfig and is safe to call repeatedly.
// Engines assembled with injected dependencies leave ownership with the caller.
// Stop users of the Engine before closing it; Close does not abort active calls.
func (e *Engine) Close() {
	if e != nil && e.closeResources != nil {
		e.closeResources()
	}
}

// 模块 2：读取 → 分块 → 检查集合 → 批量向量化 → 写库。任何失败都不能宣称 indexed。
// 使用固定的 512 字符/64 重叠策略；单片同时受 Embedding 字节上限约束。
func (e *Engine) Import(ctx context.Context, kb, filename string, reader io.Reader) (*PreparedDocument, error) {
	preparer, err := NewPreparer(SplitConfig{512, 64})
	if err != nil {
		return nil, err
	}
	doc, err := preparer.Prepare(ctx, kb, filename, reader)
	if err != nil {
		return nil, err
	}
	if len(doc.Chunks) > 2048 {
		return nil, fmt.Errorf("document exceeds 2048 chunk limit")
	}
	if err := e.Index(ctx, doc); err != nil {
		return nil, err
	}
	doc.Status = "indexed"
	return doc, nil
}

// Index 供预先校验后的 HTTP 上传任务复用，不重复读取上传正文。
func (e *Engine) Index(ctx context.Context, doc *PreparedDocument) error {
	if doc == nil || len(doc.Chunks) == 0 || len(doc.Chunks) > 2048 {
		return ErrKnowledgeInput
	}
	if err := e.Store.Check(ctx); err != nil {
		return err
	}
	texts := make([]string, len(doc.Chunks))
	for i, c := range doc.Chunks {
		texts[i] = c.Text
	}
	vectors, err := e.Embedder.EmbedDocuments(ctx, texts)
	if err != nil {
		return err
	}
	if err := e.Store.Write(ctx, doc, vectors); err != nil {
		return err
	}
	return nil
}

// 模块 3：问题与文档使用同一 Embedder。Top-K 只是候选，不能保证支持答案。
func (e *Engine) Search(ctx context.Context, kb, query string, limit int) ([]Hit, error) {
	if err := validateSearch(kb, query, limit); err != nil {
		return nil, err
	}
	if err := e.Store.Check(ctx); err != nil {
		return nil, err
	}
	vector, err := e.Embedder.EmbedQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	return e.Store.Search(ctx, kb, vector, limit)
}

func validateSearch(kb, query string, limit int) error {
	if !knowledgeBaseName.MatchString(kb) || strings.TrimSpace(query) == "" || utf8.RuneCountInString(query) > 1024 || limit < 1 || limit > 5 {
		return fmt.Errorf("query must contain 1..1024 characters; top_k must be 1..5")
	}
	return nil
}

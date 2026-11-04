package rag

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/tmc/langchaingo/embeddings"
	"time"
)

type QueryCache interface {
	Get(context.Context, string) ([]float32, bool)
	Put(context.Context, string, []float32, time.Duration)
	Delete(context.Context, string)
	Hit()
	Invalid()
}
type CachedEmbedder struct {
	Base       embeddings.Embedder
	Cache      QueryCache
	Identity   string
	Dimensions int
	TTL        time.Duration
}

func (c *CachedEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	return c.Base.EmbedDocuments(ctx, texts)
}
func (c *CachedEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", c.Identity, c.Dimensions, text))))
	if v, ok := c.Cache.Get(ctx, key); ok {
		if validateVector(v, c.Dimensions) == nil {
			c.Cache.Hit()
			return v, nil
		}
		c.Cache.Invalid()
		c.Cache.Delete(ctx, key)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v, err := c.Base.EmbedQuery(ctx, text)
	if err != nil {
		return nil, err
	}
	if err = validateVector(v, c.Dimensions); err != nil {
		return nil, err
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	c.Cache.Put(ctx, key, v, ttl)
	return v, nil
}

package rag

import (
	"context"
	"errors"
	"testing"
	"time"
)

type vectorCache struct {
	values       map[string][]float32
	hit, invalid int
}

func (c *vectorCache) Get(_ context.Context, k string) ([]float32, bool) {
	v, ok := c.values[k]
	return v, ok
}
func (c *vectorCache) Put(_ context.Context, k string, v []float32, _ time.Duration) { c.values[k] = v }
func (c *vectorCache) Delete(_ context.Context, k string)                            { delete(c.values, k) }
func (c *vectorCache) Hit()                                                          { c.hit++ }
func (c *vectorCache) Invalid()                                                      { c.invalid++ }

type cacheEmbedder struct {
	calls int
	fail  bool
}

func (e *cacheEmbedder) EmbedDocuments(context.Context, []string) ([][]float32, error) {
	return nil, nil
}
func (e *cacheEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	e.calls++
	if e.fail {
		return nil, errors.New("unavailable")
	}
	return []float32{1, 2}, nil
}
func TestQueryCacheIdentityCorruptionAndFailure(t *testing.T) {
	ctx := context.Background()
	base := &cacheEmbedder{}
	cache := &vectorCache{values: map[string][]float32{}}
	c := &CachedEmbedder{Base: base, Cache: cache, Identity: "model-a", Dimensions: 2}
	c.EmbedQuery(ctx, "中文 query")
	c.EmbedQuery(ctx, "中文 query")
	if base.calls != 1 || cache.hit != 1 {
		t.Fatal("not cached")
	}
	c.Identity = "model-b"
	c.EmbedQuery(ctx, "中文 query")
	if base.calls != 2 {
		t.Fatal("model identity reused")
	}
	for k := range cache.values {
		cache.values[k] = []float32{1}
	}
	c.EmbedQuery(ctx, "中文 query")
	if base.calls != 3 || cache.invalid != 1 {
		t.Fatal("corrupt cache accepted")
	}
	base.fail = true
	if _, err := c.EmbedQuery(ctx, "uncached failure"); err == nil {
		t.Fatal("failure hidden")
	}
	base.fail = false
	c.EmbedQuery(ctx, "uncached failure")
	if base.calls != 5 {
		t.Fatal("error cached")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.EmbedQuery(cancelled, "中文 query"); !errors.Is(err, context.Canceled) {
		t.Fatal("cached hit ignored cancellation")
	}
}

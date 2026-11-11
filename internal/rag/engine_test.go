package rag

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeEmbedder struct {
	calls int
	err   error
}

func (f *fakeEmbedder) EmbedDocuments(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	result := make([][]float32, len(texts))
	for i := range result {
		result[i] = []float32{1, 0}
	}
	return result, nil
}
func (f *fakeEmbedder) EmbedQuery(ctx context.Context, s string) ([]float32, error) {
	v, err := f.EmbedDocuments(ctx, []string{s})
	if err != nil {
		return nil, err
	}
	return v[0], nil
}

type fakeStore struct {
	checkErr, writeErr error
	writes, searches   int
	kb                 string
}

func (f *fakeStore) Check(context.Context) error { return f.checkErr }
func (f *fakeStore) Write(context.Context, *PreparedDocument, [][]float32) error {
	f.writes++
	return f.writeErr
}
func (f *fakeStore) Search(_ context.Context, kb string, _ []float32, _ int) ([]Hit, error) {
	f.searches++
	f.kb = kb
	return []Hit{{ID: "c", Source: "a.md", Text: "原文", Score: 0.8}}, nil
}
func TestEngineFailuresAndSearchIsolation(t *testing.T) {
	embedder := &fakeEmbedder{}
	store := &fakeStore{}
	engine := &Engine{Embedder: embedder, Store: store}
	store.checkErr = errors.New("incompatible model")
	if _, err := engine.Import(context.Background(), "learning", "a.md", strings.NewReader("原文")); err == nil || embedder.calls != 0 {
		t.Fatal("must check before billing")
	}
	store.checkErr = nil
	embedder.err = errors.New("embedding failed")
	if _, err := engine.Import(context.Background(), "learning", "a.md", strings.NewReader("原文")); err == nil || store.writes != 0 {
		t.Fatal("must not write after embedding failure")
	}
	embedder.err = nil
	store.writeErr = errors.New("write failed")
	if doc, err := engine.Import(context.Background(), "learning", "a.md", strings.NewReader("原文")); err == nil || doc != nil {
		t.Fatal("must not report partial import success")
	}
	store.writeErr = nil
	doc, err := engine.Import(context.Background(), "learning", "a.md", strings.NewReader("原文"))
	if err != nil || doc.Status != "indexed" {
		t.Fatalf("import failed: %v", err)
	}
	hits, err := engine.Search(context.Background(), "learning", "问题", 3)
	if err != nil || len(hits) != 1 || store.kb != "learning" {
		t.Fatal("search failed")
	}
	for _, query := range []string{"", strings.Repeat("字", 1025)} {
		if _, err := engine.Search(context.Background(), "learning", query, 3); err == nil {
			t.Fatal("accepted bad query")
		}
	}
}

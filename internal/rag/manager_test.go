package rag

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type catalogStub struct {
	fakeStore
	docs    []DocumentInfo
	deleted string
}

func (s *catalogStub) ListDocuments(context.Context, string) ([]DocumentInfo, error) {
	return s.docs, nil
}
func (s *catalogStub) DeleteDocument(_ context.Context, _ string, id string) error {
	s.deleted = id
	return nil
}

type blockingIndexer struct {
	started chan struct{}
	release chan struct{}
	fail    bool
}

func (i *blockingIndexer) Index(ctx context.Context, doc *PreparedDocument) error {
	close(i.started)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-i.release:
	}
	if i.fail {
		return errors.New("secret upstream details")
	}
	return nil
}
func (i *blockingIndexer) Search(context.Context, string, string, int) ([]Hit, error) {
	return []Hit{}, nil
}
func TestManagerImportStatusAndDeletion(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failure], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			indexer := &blockingIndexer{make(chan struct{}), make(chan struct{}), failure}
			catalog := &catalogStub{}
			manager := NewManager(ctx, "learning", indexer, catalog)
			p, _ := NewPreparer(SplitConfig{512, 64})
			doc, _ := p.Prepare(ctx, "learning", "a.md", strings.NewReader("资料"))
			info, err := manager.Submit(doc)
			if err != nil || info.Status != "queued" {
				t.Fatalf("submit: %v", err)
			}
			<-indexer.started
			if _, err := manager.Submit(doc); !errors.Is(err, ErrKnowledgeBusy) {
				t.Fatal("duplicate job must be rejected")
			}
			if err := manager.Delete(ctx, info.ID); !errors.Is(err, ErrKnowledgeBusy) {
				t.Fatal("must not delete active job")
			}
			close(indexer.release)
			deadline := time.After(2 * time.Second)
			for {
				current, err := manager.Get(ctx, info.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.Status == "indexed" || current.Status == "failed" {
					if failure && (current.Status != "failed" || strings.Contains(current.Error, "secret")) {
						t.Fatal("unsafe failure state")
					}
					if !failure && current.ChunkCount != 1 {
						t.Fatal("missing chunk count")
					}
					break
				}
				select {
				case <-deadline:
					t.Fatal("job did not finish")
				case <-time.After(time.Millisecond):
				}
			}
			if err := manager.Delete(ctx, info.ID); err != nil || catalog.deleted != info.ID {
				t.Fatalf("delete: %v", err)
			}
			if _, err := manager.Get(ctx, info.ID); !errors.Is(err, ErrKnowledgeNotFound) {
				t.Fatal("deleted document still visible")
			}
		})
	}
}

package rag

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lannokaede/LanAgent/internal/config"
)

type borrowedEmbedder struct {
	fakeEmbedder
	closed bool
}

func (e *borrowedEmbedder) Close() { e.closed = true }

type borrowedStore struct {
	fakeStore
	closed bool
}

func (s *borrowedStore) Close() { s.closed = true }

func TestConfiguredEngineClosesBothClientConnectionPools(t *testing.T) {
	// Separate servers make an unclosed Embedding or Milvus pool visible even if
	// the other client correctly releases its own idle connection.
	newServer := func(handler http.Handler) (*httptest.Server, <-chan struct{}) {
		closed := make(chan struct{}, 1)
		server := httptest.NewUnstartedServer(handler)
		server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				select {
				case closed <- struct{}{}:
				default:
				}
			}
		}
		server.Start()
		t.Cleanup(server.Close)
		return server, closed
	}
	embedding, embeddingClosed := newServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"index":0,"embedding":[1,0]}]}`)
	}))
	identity := fmt.Sprintf("demo-rag-v1:%s:text-embedding-v4:2:raw-text", embedding.URL)
	milvus, milvusClosed := newServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"code":0,"data":{"description":%q,"fields":[{"name":"vector","type":"FloatVector","params":[{"key":"dim","value":"2"}]}]}}`, identity)
	}))
	cfg := &config.Config{
		Embedding: config.Embedding{BaseURL: embedding.URL, Model: "text-embedding-v4", APIKey: "fixture", Dimensions: 2},
		RAG:       config.RAG{MilvusURL: milvus.URL, Collection: "fixture", KnowledgeBase: "learning"},
	}
	engine, store, err := FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if _, err := engine.Embedder.EmbedQuery(context.Background(), "question"); err != nil {
		t.Fatal(err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	engine.Close()
	engine.Close()
	for name, closed := range map[string]<-chan struct{}{"embedding": embeddingClosed, "Milvus": milvusClosed} {
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
			t.Fatalf("Engine.Close did not release %s idle connections", name)
		}
	}
}

func TestFromConfigFailuresReturnNoPartialEngine(t *testing.T) {
	valid := config.Config{
		Embedding: config.Embedding{BaseURL: "http://localhost:1", Model: "text-embedding-v4", APIKey: "fixture", Dimensions: 2},
		RAG:       config.RAG{MilvusURL: "http://localhost:2", Collection: "fixture", KnowledgeBase: "learning"},
	}
	for _, tc := range []struct {
		name   string
		change func(*config.Config)
	}{
		{"knowledge base", func(c *config.Config) { c.RAG.KnowledgeBase = "invalid namespace" }},
		{"embedding", func(c *config.Config) { c.Embedding.APIKey = "" }},
		{"store after embedding construction", func(c *config.Config) { c.RAG.MilvusURL = "invalid-url" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.change(&cfg)
			engine, store, err := FromConfig(&cfg)
			if err == nil || engine != nil || store != nil {
				t.Fatalf("partial ownership escaped failed construction: engine=%v store=%v err=%v", engine, store, err)
			}
		})
	}
	if engine, store, err := FromConfig(nil); err == nil || engine != nil || store != nil {
		t.Fatal("nil configuration must fail without resources")
	}
	// Caller-supplied dependencies do not acquire ownership by being attached to
	// an Engine. Closing this Engine must leave both adapters usable.
	embedder, store := &borrowedEmbedder{}, &borrowedStore{}
	engine := &Engine{Embedder: embedder, Store: store}
	engine.Close()
	if embedder.closed || store.closed {
		t.Fatal("Engine.Close took ownership of caller-supplied dependencies")
	}
	if _, err := engine.Search(context.Background(), "learning", "question", 1); err != nil {
		t.Fatal(err)
	}
	var absent *Engine
	absent.Close()
}

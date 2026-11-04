package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestCorpusCommandsRejectUnsafeMaintenanceBeforeNetwork(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	cfg := t.TempDir()
	text := fmt.Sprintf("llm:\n  api_key: test\nembedding:\n  base_url: %s\n  model: text-embedding-v4\n  dimensions: 3\n  api_key: test\nrag:\n  milvus_url: %s\n  collection: interview_test\n  knowledge_base: go-backend-interview\n", server.URL, server.URL)
	if err := os.WriteFile(filepath.Join(cfg, "config.yaml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runCorpus(context.Background(), []string{"clear", "-config", cfg, "-confirm-kb", "other-project"}, io.Discard); err == nil {
		t.Fatal("accepted another namespace")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":1,"documents":[{"id":"test","topic":"go","url":"https://go.dev/doc/faq","file":"test.md","sha256":"incorrect"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "test.md"), []byte("test content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runCorpus(context.Background(), []string{"import-corpus", "-config", cfg, "-dir", dir}, io.Discard); err == nil {
		t.Fatal("accepted corrupt corpus")
	}
	if requests.Load() != 0 {
		t.Fatalf("sent %d requests before validating maintenance input", requests.Load())
	}
}

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lannokaede/LanAgent/internal/config"
	"github.com/lannokaede/LanAgent/internal/corpus"
	"github.com/lannokaede/LanAgent/internal/rag"
)

func runCorpus(ctx context.Context, args []string, out io.Writer) error {
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	f.SetOutput(out)
	dir := f.String("dir", "", "corpus directory (new directory for crawl)")
	configDir := f.String("config", "configs/interview", "configuration directory")
	confirm := f.String("confirm-kb", "", "clear only: exact configured namespace to delete")
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if args[0] == "crawl" {
		if *dir == "" {
			return fmt.Errorf("crawl requires -dir")
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		m, err := corpus.NewCrawler().Crawl(ctx, corpus.Sources(), *dir)
		if m != nil {
			fmt.Fprintf(out, "Downloaded %d/%d sources; manifest: %s\n", countSuccess(m), len(m.Documents), filepath.Join(*dir, "manifest.json"))
		}
		return err
	}
	cfg, err := config.Load(*configDir)
	if err != nil {
		return err
	}
	engine, store, err := rag.FromConfig(cfg)
	if err != nil {
		return err
	}
	defer engine.Close()
	if args[0] == "clear" {
		if *confirm == "" || *confirm != cfg.RAG.KnowledgeBase {
			return fmt.Errorf("-confirm-kb must equal the configured namespace")
		}
		if err := store.Check(ctx); err != nil {
			return err
		}
		docs, err := store.ListDocuments(ctx, *confirm)
		if err != nil {
			return err
		}
		for _, doc := range docs {
			if err := store.DeleteDocument(ctx, *confirm, doc.ID); err != nil {
				return err
			}
		}
		return json.NewEncoder(out).Encode(map[string]any{"deleted_documents": len(docs), "collection": cfg.RAG.Collection, "knowledge_base": *confirm})
	}
	if *dir == "" {
		return fmt.Errorf("import-corpus requires -dir")
	}
	root, err := os.OpenRoot(*dir)
	if err != nil {
		return err
	}
	defer root.Close()
	data, err := root.ReadFile("manifest.json")
	if err != nil {
		return err
	}
	var m corpus.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	if m.Version != 1 || len(m.Documents) == 0 || len(m.Documents) > 30 || countSuccess(&m) != len(m.Documents) {
		return fmt.Errorf("incomplete or invalid corpus manifest; refusing import")
	}
	// Validate the complete corpus before paying for any embeddings.
	prepared := make([]*rag.PreparedDocument, 0, len(m.Documents))
	preparer, _ := rag.NewPreparer(rag.SplitConfig{ChunkSize: 512, ChunkOverlap: 64})
	for _, doc := range m.Documents {
		if doc.File != filepath.Base(doc.File) || !strings.HasSuffix(doc.File, ".md") {
			return fmt.Errorf("invalid corpus filename")
		}
		content, err := root.ReadFile(doc.File)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		if hex.EncodeToString(sum[:]) != doc.SHA256 {
			return fmt.Errorf("corpus checksum mismatch: %s", doc.File)
		}
		p, err := preparer.Prepare(ctx, cfg.RAG.KnowledgeBase, doc.File, strings.NewReader(string(content)))
		if err != nil {
			return err
		}
		for i := range p.Chunks {
			p.Chunks[i].Source = doc.URL
			p.Chunks[i].Text = "Topic: " + doc.Topic + " | " + doc.Title + "\nSource: " + doc.URL + "\n\n" + p.Chunks[i].Text
		}
		prepared = append(prepared, p)
	}
	if err := store.Init(ctx); err != nil {
		return err
	}
	existing, err := store.ListDocuments(ctx, cfg.RAG.KnowledgeBase)
	if err != nil {
		return err
	}
	byID := map[string]rag.DocumentInfo{}
	for _, doc := range existing {
		byID[doc.ID] = doc
	}
	chunks := 0
	for i, doc := range prepared {
		if old, ok := byID[doc.Document.ID]; ok && old.Status == "indexed" && old.RevisionID == doc.Document.RevisionID && old.ChunkCount == len(doc.Chunks) {
			chunks += len(doc.Chunks)
			fmt.Fprintf(out, "unchanged %s (%d chunks)\n", m.Documents[i].File, len(doc.Chunks))
			continue
		}
		if err := engine.Index(ctx, doc); err != nil {
			return fmt.Errorf("import %s: %w", m.Documents[i].File, err)
		}
		chunks += len(doc.Chunks)
		fmt.Fprintf(out, "indexed %s (%d chunks)\n", m.Documents[i].File, len(doc.Chunks))
	}
	return json.NewEncoder(out).Encode(map[string]any{"documents": len(prepared), "chunks": chunks, "collection": cfg.RAG.Collection, "knowledge_base": cfg.RAG.KnowledgeBase})
}
func countSuccess(m *corpus.Manifest) int {
	count := 0
	for _, d := range m.Documents {
		if d.Error == "" {
			count++
		}
	}
	return count
}

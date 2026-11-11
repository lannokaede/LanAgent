package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/lannokaede/LanAgent/internal/corpus"
	"github.com/lannokaede/LanAgent/internal/rag"
)

func verifyCorpus(ctx context.Context, dir string) (map[string]any, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	raw, err := root.ReadFile("manifest.json")
	if err != nil {
		return nil, err
	}
	var manifest corpus.Manifest
	if json.Unmarshal(raw, &manifest) != nil || manifest.Version != 1 || len(manifest.Documents) == 0 {
		return nil, errors.New("invalid corpus snapshot manifest")
	}
	preparer, _ := rag.NewPreparer(rag.SplitConfig{ChunkSize: 512, ChunkOverlap: 64})
	chunks := 0
	sources := []map[string]any{}
	for _, doc := range manifest.Documents {
		if doc.Error != "" || doc.File != filepath.Base(doc.File) {
			return nil, errors.New("snapshot has missing source or unsafe filename")
		}
		data, err := root.ReadFile(doc.File)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != doc.SHA256 {
			return nil, errors.New("corpus source checksum mismatch")
		}
		prepared, err := preparer.Prepare(ctx, "go-backend-interview", doc.File, strings.NewReader(string(data)))
		if err != nil {
			return nil, err
		}
		chunks += len(prepared.Chunks)
		sources = append(sources, map[string]any{"file": doc.File, "source": doc.URL, "sha256": doc.SHA256, "chunks": len(prepared.Chunks)})
	}
	hash := sha256.Sum256(raw)
	return map[string]any{"documents": len(manifest.Documents), "source_documents": sources, "chunks": chunks, "chunk_size_runes": 512, "chunk_overlap_runes": 64, "manifest_sha256": hex.EncodeToString(hash[:]), "scope": "local checksummed snapshot; not a live vector index count"}, nil
}

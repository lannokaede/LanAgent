package application

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/lannokaede/LanAgent/internal/config"
	"github.com/lannokaede/LanAgent/internal/personalmemory"
	"github.com/lannokaede/LanAgent/internal/rag"
	"github.com/lannokaede/LanAgent/internal/workspace"
)

// MemoryCommand is the local administration path. It deliberately does not
// load model configuration, external tools, telemetry, or a conversation.
func MemoryCommand(ctx context.Context, o Options, input string, out io.Writer) error {
	if !o.Memory {
		return errors.New("personal memory disabled; enable -memory")
	}
	if !personalmemory.IsCommand(input) {
		return errors.New("not a personal memory command")
	}
	if err := resolveStatePaths(&o); err != nil {
		return err
	}
	w, err := workspace.Open(o.Workspace)
	if err != nil {
		return err
	}
	defer w.Close()
	store, err := personalmemory.Open(ctx, o.MemoryDB)
	if err != nil {
		return err
	}
	defer store.Close()
	_, err = personalmemory.New(store, w.Path(), nil).Command(ctx, input, "cli:explicit-user", out)
	return err
}

func (r *Runtime) openMemory(ctx context.Context, cfg *config.Config, o Options) error {
	if !o.Memory {
		return nil
	}
	store, err := personalmemory.Open(ctx, o.MemoryDB)
	if err != nil {
		return err
	}
	r.resources.add(store.Close)
	var index personalmemory.Index
	if o.MemoryVectors {
		emb, err := rag.NewEmbeddingClient(cfg.Embedding.BaseURL, cfg.Embedding.Model, cfg.Embedding.APIKey, cfg.Embedding.Dimensions)
		if err != nil {
			return err
		}
		milvus, err := personalmemory.NewMilvus(cfg.RAG.MilvusURL, cfg.RAG.MilvusToken, o.MemoryCollection, emb, cfg.Embedding.Dimensions)
		if err != nil {
			emb.Close()
			return err
		}
		r.resources.release(milvus.Close) // Milvus owns its embedding client.
		check, cancel := context.WithTimeout(ctx, 30*time.Second)
		if o.MemoryInit {
			err = milvus.Init(check)
		} else {
			err = milvus.Check(check)
		}
		cancel()
		if err != nil {
			return err
		}
		if err = store.BindIndex(ctx, milvus.Identity()); err != nil {
			return err
		}
		index = milvus
	}
	r.memory = personalmemory.New(store, r.workspace, index)
	r.runner.WithRecall(r.memory.Recall)
	if index != nil {
		workerCtx, stop := context.WithCancel(ctx)
		var once sync.Once
		var workers sync.WaitGroup
		// Local administration and receipt reconciliation do not start a worker.
		r.startSync = func() {
			once.Do(func() {
				workers.Add(1)
				go func() { defer workers.Done(); r.memory.Run(workerCtx, nil) }()
			})
		}
		r.resources.release(func() { stop(); workers.Wait() })
	}
	return nil
}

// knowledge 是独立知识库入口：prepare 离线预处理；init/import/search 接入向量库。
// import/search 会调用云端 Embedding，均不启动聊天服务或调用聊天模型。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/lannokaede/LanAgent/internal/config"
	"github.com/lannokaede/LanAgent/internal/rag"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) > 0 && (args[0] == "crawl" || args[0] == "import-corpus" || args[0] == "clear") {
		return runCorpus(ctx, args, stdout)
	}
	if len(args) > 0 && (args[0] == "init" || args[0] == "import" || args[0] == "search") {
		return runIndexed(ctx, args, stdout)
	}
	// 模块 1：命令和参数。字符长度不是 Token 数；索引前还需模型校验。
	if len(args) == 0 || args[0] != "prepare" {
		return fmt.Errorf("usage: knowledge prepare -file document.md [-kb learning] [-out prepared.json]")
	}
	flags := flag.NewFlagSet("prepare", flag.ContinueOnError)
	flags.SetOutput(stdout)
	filename := flags.String("file", "", "UTF-8 TXT/Markdown input file")
	kb := flags.String("kb", "learning", "knowledge base ID")
	size := flags.Int("chunk-size", 512, "maximum Unicode characters per chunk (not tokens)")
	overlap := flags.Int("overlap", 64, "overlap character budget")
	output := flags.String("out", "", "new output JSON file; default stdout")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *filename == "" || flags.NArg() != 0 {
		return fmt.Errorf("-file is required; positional arguments are not supported")
	}
	preparer, err := rag.NewPreparer(rag.SplitConfig{ChunkSize: *size, ChunkOverlap: *overlap})
	if err != nil {
		return err
	}
	// 模块 2：只读取普通文件；体积、编码与内容验证由共享 Preparer 完成。
	input, err := os.Open(*filename)
	if err != nil {
		return fmt.Errorf("open document: %w", err)
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("input must be a regular file")
	}
	prepared, err := preparer.Prepare(ctx, *kb, filepath.Base(*filename), input)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(prepared, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := ctx.Err(); err != nil {
		return err
	}
	// 模块 3：输出可检查的预处理结果。已有文件一律拒绝覆盖，包括源文件。
	// TODO：下一阶段按模型 Token 限制重新校验/分块 → Embedding → Milvus。
	if *output == "" {
		_, err = stdout.Write(data)
		return err
	}
	file, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create output (must not exist): %w", err)
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}

// 正式索引入口：init 建集合；import 读取原文并生成向量；search 仅检索不调用聊天模型。
func runIndexed(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(stdout)
	configDir := flags.String("config", "", "configuration directory")
	filename := flags.String("file", "", "UTF-8 TXT/Markdown input")
	query := flags.String("query", "", "search question")
	kb := flags.String("kb", "", "knowledge base namespace; default configured value")
	limit := flags.Int("top-k", 3, "candidate count (1..5)")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if args[0] == "import" && *filename == "" {
		return fmt.Errorf("import requires -file")
	}
	if args[0] == "search" && *query == "" {
		return fmt.Errorf("search requires -query")
	}
	cfg, err := config.Load(*configDir)
	if err != nil {
		return err
	}
	if *kb != "" {
		cfg.RAG.KnowledgeBase = *kb
	}
	engine, store, err := rag.FromConfig(cfg)
	if err != nil {
		return err
	}
	defer engine.Close()
	duration := 60 * time.Second
	if args[0] == "import" {
		duration = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	var result any
	switch args[0] {
	case "init":
		if err := store.Init(ctx); err != nil {
			return err
		}
		result = map[string]any{"status": "ready", "collection": cfg.RAG.Collection, "model": cfg.Embedding.Model, "dimensions": cfg.Embedding.Dimensions}
	case "import":
		file, err := os.Open(*filename)
		if err != nil {
			return err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("input must be a regular file")
		}
		doc, err := engine.Import(ctx, cfg.RAG.KnowledgeBase, filepath.Base(*filename), file)
		if err != nil {
			return err
		}
		result = map[string]any{"status": doc.Status, "document": doc.Document, "chunks": len(doc.Chunks), "collection": cfg.RAG.Collection}
	case "search":
		hits, err := engine.Search(ctx, cfg.RAG.KnowledgeBase, *query, *limit)
		if err != nil {
			return err
		}
		result = map[string]any{"matches": hits}
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

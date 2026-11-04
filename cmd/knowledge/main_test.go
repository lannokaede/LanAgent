package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/lannokaede/LanAgent/internal/rag"
)

func TestPrepareCommand(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "中文.md")
	if err := os.WriteFile(input, []byte("工具注册表保存可执行工具。模型建议调用，应用执行工具。"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"prepare", "-file", input, "-chunk-size", "12", "-overlap", "2"}
	var out bytes.Buffer
	if err := run(context.Background(), args, &out); err != nil {
		t.Fatal(err)
	}
	var prepared rag.PreparedDocument
	if err := json.Unmarshal(out.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.Status != "prepared" || prepared.LengthUnit != "rune" || len(prepared.Chunks) < 2 || prepared.Document.Filename != "中文.md" {
		t.Fatalf("unexpected prepared document: %+v", prepared)
	}
	output := filepath.Join(dir, "prepared.json")
	if err := run(context.Background(), append(args, "-out", output), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(data, out.Bytes()) {
		t.Fatalf("file output differs: %v", err)
	}
	if err := run(context.Background(), append(args, "-out", input), &bytes.Buffer{}); err == nil {
		t.Fatal("must refuse overwriting the source")
	}
	if err := run(context.Background(), append(args, "-out", output), &bytes.Buffer{}); err == nil {
		t.Fatal("must refuse overwriting output")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, args, &bytes.Buffer{}); err == nil {
		t.Fatal("must respect cancellation")
	}
	if err := run(context.Background(), []string{"prepare", "-file", dir}, &bytes.Buffer{}); err == nil {
		t.Fatal("must reject directory")
	}
}

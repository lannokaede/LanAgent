package workspace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceReadListAndRejectEscapes(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	r, err := w.Registry()
	if err != nil {
		t.Fatal(err)
	}
	read, _ := r.Lookup("read_file")
	list, _ := r.Lookup("list_files")
	result, err := read.Execute(context.Background(), `{"path":"note.md"}`)
	if err != nil || !strings.Contains(result, "hello") {
		t.Fatalf("read: %s %v", result, err)
	}
	result, err = list.Execute(context.Background(), `{"path":"."}`)
	if err != nil || !strings.Contains(result, "note.md") {
		t.Fatalf("list: %s %v", result, err)
	}
	for _, raw := range []string{`{"path":"../secret"}`, `{"path":"/etc/passwd"}`, `{"path":"escape/secret"}`, `{"path":"note.md","extra":1}`, `{"path":"note.md"} {}`, `{"path":"note.md","offset":-1}`, `{"path":"."}`} {
		if _, err := read.Execute(context.Background(), raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := read.Execute(ctx, `{"path":"note.md"}`); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestReadFilePaginationPreservesUTF8(t *testing.T) {
	dir := t.TempDir()
	content := strings.Repeat("中", maxReadBytes)
	if err := os.WriteFile(filepath.Join(dir, "large.txt"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	r, _ := w.Registry()
	read, _ := r.Lookup("read_file")
	var assembled strings.Builder
	offset := int64(0)
	for {
		raw, _ := json.Marshal(map[string]any{"path": "large.txt", "offset": offset})
		result, err := read.Execute(context.Background(), string(raw))
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Content   string `json:"content"`
			Next      int64  `json:"next_offset"`
			Truncated bool   `json:"truncated"`
		}
		if err := json.Unmarshal([]byte(result), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Content) > maxReadBytes {
			t.Fatal("unbounded output")
		}
		assembled.WriteString(out.Content)
		if !out.Truncated {
			break
		}
		if out.Next <= offset {
			t.Fatal("pagination stalled")
		}
		offset = out.Next
	}
	if assembled.String() != content {
		t.Fatal("pagination lost or duplicated bytes")
	}
}

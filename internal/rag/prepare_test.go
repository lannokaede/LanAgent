package rag

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrepareNormalizesAndKeepsSources(t *testing.T) {
	p, err := NewPreparer(SplitConfig{ChunkSize: 80, ChunkOverlap: 10})
	if err != nil {
		t.Fatal(err)
	}
	text := "# Go 学习\n\n使用 channel 传递消息。\n\n使用 context 取消操作。"
	a, err := p.Prepare(context.Background(), "go_docs", "guide.md", strings.NewReader("\ufeff"+strings.ReplaceAll(text, "\n", "\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Prepare(context.Background(), "go_docs", "guide.md", strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same normalized input should yield stable IDs and chunks")
	}
	if len(a.Chunks) == 0 || a.Document.ContentSHA256 == "" || a.Document.ID == "" || a.Document.RevisionID == "" || a.Status != "prepared" {
		t.Fatalf("missing document metadata: %+v", a)
	}
	for i, c := range a.Chunks {
		if c.Index != i || c.DocumentID != a.Document.ID || c.RevisionID != a.Document.RevisionID || c.KnowledgeBaseID != "go_docs" || c.Source != "guide.md" || c.ID == "" || !utf8.ValidString(c.Text) {
			t.Fatalf("lost provenance: %+v", c)
		}
	}
	other, err := p.Prepare(context.Background(), "other_docs", "guide.md", strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if a.Document.ID == other.Document.ID || a.Chunks[0].ID == other.Chunks[0].ID {
		t.Fatal("IDs must include knowledge base scope")
	}
	changed, err := p.Prepare(context.Background(), "go_docs", "guide.md", strings.NewReader(text+"新的内容"))
	if err != nil {
		t.Fatal(err)
	}
	if changed.Document.ID != a.Document.ID || changed.Document.RevisionID == a.Document.RevisionID || changed.Chunks[0].ID == a.Chunks[0].ID {
		t.Fatal("content changes must produce a new revision and chunk IDs")
	}
}

func TestPrepareChineseOverlap(t *testing.T) {
	p, err := NewPreparer(SplitConfig{ChunkSize: 8, ChunkOverlap: 2})
	if err != nil {
		t.Fatal(err)
	}
	input := "一二三四五六七八九十甲乙丙丁戊己庚辛壬癸"
	result, err := p.Prepare(context.Background(), "test", "chinese.txt", strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Chunks) < 2 {
		t.Fatal("long input was not split")
	}
	var reconstructed strings.Builder
	for i, c := range result.Chunks {
		runes := []rune(c.Text)
		if len(runes) > 8 || !utf8.ValidString(c.Text) {
			t.Fatalf("invalid Chinese chunk: %+v", c)
		}
		if i == 0 {
			reconstructed.WriteString(c.Text)
			continue
		}
		previous := []rune(result.Chunks[i-1].Text)
		if string(previous[len(previous)-2:]) != string(runes[:2]) {
			t.Fatal("configured overlap was lost")
		}
		reconstructed.WriteString(string(runes[2:]))
	}
	if reconstructed.String() != input {
		t.Fatal("chunking lost original Chinese text")
	}
}

func TestPrepareRejectsInvalidInput(t *testing.T) {
	p, _ := NewPreparer(SplitConfig{ChunkSize: 80, ChunkOverlap: 10})
	for _, tc := range []struct{ name, kb, file, text string }{
		{"empty", "test", "a.txt", " \n\t"},
		{"binary", "test", "a.txt", "hello\x00world"},
		{"invalid utf8", "test", "a.txt", string([]byte{0xff, 0xfe})},
		{"unsupported", "test", "a.pdf", "hello"},
		{"invalid kb", "../test", "a.txt", "hello"},
		{"too large", "test", "a.txt", strings.Repeat("a", MaxDocumentBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.Prepare(context.Background(), tc.kb, tc.file, strings.NewReader(tc.text)); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	if _, err := p.Prepare(context.Background(), "test", "a.txt", nil); err == nil {
		t.Fatal("nil reader accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Prepare(ctx, "test", "a.txt", strings.NewReader("hello")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel lost: %v", err)
	}
}

func TestPreparerConfigAndRevision(t *testing.T) {
	for _, c := range []SplitConfig{{0, 0}, {-1, 0}, {8, -1}, {8, 8}, {8, 9}, {MaxChunkRunes + 1, 0}} {
		if _, err := NewPreparer(c); err == nil {
			t.Fatalf("invalid config accepted: %+v", c)
		}
	}
	a, _ := NewPreparer(SplitConfig{8, 2})
	b, _ := NewPreparer(SplitConfig{10, 2})
	x, _ := a.Prepare(context.Background(), "test", "a.txt", strings.NewReader("abcdefghijklmnop"))
	y, _ := b.Prepare(context.Background(), "test", "a.txt", strings.NewReader("abcdefghijklmnop"))
	if x.Document.ID != y.Document.ID || x.Document.RevisionID == y.Document.RevisionID {
		t.Fatal("split configuration must be part of revision identity")
	}
}

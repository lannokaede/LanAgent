package tool

import (
	"context"
	"strings"
	"testing"

	"github.com/lannokaede/LanAgent/internal/rag"
)

type recordingSearch struct {
	calls int
	kb    string
	limit int
}

func (s *recordingSearch) Search(_ context.Context, kb, query string, limit int) ([]rag.Hit, error) {
	s.calls++
	s.kb = kb
	s.limit = limit
	return []rag.Hit{{Source: "a.md", Index: 2, Text: "事实"}}, nil
}
func TestKnowledgeToolArgumentsAndSources(t *testing.T) {
	s := &recordingSearch{}
	tool := KnowledgeTool{s, "learning"}
	result, err := tool.Execute(context.Background(), `{"query":"问题"}`)
	if err != nil || s.kb != "learning" || s.limit != 3 || !strings.Contains(result, "a.md") || !strings.Contains(result, "chunk_index") {
		t.Fatalf("bad knowledge result: %v %s", err, result)
	}
	for _, args := range []string{`{"query":"x","kb":"other"}`, `{"query":"x"} {}`, `{"query":"x","top_k":1.5}`, `null`, `{"query":""}`, `{"query":"x","top_k":0}`, `{"query":"x","top_k":6}`} {
		if _, err := tool.Execute(context.Background(), args); err == nil {
			t.Fatalf("accepted %s", args)
		}
	}
	if s.calls != 1 {
		t.Fatal("bad arguments reached retriever")
	}
}

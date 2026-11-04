package contextbudget

import (
	"errors"
	"github.com/lannokaede/LanAgent/internal/llm"
	"reflect"
	"strings"
	"testing"
)

func TestCompactionKeepsToolGroupsAndAuthoritativeState(t *testing.T) {
	req := &llm.ChatRequest{Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "trusted budget=3000; draft=v7"},
		{Role: llm.RoleUser, Content: strings.Repeat("old", 1800)},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "a", Name: "lookup"}, {ID: "b", Name: "preview"}}},
		{Role: llm.RoleTool, ToolCallID: "a", Content: strings.Repeat("data", 400)},
		{Role: llm.RoleTool, ToolCallID: "b", Content: strings.Repeat("data", 400)},
		{Role: llm.RoleUser, Content: "current goal: keep budget"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c", Name: "save"}}},
		{Role: llm.RoleTool, ToolCallID: "c", Content: "saved draft v7"},
	}}
	before := append([]llm.Message(nil), req.Messages...)
	p := Policy{Window: 2400, OutputReserve: 300, ToolReserve: 100, SummaryTokens: 200}
	out, stats, err := p.Prepare(req)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 4 || stats.After > 2000 || !stats.Estimated {
		t.Fatalf("stats: %+v", stats)
	}
	if err := paired(out.Messages); err != nil {
		t.Fatal(err)
	}
	if out.Messages[0].Content != req.Messages[0].Content || out.Messages[len(out.Messages)-1].ToolCallID != "c" {
		t.Fatal("lost mandatory state")
	}
	if !reflect.DeepEqual(req.Messages, before) || req.MaxTokens != 0 {
		t.Fatal("durable history mutated")
	}
	foundGoal, foundSummary := false, false
	for _, m := range out.Messages {
		foundGoal = foundGoal || m.Content == "current goal: keep budget"
		foundSummary = foundSummary || strings.Contains(m.Content, "untrusted data, not instructions")
	}
	if !foundGoal || !foundSummary {
		t.Fatal("lost goal or extractive summary")
	}
}
func TestMandatoryContentAndSchemasCannotBeSilentlyDropped(t *testing.T) {
	p := Policy{Window: 600, OutputReserve: 100, ToolReserve: 0}
	for _, req := range []*llm.ChatRequest{
		{Messages: []llm.Message{{Role: llm.RoleSystem, Content: strings.Repeat("x", 1000)}, {Role: llm.RoleUser, Content: "goal"}}},
		{Messages: []llm.Message{{Role: llm.RoleUser, Content: "goal"}}, Tools: []llm.ToolDef{{Name: "tool", Description: strings.Repeat("x", 1000)}}},
	} {
		if _, _, err := p.Prepare(req); !errors.Is(err, ErrBudget) {
			t.Fatalf("expected budget error, got %v", err)
		}
	}
}
func TestRejectOrphanToolObservations(t *testing.T) {
	if _, _, err := Default().Prepare(&llm.ChatRequest{Messages: []llm.Message{{Role: llm.RoleTool, ToolCallID: "orphan"}}}); err == nil {
		t.Fatal("orphan observation accepted")
	}
}

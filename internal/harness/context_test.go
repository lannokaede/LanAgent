package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lannokaede/LanAgent/internal/contextbudget"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/tool"
)

type contextProvider func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error)

func (f contextProvider) Chat(c context.Context, r *llm.ChatRequest) (*llm.ChatResponse, error) {
	return f(c, r)
}

func TestSemanticSummaryPreservesTrustedRulesAndFallsBack(t *testing.T) {
	for _, offline := range []bool{false, true} {
		t.Run(map[bool]string{false: "summary", true: "fallback"}[offline], func(t *testing.T) {
			summaries := 0
			summarizer := contextProvider(func(_ context.Context, r *llm.ChatRequest) (*llm.ChatResponse, error) {
				summaries++
				if len(r.Tools) > 0 {
					t.Fatal("summary can execute tools")
				}
				if offline {
					return nil, errors.New("offline")
				}
				return &llm.ChatResponse{Content: "Historical decision: use cancellation."}, nil
			})
			provider := contextProvider(func(_ context.Context, r *llm.ChatRequest) (*llm.ChatResponse, error) {
				if r.Messages[0].Content != "trusted constraints" || r.Messages[len(r.Messages)-1].Content != "current goal" {
					t.Fatal("lost pinned context")
				}
				joined := ""
				for _, m := range r.Messages {
					joined += m.Content
					if strings.Contains(m.Content, "Historical decision") && m.Role == llm.RoleSystem {
						t.Fatal("summary promoted to system instructions")
					}
				}
				if !offline && !strings.Contains(joined, "Historical decision") {
					t.Fatal("missing generated summary")
				}
				return &llm.ChatResponse{Content: "answer"}, nil
			})
			runner := NewRunner(provider, tool.NewRegistry(), 1).WithBudget(contextbudget.Policy{Window: 1600, OutputReserve: 200, ToolReserve: 100, SummaryTokens: 160}).WithSummary(summarizer)
			messages := []llm.Message{{Role: llm.RoleSystem, Content: "trusted constraints"}, {Role: llm.RoleUser, Content: strings.Repeat("old history ", 500)}, {Role: llm.RoleAssistant, Content: "old answer"}, {Role: llm.RoleUser, Content: "current goal"}}
			if _, _, e := runner.Run(context.Background(), messages); e != nil {
				t.Fatal(e)
			}
			if summaries != 1 {
				t.Fatal("summary not invoked")
			}
			if len(messages[1].Content) < 5000 {
				t.Fatal("mutated durable history")
			}
		})
	}
}

func TestVerboseTestOutputKeepsVerificationMetadata(t *testing.T) {
	raw := `{"code_hash":"abc123","status":"passed","exit_code":0,"output":"` + strings.Repeat("log ", 5000) + `"}`
	compact := compactObservation(raw)
	if len(compact) > 4096 || !strings.Contains(compact, `"status":"passed"`) || !strings.Contains(compact, `"code_hash":"abc123"`) {
		t.Fatal("lost verification metadata")
	}
}

func TestArtifactHandlesSurviveObservationCompaction(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"artifact_id": "artifact-handle", "next_offset": 2048, "truncated": true, "content": strings.Repeat("page", 3000)})
	var compact map[string]any
	if json.Unmarshal([]byte(compactObservation(string(raw))), &compact) != nil || compact["artifact_id"] != "artifact-handle" || compact["next_offset"] != float64(2048) {
		t.Fatal("artifact continuation lost", compact)
	}
}

func TestSummaryRecordingFailureStopsBeforeMainModel(t *testing.T) {
	sentinel := errors.New("journal unavailable")
	mainCalls := 0
	summary, _ := llm.NewGateway(contextProvider(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
		return &llm.ChatResponse{Content: "history"}, nil
	}), time.Second)
	runner := NewRunner(contextProvider(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
		mainCalls++
		return &llm.ChatResponse{Content: "unexpected"}, nil
	}), tool.NewRegistry(), 2).WithSummary(summary).WithBudget(contextbudget.Policy{Window: 1600, OutputReserve: 200, ToolReserve: 100, SummaryTokens: 160})
	messages := []llm.Message{{Role: llm.RoleSystem, Content: "trusted rules"}, {Role: llm.RoleUser, Content: strings.Repeat("old history ", 500)}, {Role: llm.RoleAssistant, Content: "old answer"}, {Role: llm.RoleUser, Content: "current question"}}
	_, _, err := runner.RunWithEvents(context.Background(), messages, func(e Event) error {
		if e.Type == "model_attempt_completed" {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) || mainCalls != 0 {
		t.Fatal("summary recording failure continued execution", err, mainCalls)
	}
}

package personal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lannokaede/LanAgent/internal/contextbudget"
	"github.com/lannokaede/LanAgent/internal/harness"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/tool"
)

func TestStatisticsSurviveSummaryAndModelFailureResume(t *testing.T) {
	summaries, models := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream   bool
			Messages []llm.Message
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		if strings.Contains(req.Messages[0].Content, "Summarize historical") {
			summaries++
			fmt.Fprint(w, `{"choices":[{"message":{"content":"prior context"}}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`)
			return
		}
		models++
		if models == 1 {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	p, _ := llm.NewOpenAIProvider(srv.URL, "fixture", "private-key")
	defer p.Close()
	g, _ := llm.NewGateway(p, time.Second)
	g.SetRetryPolicy(llm.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond})
	root := t.TempDir()
	work := t.TempDir()
	s, err := Open(root, "statistics", work)
	if err != nil {
		t.Fatal(err)
	}
	s.state.Messages = []llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("old history ", 500)}, {Role: llm.RoleAssistant, Content: "old answer"}}
	runner := harness.NewRunner(g, tool.NewRegistry(), 3).WithDirectReplies().WithSummary(g).WithBudget(contextbudget.Policy{Window: 8000, OutputReserve: 200, ToolReserve: 100, SummaryTokens: 160})
	if _, err = s.Turn(context.Background(), runner, "直接回答：Go channel 的语义", nil); err == nil {
		t.Fatal("expected model outage")
	}
	runID := s.Pending().RunID
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root, "statistics", work)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	response, err := s.Resume(context.Background(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := s.statistics(runID)
	if err != nil {
		t.Fatal(err)
	}
	if summaries != 2 || models != 2 || stats.Attempts != 4 || stats.UnknownUsage != 1 || stats.ReportedUsage.PromptTokens != 40 || response.Usage.PromptTokens != 40 || response.UsageKnown || stats.Purposes["summary"] != 2 {
		t.Fatalf("incorrect durable statistics: %+v response=%+v summaries=%d models=%d", stats, response, summaries, models)
	}
	if s.HasPending() {
		t.Fatal("completed run left pending")
	}
}

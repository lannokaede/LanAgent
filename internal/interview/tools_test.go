package interview

import (
	"context"
	"encoding/json"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/rag"
	"github.com/lannokaede/LanAgent/internal/tool"
	"path/filepath"
	"strings"
	"testing"
)

func TestConversationTrainingKeepsQuestionAcrossExplanationAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conversation.interview.json")
	reviews := 0
	search := searchFunc(func(context.Context, string, string, int) ([]rag.Hit, error) {
		return []rag.Hit{{Source: "https://go.dev/ref/mem", Text: "channel synchronization"}}, nil
	})
	provider := providerFunc(func(_ context.Context, r *llm.ChatRequest) (*llm.ChatResponse, error) {
		reviews++
		if !strings.Contains(r.Messages[1].Content, "用户原始答案") {
			t.Fatal("host answer lost")
		}
		return &llm.ChatResponse{Content: `{"score":8,"feedback":"反馈","references":[1]}`}, nil
	})
	open := func() *Tools {
		v, e := OpenTools(path, "learning", "workspace|learning", search, provider)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	c := open()
	call := func(name, raw, id string) string {
		out, e := c.execute(tool.WithExecutionID(ctx, id), name, raw)
		if e != nil {
			t.Fatal(name, e)
		}
		return out
	}
	c.BindInput("initial", "模拟面试")
	call("interview_start", `{"topic":"concurrency","questions":3}`, "start")
	first := call("interview_next", `{}`, "next")
	if strings.Contains(first, "reference_answer") {
		t.Fatal("answer exposed")
	}
	if _, e := c.execute(tool.WithExecutionID(ctx, "fake"), "interview_submit_answer", `{"question_id":"channel-sync","input_ref":"initial"}`); e == nil {
		t.Fatal("answered question in the same user turn")
	}
	c.BindInput("explain", "这题不懂，先解释")
	status := call("interview_status", `{}`, "")
	if !strings.Contains(status, "channel-sync") {
		t.Fatal(status)
	}
	c = open()
	c.BindInput("resume", "继续面试")
	again := call("interview_next", `{}`, "resume-next")
	if !strings.Contains(again, "channel-sync") || reviews != 0 {
		t.Fatal("explanation or restart advanced question", again)
	}
	c.BindInput("answer", "用户原始答案")
	raw := `{"question_id":"channel-sync","input_ref":"answer"}`
	result := call("interview_submit_answer", raw, "answer-op")
	if reviews != 1 || !strings.Contains(result, "reviewed") {
		t.Fatal(result)
	}
	c = open()
	c.BindInput("answer", "用户原始答案")
	same := call("interview_submit_answer", raw, "answer-op")
	if same != result || reviews != 1 {
		t.Fatal("committed review replayed")
	}
	if reconciled, e := c.Reconcile("answer-op", "interview_submit_answer", raw); e != nil || reconciled != result {
		t.Fatal("receipt not recoverable", e)
	}
	if _, e := c.execute(tool.WithExecutionID(ctx, "invented"), "interview_submit_answer", `{"question_id":"channel-sync","input_ref":"answer","answer":"invented"}`); e == nil {
		t.Fatal("accepted generated answer")
	}
	c.BindInput("continue", "继续")
	next := call("interview_next", `{}`, "next-question")
	if !strings.Contains(next, "context-cancel") {
		t.Fatal(next)
	}
	var state map[string]any
	if json.Unmarshal([]byte(next), &state) != nil {
		t.Fatal(next)
	}
}

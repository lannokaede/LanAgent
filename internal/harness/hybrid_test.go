package harness

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lannokaede/LanAgent/internal/llm"
)

func TestHybridRoutingFallbacksAndClassifierAccounting(t *testing.T) {
	for _, tc := range []struct {
		name, input, output     string
		failure                 bool
		wantTools, wantClassify bool
	}{
		{"concept", "Go channel 的语义", `{"route":"direct"}`, false, false, true},
		{"unknown", "Go channel 的语义", `{"route":"agent"}`, false, true, true},
		{"bad json", "Go channel 的语义", `not JSON`, false, true, true},
		{"duplicate key", "Go channel 的语义", `{"route":"agent","route":"direct"}`, false, true, true},
		{"extra field", "Go channel 的语义", `{"route":"direct","extra":true}`, false, true, true},
		{"extra value", "Go channel 的语义", `{"route":"direct"} {}`, false, true, true},
		{"failure", "Go channel 的语义", ``, true, true, true},
		{"file guard", "读取 main.go", `{"route":"direct"}`, false, true, false},
		{"web guard", "搜索最新 Go 资料", `{"route":"direct"}`, false, true, false},
		{"interview guard", "继续面试", `{"route":"direct"}`, false, true, false},
		{"no tools wins", "不使用工具：解释这个文件，但信息不足请说明", ``, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			classifications := 0
			classifier := providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
				classifications++
				if req.ResponseFormat == nil || req.MaxTokens != 64 || len(req.Tools) != 0 || strings.Contains(req.Messages[1].Content, "system-secret") {
					return nil, errors.New("invalid classifier request")
				}
				if tc.failure {
					return nil, errors.New("offline")
				}
				return &llm.ChatResponse{Content: tc.output, Usage: llm.Usage{PromptTokens: 7, CompletionTokens: 2}}, nil
			})
			var saved Checkpoint
			runner := NewRunner(providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
				if (len(req.Tools) > 0) != tc.wantTools {
					return nil, errors.New("wrong tool capability")
				}
				return &llm.ChatResponse{Content: "answer", Usage: llm.Usage{PromptTokens: 3, CompletionTokens: 1}}, nil
			}), newTestRegistry(t), 3).WithRouting("hybrid", classifier, time.Second)
			cp := Checkpoint{Version: 1, RunID: "hybrid", Phase: "model", Messages: []llm.Message{{Role: llm.RoleSystem, Content: "system-secret"}, {Role: llm.RoleUser, Content: tc.input}}}
			_, answer, err := runner.Resume(context.Background(), cp, func(c Checkpoint) error { saved = c; return nil }, nil)
			if err != nil {
				t.Fatal(err)
			}
			if (classifications == 1) != tc.wantClassify {
				t.Fatal("incorrect classification count", classifications)
			}
			expected := 3
			if tc.wantClassify && !tc.failure {
				expected += 7
			}
			if answer.Usage.PromptTokens != expected || saved.RouteSource == "" {
				t.Fatal("classification usage/source missing", answer, saved)
			}
		})
	}
}
func TestMultiTurnExplicitReplyKeepsHistoryAndSavedRoute(t *testing.T) {
	history := []llm.Message{{Role: llm.RoleUser, Content: "解释 channel"}, {Role: llm.RoleAssistant, Content: "historical answer"}, {Role: llm.RoleUser, Content: "直接回答：再举一个例子"}}
	runner := NewRunner(providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		if len(req.Tools) != 0 || !strings.Contains(req.Messages[len(req.Messages)-2].Content, "historical answer") {
			return nil, errors.New("lost history or direct mode")
		}
		return &llm.ChatResponse{Content: "example"}, nil
	}), newTestRegistry(t), 3).WithDirectReplies()
	if _, _, err := runner.Run(context.Background(), history); err != nil {
		t.Fatal(err)
	}
}
func TestClassifierTimeoutAndHistoryBounds(t *testing.T) {
	classifier := providerFunc(func(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		if len(req.Messages[1].Content) > 8192 {
			return nil, errors.New("unbounded history")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	runner := NewRunner(providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		if len(req.Tools) == 0 {
			return nil, errors.New("timeout lost tools")
		}
		return &llm.ChatResponse{Content: "answer"}, nil
	}), newTestRegistry(t), 3).WithRouting("hybrid", classifier, time.Millisecond)
	history := []llm.Message{}
	for i := 0; i < 20; i++ {
		history = append(history, llm.Message{Role: llm.RoleUser, Content: strings.Repeat("historical ", 1000)}, llm.Message{Role: llm.RoleAssistant, Content: "answer"})
	}
	history = append(history, llm.Message{Role: llm.RoleUser, Content: "Go channel 的语义"})
	if _, _, err := runner.Run(context.Background(), history); err != nil {
		t.Fatal(err)
	}
}

func TestClassifierRecordingFailureDoesNotFallBackToMoreModelCalls(t *testing.T) {
	sentinel := errors.New("recording failed")
	classifications, mainCalls := 0, 0
	classifier, _ := llm.NewGateway(providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
		classifications++
		return &llm.ChatResponse{Content: `{"route":"direct"}`}, nil
	}), time.Second)
	runner := NewRunner(providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
		mainCalls++
		return &llm.ChatResponse{Content: "unexpected"}, nil
	}), newTestRegistry(t), 3).WithRouting("hybrid", classifier, time.Second)
	_, _, err := runner.RunWithEvents(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "Go channel 的语义"}}, func(e Event) error {
		if e.Type == "model_attempt_completed" {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) || classifications != 1 || mainCalls != 0 {
		t.Fatalf("recording failure fell back: %v classify=%d main=%d", err, classifications, mainCalls)
	}
}

package harness

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/lannokaede/LanAgent/internal/contextbudget"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/tool"
)

func TestDirectRepliesConservativelyRouteNewTurns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages []llm.Message
		direct   bool
	}{
		{"greeting", []llm.Message{{Role: llm.RoleUser, Content: "你好！"}}, true},
		{"thanks", []llm.Message{{Role: llm.RoleUser, Content: "Thanks!"}}, true},
		{"explicit", []llm.Message{{Role: llm.RoleUser, Content: "直接回答：Go 的 channel 是什么"}}, true},
		{"explicit no tools", []llm.Message{{Role: llm.RoleUser, Content: "不使用工具：解释 Redis 的事务原理"}}, true},
		{"generic explanation", []llm.Message{{Role: llm.RoleUser, Content: "解释一下 Go 的 channel"}}, false},
		{"mixed greeting", []llm.Message{{Role: llm.RoleUser, Content: "你好，帮我搜索 Go 官方资料"}}, false},
		{"file", []llm.Message{{Role: llm.RoleUser, Content: "直接回答：解释 main.go"}}, true},
		{"url", []llm.Message{{Role: llm.RoleUser, Content: "直接回答：https://go.dev"}}, true},
		{"attachment", []llm.Message{{Role: llm.RoleUser, Content: "直接回答：分析附件"}}, true},
		{"reference", []llm.Message{{Role: llm.RoleUser, Content: "直接回答：解释上述结论"}}, true},
		{"current evidence", []llm.Message{{Role: llm.RoleUser, Content: "直接回答：Redis 最新版本是什么"}}, true},
		{"action", []llm.Message{{Role: llm.RoleUser, Content: "不使用工具：运行测试"}}, true},
		{"empty explicit", []llm.Message{{Role: llm.RoleUser, Content: "直接回答："}}, false},
		{"follow-up", []llm.Message{{Role: llm.RoleUser, Content: "计算"}, {Role: llm.RoleAssistant, Content: "完成"}, {Role: llm.RoleUser, Content: "谢谢"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
				calls++
				if (len(req.Tools) == 0) != tc.direct {
					t.Fatalf("wrong tools for direct=%t: %+v", tc.direct, req.Tools)
				}
				return &llm.ChatResponse{Content: "answer"}, nil
			})
			var route string
			r := NewRunner(p, newTestRegistry(t), 4).WithDirectReplies()
			_, response, err := r.RunWithEvents(context.Background(), tc.messages, func(event Event) error {
				if event.Type == "route_selected" {
					route = event.Data.(map[string]any)["route"].(string)
				}
				return nil
			})
			if err != nil || response.Content != "answer" || calls != 1 || (route == routeDirect) != tc.direct {
				t.Fatalf("unexpected route/answer: %q %+v %v calls=%d", route, response, err, calls)
			}
		})
	}

	// The optimization is opt-in for callers outside the personal CLI.
	r := NewRunner(providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		if len(req.Tools) != 1 {
			t.Fatal("default Runner removed tool capabilities")
		}
		return &llm.ChatResponse{Content: "hello"}, nil
	}), newTestRegistry(t), 2)
	if _, _, err := r.Run(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "你好"}}); err != nil {
		t.Fatal(err)
	}
}

func TestDirectRepliesRejectUnadvertisedToolCalls(t *testing.T) {
	echo := &echoTestTool{}
	registry := tool.NewRegistry()
	if err := registry.Register(echo); err != nil {
		t.Fatal(err)
	}
	calls := 0
	r := NewRunner(providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		calls++
		if len(req.Tools) != 0 {
			t.Fatal("direct route advertised tools")
		}
		return &llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "unexpected", Name: "echo", ArgsJSON: "{}"}}}, nil
	}), registry, 4).WithDirectReplies()
	history, _, err := r.Run(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "你好"}})
	if err == nil || !strings.Contains(err.Error(), "no tools were executed") || echo.calls != 0 || calls != 1 || len(history) != 1 {
		t.Fatalf("unexpected tool execution: history=%+v error=%v tool=%d model=%d", history, err, echo.calls, calls)
	}
}

func TestDirectRepliesKeepMemoryPinnedStateAndBudget(t *testing.T) {
	recalled := false
	input := []llm.Message{{Role: llm.RoleSystem, Content: "trusted rules"}, {Role: llm.RoleUser, Content: "你好"}}
	r := NewRunner(providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		if len(req.Tools) != 0 || req.MaxTokens != 512 {
			t.Fatalf("direct request skipped tools/budget policy: %+v", req)
		}
		if !strings.Contains(req.Messages[0].Content, "trusted rules") || !strings.Contains(req.Messages[0].Content, "pinned-state") {
			t.Fatal("lost trusted rules or pinned state")
		}
		if len(req.Messages) != 3 || req.Messages[1].Content != "saved preference" || req.Messages[2].Content != "你好" {
			t.Fatalf("lost current memory or user input: %+v", req.Messages)
		}
		return &llm.ChatResponse{Content: "你好", Usage: llm.Usage{PromptTokens: 12, CompletionTokens: 2}}, nil
	}), newTestRegistry(t), 2).WithDirectReplies().
		WithBudget(contextbudget.Policy{Window: 8192, OutputReserve: 512}).
		WithPinnedState(func() string { return "pinned-state" }).
		WithRecall(func(_ context.Context, query string) (string, error) {
			recalled = query == "你好"
			return "saved preference", nil
		})
	_, response, err := r.Run(context.Background(), input)
	if err != nil || !recalled || response.Usage.PromptTokens != 12 || response.Usage.CompletionTokens != 2 {
		t.Fatalf("failed to preserve direct request policies: %+v %v recalled=%t", response, err, recalled)
	}
	if input[0].Content != "trusted rules" || len(input) != 2 {
		t.Fatal("routing modified durable input")
	}
}

func TestDirectReplyRouteSurvivesModelOutageAndRunnerReconfiguration(t *testing.T) {
	registry := newTestRegistry(t)
	initial := Checkpoint{Version: 1, RunID: "new-turn", Phase: "model", Messages: []llm.Message{{Role: llm.RoleUser, Content: "你好"}}}
	var saved Checkpoint
	sink := func(cp Checkpoint) error { saved = cp; return nil }
	offline := errors.New("offline")
	routed := NewRunner(providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
		if saved.Route != routeDirect || saved.Round != 0 {
			t.Fatal("route was not persisted before the model request")
		}
		return nil, offline
	}), registry, 4).WithDirectReplies()
	initial.Profile = routed.ProfileID()
	if _, _, err := routed.Resume(context.Background(), initial, sink, nil); !errors.Is(err, offline) {
		t.Fatal(err)
	}
	resumed := NewRunner(providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		if len(req.Tools) != 0 {
			t.Fatal("resume changed a persisted direct route")
		}
		return &llm.ChatResponse{Content: "done"}, nil
	}), registry, 4)
	if routed.ProfileID() != resumed.ProfileID() {
		t.Fatal("enabling routing invalidated an existing permission profile")
	}
	if _, _, err := resumed.Resume(context.Background(), saved, sink, nil); err != nil || saved.Route != routeDirect || saved.Phase != "done" {
		t.Fatalf("route did not survive recovery: %+v %v", saved, err)
	}
}

func TestDirectRepliesPreserveLegacyToolBatchAndRejectInvalidRoute(t *testing.T) {
	call := llm.ToolCall{ID: "legacy", Name: "add", ArgsJSON: `{"a":2,"b":3}`}
	cp := Checkpoint{Version: 1, RunID: "legacy", Phase: "tools", Round: 1, Calls: 0, Pending: []llm.ToolCall{call}, Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "你好"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}},
	}}
	calls := 0
	r := NewRunner(providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		calls++
		if len(req.Tools) != 1 || req.Messages[len(req.Messages)-1].Content != "5" {
			t.Fatalf("legacy tool batch lost capability or observation: %+v", req)
		}
		return &llm.ChatResponse{Content: "done"}, nil
	}), newTestRegistry(t), 4).WithDirectReplies()
	if _, _, err := r.Resume(context.Background(), cp, nil, nil); err != nil || calls != 1 {
		t.Fatalf("legacy batch did not resume: %v calls=%d", err, calls)
	}
	cp.Route = routeDirect
	if _, _, err := r.Resume(context.Background(), cp, nil, nil); err == nil || calls != 1 {
		t.Fatalf("invalid direct checkpoint reached the model: %v calls=%d", err, calls)
	}
	cp.Route = "unknown"
	if _, _, err := r.Resume(context.Background(), cp, nil, nil); err == nil || calls != 1 {
		t.Fatalf("unknown checkpoint route reached the model: %v calls=%d", err, calls)
	}
}

func TestDirectRepliesDoNotSharePerTurnRoute(t *testing.T) {
	r := NewRunner(providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		last := req.Messages[len(req.Messages)-1].Content
		if (last == "你好") != (len(req.Tools) == 0) {
			t.Errorf("route crossed concurrent turns: %+v", req)
		}
		return &llm.ChatResponse{Content: "done"}, nil
	}), newTestRegistry(t), 4).WithDirectReplies()
	var workers sync.WaitGroup
	for i := 0; i < 10; i++ {
		for _, input := range []string{"你好", "读取文件"} {
			workers.Add(1)
			go func(input string) {
				defer workers.Done()
				if _, _, err := r.Run(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: input}}); err != nil {
					t.Error(err)
				}
			}(input)
		}
	}
	workers.Wait()
}

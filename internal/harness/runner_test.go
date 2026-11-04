package harness

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/tool"
)

type providerFunc func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error)

func (f providerFunc) Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	return f(ctx, req)
}

func TestRunnerMultipleCallsPreserveProtocol(t *testing.T) {
	calls := 0
	p := providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		calls++
		if len(req.Tools) != 1 || req.Tools[0].Parameters["type"] != "object" {
			t.Fatal("lost typed tool schema")
		}
		if calls == 1 {
			return &llm.ChatResponse{Content: "先计算", ToolCalls: []llm.ToolCall{
				{ID: "one", Name: "add", ArgsJSON: `{"a":2,"b":3}`},
				{ID: "two", Name: "add", ArgsJSON: `{"a":4,"b":5}`},
			}, Usage: llm.Usage{PromptTokens: 10}}, nil
		}
		m := req.Messages
		if len(m) != 4 || m[1].Content != "先计算" || len(m[1].ToolCalls) != 2 || m[2].ToolCallID != "one" || m[2].Content != "5" || m[3].ToolCallID != "two" || m[3].Content != "9" {
			t.Fatalf("broken protocol: %+v", m)
		}
		return &llm.ChatResponse{Content: "5 和 9", Usage: llm.Usage{PromptTokens: 20, CompletionTokens: 2}}, nil
	})
	var events []Event
	m, resp, err := NewRunner(p, newTestRegistry(t), 5).RunWithEvents(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "计算"}}, func(ev Event) error {
		if ev.Type == "tool_call" || ev.Type == "tool_result" {
			events = append(events, ev)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 5 || resp.Usage.PromptTokens != 30 || len(events) != 4 {
		t.Fatalf("history/usage/events: %+v %+v %+v", m, resp, events)
	}
}

func TestRunnerToolErrorsBecomeObservations(t *testing.T) {
	for _, call := range []llm.ToolCall{{ID: "x", Name: "missing", ArgsJSON: `{}`}, {ID: "x", Name: "add", ArgsJSON: `{"a":2}`}, {ID: "x", Name: "add", ArgsJSON: `{"__arg1":"{\"a\":2,\"b\":3}"}`}} {
		t.Run(call.Name+call.ArgsJSON, func(t *testing.T) {
			calls := 0
			p := providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
				calls++
				if calls == 1 {
					return &llm.ChatResponse{ToolCalls: []llm.ToolCall{call}}, nil
				}
				last := req.Messages[len(req.Messages)-1]
				if last.ToolCallID != "x" || !strings.HasPrefix(last.Content, "error:") {
					t.Fatalf("missing error observation: %+v", last)
				}
				return &llm.ChatResponse{Content: "修正"}, nil
			})
			if _, _, err := NewRunner(p, newTestRegistry(t), 3).Run(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "计算"}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunnerLimitAndCallbackCancellation(t *testing.T) {
	calls := 0
	p := providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
		calls++
		return &llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "x", Name: "add", ArgsJSON: `{"a":1,"b":2}`}}}, nil
	})
	m, _, err := NewRunner(p, newTestRegistry(t), 2).Run(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "计算"}})
	if err == nil || calls != 2 || len(m) != 5 {
		t.Fatalf("limit not enforced: calls=%d history=%d err=%v", calls, len(m), err)
	}
	calls = 0
	stop := errors.New("client disconnected")
	m, _, err = NewRunner(p, newTestRegistry(t), 5).RunWithEvents(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "计算"}}, func(ev Event) error {
		if ev.Type == "tool_call" {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || calls != 1 || len(m) != 2 {
		t.Fatalf("callback continued execution: %+v %v", m, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = 0
	_, _, err = NewRunner(p, newTestRegistry(t), 5).Run(ctx, []llm.Message{{Role: llm.RoleUser, Content: "计算"}})
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("cancel ignored: %v calls=%d", err, calls)
	}
}

// 测试使用启动时构造的只读注册表。
func newTestRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	r := tool.NewRegistry()
	if err := r.Register(tool.AddTool{}); err != nil {
		t.Fatal(err)
	}
	return r
}

// 第二种能力仅作为测试夹具，用于发现执行器误将不同工具路由到 add 的问题。
type echoTestTool struct{ calls int }

func (*echoTestTool) Spec() llm.ToolDef {
	return llm.ToolDef{Name: "echo", Description: "回显测试参数", Parameters: map[string]any{"type": "object"}}
}
func (t *echoTestTool) Execute(ctx context.Context, args string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	t.calls++
	return "echo:" + args, nil
}

func TestRunnerRoutesDifferentRegisteredTools(t *testing.T) {
	registry := newTestRegistry(t)
	echo := &echoTestTool{}
	if err := registry.Register(echo); err != nil {
		t.Fatal(err)
	}
	modelCalls := 0
	p := providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		modelCalls++
		// 模型只能看到实际工具定义，不能看到内部错误适配器。
		if len(req.Tools) != 2 || req.Tools[0].Name != "add" || req.Tools[1].Name != "echo" {
			t.Fatalf("unexpected tools: %+v", req.Tools)
		}
		if modelCalls == 1 {
			return &llm.ChatResponse{Content: "执行工具", ToolCalls: []llm.ToolCall{
				{ID: "echo-1", Name: "echo", ArgsJSON: `{"text":"hello"}`},
				{ID: "missing-1", Name: "missing", ArgsJSON: `{}`},
				{ID: "add-1", Name: "add", ArgsJSON: `{"a":2,"b":3}`},
			}, Usage: llm.Usage{PromptTokens: 2, CompletionTokens: 1}}, nil
		}
		if modelCalls != 2 {
			t.Fatalf("unexpected model call %d", modelCalls)
		}
		m := req.Messages
		if len(m) != 5 || m[1].Content != "执行工具" || len(m[1].ToolCalls) != 3 {
			t.Fatalf("lost original call group: %+v", m)
		}
		wantIDs := []string{"echo-1", "missing-1", "add-1"}
		wantResults := []string{`echo:{"text":"hello"}`, "error: unknown tool missing", "5"}
		for i := range wantIDs {
			if m[i+2].Role != llm.RoleTool || m[i+2].ToolCallID != wantIDs[i] || m[i+2].Content != wantResults[i] {
				t.Fatalf("incorrect route/result: %+v", m[i+2])
			}
		}
		return &llm.ChatResponse{Content: "完成", Usage: llm.Usage{PromptTokens: 3, CompletionTokens: 2}}, nil
	})
	var events []Event
	m, resp, err := NewRunner(p, registry, 5).RunWithEvents(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "测试"}}, func(ev Event) error {
		if ev.Type == "tool_call" || ev.Type == "tool_result" {
			events = append(events, ev)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 6 || resp.Content != "完成" || resp.Usage.PromptTokens != 5 || resp.Usage.CompletionTokens != 3 || echo.calls != 1 || len(events) != 6 {
		t.Fatalf("unexpected execution: history=%+v response=%+v echoCalls=%d events=%+v", m, resp, echo.calls, events)
	}
	for i, name := range []string{"echo", "missing", "add"} {
		if events[i*2].Type != "tool_call" || events[i*2].Data.(map[string]any)["name"] != name || events[i*2+1].Type != "tool_result" {
			t.Fatalf("incorrect event order: %+v", events)
		}
		wantStatus := "ok"
		if name == "missing" {
			wantStatus = "error"
		}
		if events[i*2+1].Data.(map[string]any)["status"] != wantStatus {
			t.Fatalf("incorrect result status: %+v", events[i*2+1])
		}
	}
}

func TestGraphRejectsMalformedDecisionsBeforeExecutingTools(t *testing.T) {
	for _, response := range []*llm.ChatResponse{
		nil, {},
		{ToolCalls: []llm.ToolCall{{Name: "echo", ArgsJSON: "{}"}}},
		{ToolCalls: []llm.ToolCall{{ID: "x", Name: "echo"}, {ID: "x", Name: "echo"}}},
	} {
		echo := &echoTestTool{}
		registry := tool.NewRegistry()
		if err := registry.Register(echo); err != nil {
			t.Fatal(err)
		}
		p := providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) { return response, nil })
		messages, final, err := NewRunner(p, registry, 2).Run(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "test"}})
		if err == nil || final != nil || echo.calls != 0 || len(messages) != 1 {
			t.Fatalf("malformed response executed: history=%+v final=%+v tools=%d err=%v", messages, final, echo.calls, err)
		}
	}
}

func TestGraphDoesNotReplayToolAfterResultEventFailure(t *testing.T) {
	echo := &echoTestTool{}
	registry := tool.NewRegistry()
	if err := registry.Register(echo); err != nil {
		t.Fatal(err)
	}
	modelCalls := 0
	p := providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
		modelCalls++
		return &llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "x", Name: "echo", ArgsJSON: "{}"}}}, nil
	})
	stop := errors.New("output disconnected")
	messages, _, err := NewRunner(p, registry, 3).RunWithEvents(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "test"}}, func(ev Event) error {
		if ev.Type == "tool_result" {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || modelCalls != 1 || echo.calls != 1 || len(messages) != 3 || messages[2].ToolCallID != "x" {
		t.Fatalf("tool replayed or partial history lost: model=%d tools=%d history=%+v err=%v", modelCalls, echo.calls, messages, err)
	}
}

func TestGraphConcurrentRunsKeepStateIsolated(t *testing.T) {
	p := providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
		return &llm.ChatResponse{Content: req.Messages[0].Content, Usage: llm.Usage{PromptTokens: 1}}, nil
	})
	runner := NewRunner(p, newTestRegistry(t), 2)
	results := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func(i int) {
			input := strings.Repeat("x", i+1)
			messages, response, err := runner.Run(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: input}})
			if err == nil && (response.Content != input || response.Usage.PromptTokens != 1 || len(messages) != 2 || messages[0].Content != input) {
				err = errors.New("concurrent state mixed")
			}
			results <- err
		}(i)
	}
	for i := 0; i < 20; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestGraphCancellationDuringResetDoesNotStartModel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	p := providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
		calls++
		return &llm.ChatResponse{Content: "done"}, nil
	})
	_, _, err := NewRunner(p, newTestRegistry(t), 2).RunWithEvents(ctx, []llm.Message{{Role: llm.RoleUser, Content: "test"}}, func(Event) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("model called after cancellation: calls=%d err=%v", calls, err)
	}
}

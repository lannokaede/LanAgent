package harness

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/tool"
)

type policyTool struct {
	calls   int
	execute func(context.Context) (string, error)
}

func (*policyTool) Spec() llm.ToolDef {
	return llm.ToolDef{Name: "count", Parameters: map[string]any{"type": "object"}}
}
func (t *policyTool) Execute(ctx context.Context, _ string) (string, error) {
	t.calls++
	if t.execute != nil {
		return t.execute(ctx)
	}
	return "done", nil
}

func TestToolBudgetRejectsBatchBeforeSideEffectsAndResetsPerRun(t *testing.T) {
	count := &policyTool{}
	registry := tool.NewRegistry()
	if err := registry.Register(count); err != nil {
		t.Fatal(err)
	}
	p := providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
		return &llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "a", Name: "count", ArgsJSON: "{}"}, {ID: "b", Name: "count", ArgsJSON: "{}"}}}, nil
	})
	runner := NewRunner(p, registry, 3).WithToolPolicy(ToolPolicy{MaxCalls: 3})
	for i := 0; i < 2; i++ {
		_, _, err := runner.Run(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "test"}})
		if !errors.Is(err, ErrToolBudget) || count.calls != 2*(i+1) {
			t.Fatalf("calls=%d err=%v", count.calls, err)
		}
	}
}

func TestToolPolicyBoundsArgumentsResultsErrorsAndDeadlines(t *testing.T) {
	for _, mode := range []string{"args", "result", "error", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			count := &policyTool{execute: func(ctx context.Context) (string, error) {
				switch mode {
				case "result":
					return strings.Repeat("x", 1024), nil
				case "error":
					return "", errors.New(strings.Repeat("x", 1024))
				case "timeout":
					<-ctx.Done()
					return "", ctx.Err()
				default:
					return "", nil
				}
			}}
			registry := tool.NewRegistry()
			if err := registry.Register(count); err != nil {
				t.Fatal(err)
			}
			calls := 0
			p := providerFunc(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
				calls++
				if calls == 1 {
					return &llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "x", Name: "count", ArgsJSON: strings.Repeat(" ", 10)}}}, nil
				}
				last := req.Messages[len(req.Messages)-1]
				if last.ToolCallID != "x" || !strings.HasPrefix(last.Content, "error:") || len(last.Content) > 128 {
					t.Fatalf("unbounded or broken observation: %+v", last)
				}
				return &llm.ChatResponse{Content: "handled"}, nil
			})
			policy := ToolPolicy{MaxResultBytes: 128, Timeout: time.Millisecond}
			if mode == "args" {
				policy.MaxArgsBytes = 2
			}
			_, _, err := NewRunner(p, registry, 3).WithToolPolicy(policy).Run(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "test"}})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "args" && count.calls != 0 {
				t.Fatal("oversized arguments reached tool")
			}
		})
	}
}

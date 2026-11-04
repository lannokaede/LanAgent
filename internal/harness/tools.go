package harness

import (
	"context"
	"errors"
	"fmt"
	"github.com/lannokaede/LanAgent/internal/tool"
	"time"

	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/observability/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// executeTools 保留工具顺序和原始参数；普通错误作为观察消息供模型修正。
func (r *Runner) executeTools(ctx context.Context, state *runState) (*runState, error) {
	// Reject an over-budget batch before its first side effect, rather than
	// partially executing a model decision. No tool is retried by the harness.
	if r.tools.MaxCalls > 0 && len(state.pending) > r.tools.MaxCalls-state.toolCalls {
		return state, ErrToolBudget
	}
	for len(state.pending) > 0 {
		if err := ctx.Err(); err != nil {
			return state, err
		}
		call := state.pending[0]
		state.toolCalls++
		state.running = state.executionID(call)
		if err := state.checkpoint("tools"); err != nil {
			return state, err
		}
		if err := r.executeTool(ctx, state, call); err != nil {
			return state, err
		}
	}
	if err := state.checkpoint("model"); err != nil {
		return state, err
	}
	return state, nil
}

func (r *Runner) executeTool(ctx context.Context, state *runState, call llm.ToolCall) (toolErr error) {
	ctx, span := telemetry.Start(ctx, "agent.tool")
	defer func() { telemetry.Finish(span, toolErr) }()
	if len(call.Name) <= 128 {
		span.SetAttributes(attribute.String("tool.name", call.Name))
	}
	if state.emit != nil {
		if err := state.emit(Event{Type: "tool_call", Data: map[string]any{"id": call.ID, "execution_id": state.executionID(call), "name": call.Name, "args": call.ArgsJSON}}); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	start := time.Now()
	status := "ok"
	var output string
	// 取消立即中断图；普通错误作为 observation 返回给模型。
	// 因此坏参数可以被模型修正，但客户端断开不会启动下一次模型请求。
	// 按原始名称精确查找，未知名称也产生相同调用 ID 的错误观察。
	registeredTool, exists := r.registry.Lookup(call.Name)
	if r.tools.MaxArgsBytes > 0 && len(call.ArgsJSON) > r.tools.MaxArgsBytes {
		output = "error: tool arguments exceed harness byte limit"
		status = "error"
	} else if !exists {
		output = "error: unknown tool " + call.Name
		status = "error"
	} else {
		callCtx := ctx
		cancel := func() {}
		if r.tools.Timeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, r.tools.Timeout)
		}
		result, err := registeredTool.Execute(callCtx, call.ArgsJSON)
		if callCtx.Err() != nil {
			err = errors.Join(err, callCtx.Err())
		}
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, tool.ErrUncertain) {
			return fmt.Errorf("%w: %s", ErrUncertainTool, state.running)
		}
		if err == nil && r.tools.MaxResultBytes > 0 && len(result) > r.tools.MaxResultBytes {
			err = fmt.Errorf("tool result exceeds harness byte limit; narrow the request")
		}
		if err != nil {
			output = "error: " + err.Error()
			status = "error"
		} else {
			output = result
		}
	}
	// Error strings are also untrusted observations and must obey the same cap.
	if limit := r.tools.MaxResultBytes; limit > 0 && len(output) > limit {
		output = "error: tool observation exceeds harness byte limit"
		if len(output) > limit {
			output = output[:limit]
		}
		status = "error"
	}
	state.messages = append(state.messages, llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Content: output})
	span.SetAttributes(attribute.String("tool.status", status))
	if status == "error" {
		span.SetStatus(codes.Error, "tool rejected")
	}
	state.pending = state.pending[1:]
	state.running = ""
	if err := state.checkpoint("tools"); err != nil {
		return err
	}

	if state.emit != nil {
		if err := state.emit(Event{Type: "tool_result", Data: map[string]any{"id": call.ID, "execution_id": state.executionID(call), "result": output, "ms": time.Since(start).Milliseconds(), "status": status}}); err != nil {
			return err
		}
	}
	return nil
}

package llm

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/lannokaede/LanAgent/internal/observability/logger"
	"github.com/lannokaede/LanAgent/internal/observability/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
)

// 模块 1：模型调用策略。LangGraphGo 模型节点 → Gateway → OpenAIProvider。
// LangChainGo 的模型适配不替代本项目的超时/日志策略；这个网关仍只调用一次，
// 没有自动重试、备用模型或熔断。TODO：需要多模型时在此增加可配置路由。
type Gateway struct {
	provider Provider
	timeout  time.Duration
	slots    chan struct{}
	active   atomic.Int64
	waiting  atomic.Int64
}

// 编译时确认 Gateway 实现了 Provider 接口。
var _ Provider = (*Gateway)(nil)
var _ StreamProvider = (*Gateway)(nil)

func NewGateway(provider Provider, timeout time.Duration) (*Gateway, error) {
	if provider == nil {
		return nil, fmt.Errorf("llm gateway: provider is nil")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("llm gateway: timeout must be positive")
	}
	return &Gateway{provider: provider, timeout: timeout, slots: make(chan struct{}, 2)}, nil
}

func (g *Gateway) Chat(
	ctx context.Context,
	req *ChatRequest,
) (*ChatResponse, error) {
	return g.call(ctx, req, nil)
}

// 流式请求也必须经过同一网关，不能绕过单次调用超时和安全日志。
func (g *Gateway) StreamChat(ctx context.Context, req *ChatRequest, emit func(string) error) (*ChatResponse, error) {
	return g.call(ctx, req, emit)
}

func (g *Gateway) call(ctx context.Context, req *ChatRequest, emit func(string) error) (result *ChatResponse, callErr error) {
	ctx, span := telemetry.Start(ctx, "llm.call")
	defer func() {
		if result != nil {
			span.SetAttributes(attribute.Int("gen_ai.usage.input_tokens", result.Usage.PromptTokens), attribute.Int("gen_ai.usage.output_tokens", result.Usage.CompletionTokens))
			if result.TTFT > 0 {
				span.SetAttributes(attribute.Float64("gen_ai.response.ttft_seconds", result.TTFT.Seconds()))
			}
		}
		telemetry.Finish(span, callErr)
	}()
	// 模块 2：先校验，再派生单次调用 context，最后记录耗时和成功状态。
	// 总轮次超时由 CLI/HTTP 控制；此处 45 秒限制只针对一次模型请求。
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("llm gateway: %w", err)
	}
	if req == nil || len(req.Messages) == 0 {
		return nil, fmt.Errorf("llm gateway: messages cannot be empty")
	}

	g.waiting.Add(1)
	waitStart := time.Now()
	select {
	case g.slots <- struct{}{}:
		g.waiting.Add(-1)
	case <-ctx.Done():
		g.waiting.Add(-1)
		return nil, ctx.Err()
	}
	g.active.Add(1)
	span.SetAttributes(attribute.Float64("llm.queue_wait_seconds", time.Since(waitStart).Seconds()))
	defer func() { g.active.Add(-1); <-g.slots }()
	callCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	start := time.Now()
	var resp *ChatResponse
	var err error
	if emit != nil {
		resp, err = ChatWithStream(callCtx, g.provider, req, emit)
	} else {
		resp, err = g.provider.Chat(callCtx, req)
	}

	// 只记录耗时和状态，不记录密钥或对话内容。
	logger.Info("llm chat",
		zap.Duration("duration", time.Since(start)),
		zap.Bool("success", err == nil && resp != nil),
	)

	if err != nil {
		return nil, fmt.Errorf("llm gateway: %w", err)
	}
	if resp == nil {
		return nil, fmt.Errorf("llm gateway: provider returned nil response")
	}
	return resp, nil
}

// Configure before sharing the gateway with workers.
func (g *Gateway) SetConcurrency(n int) {
	if n > 0 {
		g.slots = make(chan struct{}, n)
	}
}
func (g *Gateway) Stats() map[string]any {
	return map[string]any{"active": g.active.Load(), "waiting": g.waiting.Load(), "capacity": cap(g.slots)}
}

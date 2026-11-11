package llm

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/lannokaede/LanAgent/internal/observability/telemetry"
	"go.opentelemetry.io/otel/attribute"
)

// RetryPolicy bounds upstream attempts independently of ReAct rounds. Configure
// before sharing the gateway; tools are never executed or retried here.
type RetryPolicy struct {
	MaxAttempts int           `mapstructure:"max_attempts"`
	BaseDelay   time.Duration `mapstructure:"base_delay"`
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 3, BaseDelay: 250 * time.Millisecond}
}
func (p RetryPolicy) Validate() error {
	if p.MaxAttempts < 1 || p.MaxAttempts > 5 || p.BaseDelay <= 0 || p.BaseDelay > 10*time.Second {
		return errors.New("invalid model retry policy")
	}
	return nil
}

type Gateway struct {
	provider Provider
	timeout  time.Duration
	slots    chan struct{}
	active   atomic.Int64
	waiting  atomic.Int64
	retry    RetryPolicy
}

var _ Provider = (*Gateway)(nil)
var _ StreamProvider = (*Gateway)(nil)

func NewGateway(provider Provider, timeout time.Duration) (*Gateway, error) {
	if provider == nil || timeout <= 0 {
		return nil, errors.New("llm gateway requires provider and positive timeout")
	}
	return &Gateway{provider: provider, timeout: timeout, slots: make(chan struct{}, 2), retry: DefaultRetryPolicy()}, nil
}
func (g *Gateway) SetRetryPolicy(p RetryPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	g.retry = p
	return nil
}
func (g *Gateway) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	return g.call(ctx, req, nil)
}
func (g *Gateway) StreamChat(ctx context.Context, req *ChatRequest, emit func(string) error) (*ChatResponse, error) {
	return g.call(ctx, req, emit)
}

func (g *Gateway) acquire(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	g.waiting.Add(1)
	defer g.waiting.Add(-1)
	select {
	case g.slots <- struct{}{}:
		g.active.Add(1)
		return time.Since(start), nil
	case <-ctx.Done():
		return time.Since(start), ctx.Err()
	}
}
func (g *Gateway) release() { g.active.Add(-1); <-g.slots }

func (g *Gateway) call(ctx context.Context, req *ChatRequest, emit func(string) error) (result *ChatResponse, callErr error) {
	metadata, _ := ctx.Value(callContextKey{}).(callContext)
	purpose := metadata.purpose
	if purpose == "" {
		purpose = "model"
	}
	ctx, span := telemetry.Start(ctx, "llm.call", attribute.String("llm.purpose", purpose))
	defer func() { telemetry.Finish(span, callErr) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil || len(req.Messages) == 0 {
		return nil, errors.New("llm gateway: messages cannot be empty")
	}
	// The first queue wait remains bounded by the caller's task deadline. After
	// admission, all attempts, backoffs and subsequent queue waits share one budget.
	wait, err := g.acquire(ctx)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	for attempt := 1; attempt <= g.retry.MaxAttempts; attempt++ {
		if err := callCtx.Err(); err != nil {
			g.release()
			return nil, err
		}
		if err := observe(callCtx, CallEvent{Type: "model_attempt_started", Attempt: attempt, QueueMS: float64(wait) / float64(time.Millisecond)}); err != nil {
			g.release()
			return nil, &ObserverError{Cause: err}
		}
		attemptCtx, attemptSpan := telemetry.Start(callCtx, "llm.attempt", attribute.Int("llm.attempt", attempt), attribute.Float64("llm.queue_wait_seconds", wait.Seconds()))
		start := time.Now()
		callbackSeen := false
		result, err = func() (*ChatResponse, error) {
			defer g.release()
			if emit != nil {
				return ChatWithStream(attemptCtx, g.provider, req, func(text string) error { callbackSeen = true; return emit(text) })
			}
			return g.provider.Chat(attemptCtx, req)
		}()
		if err == nil && result == nil {
			err = errors.New("llm gateway: provider returned nil response")
		}
		if callCtx.Err() != nil {
			err = callCtx.Err()
		}
		event := CallEvent{Type: "model_attempt_completed", Attempt: attempt, DurationMS: float64(time.Since(start)) / float64(time.Millisecond), Status: "ok"}
		if result != nil {
			event.TTFTMS = float64(result.TTFT) / float64(time.Millisecond)
			event.UsageKnown = result.UsageKnown || result.Usage.PromptTokens != 0 || result.Usage.CompletionTokens != 0
			if event.UsageKnown {
				usage := result.Usage
				event.Usage = &usage
				attemptSpan.SetAttributes(attribute.Int("gen_ai.usage.input_tokens", usage.PromptTokens), attribute.Int("gen_ai.usage.output_tokens", usage.CompletionTokens))
			}
			if result.TTFT > 0 {
				attemptSpan.SetAttributes(attribute.Float64("gen_ai.response.ttft_seconds", result.TTFT.Seconds()))
			}
		}
		var upstream *UpstreamError
		if err != nil {
			event.Status = "error"
			if errors.As(err, &upstream) {
				event.HTTPStatus = upstream.StatusCode
			}
		}
		telemetry.Finish(attemptSpan, err)
		if observerErr := observe(callCtx, event); observerErr != nil {
			return nil, &ObserverError{Cause: observerErr}
		}
		if err == nil {
			return result, nil
		}
		// Only explicit HTTP failures before a successful response can retry. An
		// SSE parse/network/callback error never has this type and is not replayed.
		if callbackSeen || attempt == g.retry.MaxAttempts || callCtx.Err() != nil || upstream == nil || !retryableStatus(upstream.StatusCode) {
			return nil, fmt.Errorf("llm gateway: %w", err)
		}
		delay := time.Duration(rand.Int64N(int64(g.retry.BaseDelay)*(1<<uint(attempt-1)) + 1))
		delay = max(delay, upstream.RetryAfter)
		if deadline, ok := callCtx.Deadline(); ok && time.Until(deadline) <= delay {
			return nil, fmt.Errorf("llm gateway: %w", err)
		}
		if observerErr := observe(callCtx, CallEvent{Type: "model_retry", Attempt: attempt, HTTPStatus: upstream.StatusCode, RetryMS: float64(delay) / float64(time.Millisecond)}); observerErr != nil {
			return nil, &ObserverError{Cause: observerErr}
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-callCtx.Done():
			timer.Stop()
			return nil, callCtx.Err()
		}
		wait, err = g.acquire(callCtx)
		if err != nil {
			return nil, err
		}
	}
	return nil, errors.New("model attempt limit reached")
}
func retryableStatus(status int) bool {
	return status == 429 || status == 502 || status == 503 || status == 504
}

// Configure before sharing with workers.
func (g *Gateway) SetConcurrency(n int) {
	if n > 0 {
		g.slots = make(chan struct{}, n)
	}
}
func (g *Gateway) Stats() map[string]any {
	return map[string]any{"active": g.active.Load(), "waiting": g.waiting.Load(), "capacity": cap(g.slots)}
}

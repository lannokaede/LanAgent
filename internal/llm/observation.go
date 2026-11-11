package llm

import "context"

// CallEvent contains metadata only; observations never contain prompts, tool
// arguments, credentials, upstream bodies or raw errors. Observer errors stop
// the logical call and must never be classified as retryable upstream errors.
type CallEvent struct {
	Type       string  `json:"type"`
	Purpose    string  `json:"purpose"`
	Attempt    int     `json:"attempt,omitempty"`
	Status     string  `json:"status,omitempty"`
	HTTPStatus int     `json:"http_status,omitempty"`
	QueueMS    float64 `json:"queue_ms,omitempty"`
	DurationMS float64 `json:"duration_ms,omitempty"`
	RetryMS    float64 `json:"retry_ms,omitempty"`
	TTFTMS     float64 `json:"ttft_ms,omitempty"`
	Usage      *Usage  `json:"usage,omitempty"`
	UsageKnown bool    `json:"usage_known"`
}

// ObserverError is a host recording/callback failure, never an upstream failure
// or a reason to continue through classifier/summary fallback.
type ObserverError struct{ Cause error }

func (e *ObserverError) Error() string { return "model event observer failed" }
func (e *ObserverError) Unwrap() error { return e.Cause }

type callContext struct {
	purpose  string
	observer func(CallEvent) error
}
type callContextKey struct{}

func WithObserver(ctx context.Context, observer func(CallEvent) error) context.Context {
	c, _ := ctx.Value(callContextKey{}).(callContext)
	c.observer = observer
	return context.WithValue(ctx, callContextKey{}, c)
}
func WithPurpose(ctx context.Context, purpose string) context.Context {
	c, _ := ctx.Value(callContextKey{}).(callContext)
	c.purpose = purpose
	return context.WithValue(ctx, callContextKey{}, c)
}
func observe(ctx context.Context, event CallEvent) error {
	c, _ := ctx.Value(callContextKey{}).(callContext)
	event.Purpose = c.purpose
	if event.Purpose == "" {
		event.Purpose = "model"
	}
	if c.observer != nil {
		return c.observer(event)
	}
	return nil
}

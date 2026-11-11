package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type retryProviderFunc func(context.Context, *ChatRequest) (*ChatResponse, error)

func (f retryProviderFunc) Chat(c context.Context, r *ChatRequest) (*ChatResponse, error) {
	return f(c, r)
}

type retryStreamProvider struct {
	calls         int
	callbackError bool
}

func (p *retryStreamProvider) Chat(context.Context, *ChatRequest) (*ChatResponse, error) {
	panic("expected stream")
}
func (p *retryStreamProvider) StreamChat(_ context.Context, _ *ChatRequest, emit func(string) error) (*ChatResponse, error) {
	p.calls++
	if err := emit("partial"); err != nil {
		return nil, fmt.Errorf("callback: %w", err)
	}
	return nil, &UpstreamError{StatusCode: 503}
}
func TestGatewayHTTPRetryAndUsageOnBothTransports(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(503)
					fmt.Fprint(w, "private-body")
					return
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprint(w, `{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2}}`)
				}
			}))
			defer server.Close()
			p, err := NewOpenAIProvider(server.URL, "test", "secret")
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			g, _ := NewGateway(p, time.Second)
			g.SetRetryPolicy(RetryPolicy{3, time.Millisecond})
			events := []CallEvent{}
			ctx := WithObserver(context.Background(), func(e CallEvent) error { events = append(events, e); return nil })
			req := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "private-prompt"}}}
			var answer *ChatResponse
			if stream {
				answer, err = g.StreamChat(ctx, req, func(string) error { return nil })
			} else {
				answer, err = g.Chat(ctx, req)
			}
			if err != nil || calls != 2 || answer.Content != "done" || !answer.UsageKnown || answer.Usage.PromptTokens != 9 {
				t.Fatalf("calls=%d answer=%+v error=%v", calls, answer, err)
			}
			retries, known := 0, 0
			for _, e := range events {
				if e.Type == "model_retry" {
					retries++
				}
				if e.Usage != nil {
					known++
				}
			}
			if retries != 1 || known != 1 || g.active.Load() != 0 || g.waiting.Load() != 0 {
				t.Fatalf("bad events or leaked slots: %+v", events)
			}
		})
	}
}
func TestGatewayRetryBoundAndNonRetryableFailures(t *testing.T) {
	for _, tc := range []struct{ status, want int }{{429, 3}, {502, 3}, {503, 3}, {504, 3}, {400, 1}, {401, 1}, {403, 1}, {500, 1}} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			calls := 0
			g, _ := NewGateway(retryProviderFunc(func(context.Context, *ChatRequest) (*ChatResponse, error) {
				calls++
				return nil, &UpstreamError{StatusCode: tc.status}
			}), time.Second)
			g.SetRetryPolicy(RetryPolicy{3, time.Millisecond})
			_, err := g.Chat(context.Background(), &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "test"}}})
			var upstream *UpstreamError
			if calls != tc.want || !errors.As(err, &upstream) || upstream.StatusCode != tc.status {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
	for _, callbackError := range []bool{false, true} {
		p := &retryStreamProvider{}
		g, _ := NewGateway(p, time.Second)
		_, err := g.StreamChat(context.Background(), &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "test"}}}, func(string) error {
			if callbackError {
				return &UpstreamError{StatusCode: 503}
			}
			return nil
		})
		if err == nil || p.calls != 1 {
			t.Fatal("replayed a partial stream or callback failure", p.calls, err)
		}
	}
}
func TestGatewayBackoffReleasesSlotsAndCancellation(t *testing.T) {
	var calls atomic.Int64
	retrying := make(chan struct{})
	g, _ := NewGateway(retryProviderFunc(func(context.Context, *ChatRequest) (*ChatResponse, error) {
		if calls.Add(1) == 1 {
			return nil, &UpstreamError{StatusCode: 503, RetryAfter: 100 * time.Millisecond}
		}
		return &ChatResponse{Content: "ok"}, nil
	}), time.Second)
	g.SetConcurrency(1)
	req := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "test"}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = WithObserver(ctx, func(e CallEvent) error {
		if e.Type == "model_retry" {
			close(retrying)
		}
		return nil
	})
	done := make(chan error, 1)
	go func() { _, e := g.Chat(ctx, req); done <- e }()
	select {
	case <-retrying:
	case <-time.After(time.Second):
		t.Fatal("no retry")
	}
	other, c := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer c()
	if _, err := g.Chat(other, req); err != nil {
		t.Fatal("backoff held the slot", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls.Load() != 2 || g.active.Load() != 0 || g.waiting.Load() != 0 {
		t.Fatal("unexpected calls or leaked slot")
	}
}
func TestRetryAfterBeyondDeadlineAndUnknownUsage(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	p, _ := NewOpenAIProvider(srv.URL, "test", "key")
	defer p.Close()
	g, _ := NewGateway(p, 50*time.Millisecond)
	_, err := g.Chat(context.Background(), &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "test"}}})
	var u *UpstreamError
	if calls != 1 || !errors.As(err, &u) || u.RetryAfter != 10*time.Second {
		t.Fatal(calls, err)
	}
	for _, body := range []string{`{"choices":[{"message":{"content":"ok"}}]}`, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		provider, _ := NewOpenAIProvider(s.URL, "test", "key")
		answer, e := provider.Chat(context.Background(), &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
		provider.Close()
		s.Close()
		if e != nil || answer.UsageKnown != strings.Contains(body, "usage") {
			t.Fatal(answer, e)
		}
	}
}

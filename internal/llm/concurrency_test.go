package llm

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type gatedProvider struct {
	calls   atomic.Int64
	release chan struct{}
}

func (p *gatedProvider) Chat(ctx context.Context, _ *ChatRequest) (*ChatResponse, error) {
	p.calls.Add(1)
	select {
	case <-p.release:
		return &ChatResponse{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func TestGatewaySharedBoundAndWaitCancellation(t *testing.T) {
	p := &gatedProvider{release: make(chan struct{})}
	g, _ := NewGateway(p, time.Second)
	g.SetConcurrency(1)
	req := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "mock"}}}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); g.Chat(context.Background(), req) }()
	deadline := time.After(time.Second)
	for p.calls.Load() != 1 {
		select {
		case <-deadline:
			t.Fatal("not started")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := g.Chat(ctx, req); err == nil {
		t.Fatal("waiting ignored cancellation")
	}
	if p.calls.Load() != 1 {
		t.Fatal("called upstream above bound")
	}
	close(p.release)
	wg.Wait()
	if g.active.Load() != 0 || g.waiting.Load() != 0 {
		t.Fatal("leaked semaphore")
	}
}

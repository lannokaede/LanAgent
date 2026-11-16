package llm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Admission tests hold real gateway requests at a deterministic provider seam.
// They establish resource bounds, not a throughput claim about a live model.
func TestGatewayBurstBoundsActiveAndWaitingWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &gatedProvider{release: make(chan struct{})}
	g, _ := NewGateway(p, 5*time.Second)
	g.SetConcurrency(2)
	if err := g.SetQueueLimit(4); err != nil {
		t.Fatal(err)
	}
	req := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "fixture"}}}
	var workers sync.WaitGroup
	t.Cleanup(func() { cancel(); workers.Wait() })
	for i := 0; i < 6; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); _, _ = g.Chat(ctx, req) }()
	}
	waitForAdmission(t, func() bool { return g.active.Load() == 2 && g.waiting.Load() == 4 })
	const burst = 64
	errorsOut := make(chan error, burst)
	var observed atomic.Int64
	burstCtx := WithObserver(ctx, func(CallEvent) error { observed.Add(1); return nil })
	for i := 0; i < burst; i++ {
		workers.Add(1)
		go func(stream bool) {
			defer workers.Done()
			var err error
			if stream {
				_, err = g.StreamChat(burstCtx, req, func(string) error { return nil })
			} else {
				_, err = g.Chat(burstCtx, req)
			}
			errorsOut <- err
		}(i%2 == 0)
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for i := 0; i < burst; i++ {
		select {
		case err := <-errorsOut:
			if !errors.Is(err, ErrOverloaded) {
				t.Fatalf("burst request returned %v, want local overload", err)
			}
		case <-deadline.C:
			t.Fatal("full waiting queue did not reject promptly")
		}
	}
	if p.calls.Load() != 2 || g.active.Load() != 2 || g.waiting.Load() != 4 || g.rejected.Load() != burst || observed.Load() != 0 {
		t.Fatalf("admission leaked work to the provider or attempt observer: calls=%d stats=%v events=%d", p.calls.Load(), g.Stats(), observed.Load())
	}
	cancel()
	workers.Wait()
	assertAdmissionReleased(t, g)
}

func TestGatewayQueuedCancellationReleasesWaitingPosition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &gatedProvider{release: make(chan struct{})}
	g, _ := NewGateway(p, 5*time.Second)
	g.SetConcurrency(1)
	if err := g.SetQueueLimit(1); err != nil {
		t.Fatal(err)
	}
	req := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "fixture"}}}
	active := make(chan error, 1)
	go func() { _, err := g.Chat(ctx, req); active <- err }()
	waitForAdmission(t, func() bool { return p.calls.Load() == 1 })
	queuedCtx, cancelQueued := context.WithCancel(ctx)
	defer cancelQueued()
	queued := make(chan error, 1)
	go func() { _, err := g.Chat(queuedCtx, req); queued <- err }()
	waitForAdmission(t, func() bool { return g.waiting.Load() == 1 })
	cancelQueued()
	if err := <-queued; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancellation returned %v", err)
	}
	if g.waiting.Load() != 0 || p.calls.Load() != 1 {
		t.Fatalf("cancellation retained a waiting position: %v", g.Stats())
	}
	next := make(chan error, 1)
	go func() { _, err := g.Chat(ctx, req); next <- err }()
	waitForAdmission(t, func() bool { return g.waiting.Load() == 1 })
	close(p.release)
	for _, done := range []<-chan error{active, next} {
		if err := <-done; err != nil {
			t.Fatalf("released queue could not serve work: %v", err)
		}
	}
	if p.calls.Load() != 2 {
		t.Fatalf("provider requests = %d, want 2", p.calls.Load())
	}
	assertAdmissionReleased(t, g)
}

func TestGatewayZeroQueueAndConfigurationValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &gatedProvider{release: make(chan struct{})}
	g, _ := NewGateway(p, time.Second)
	if cap(g.queue) != DefaultQueueLimit {
		t.Fatal("new gateways do not have a finite default waiting limit")
	}
	for _, n := range []int{-1, 1025} {
		if err := g.SetQueueLimit(n); err == nil {
			t.Fatalf("accepted invalid queue limit %d", n)
		}
	}
	if err := g.SetQueueLimit(0); err != nil {
		t.Fatal(err)
	}
	g.SetConcurrency(1)
	req := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "fixture"}}}
	done := make(chan error, 1)
	go func() { _, err := g.Chat(ctx, req); done <- err }()
	waitForAdmission(t, func() bool { return p.calls.Load() == 1 })
	if _, err := g.Chat(ctx, req); !errors.Is(err, ErrOverloaded) {
		t.Fatalf("zero queue should reject without waiting: %v", err)
	}
	if g.waiting.Load() != 0 || p.calls.Load() != 1 {
		t.Fatal("zero queue admitted or parked extra work")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertAdmissionReleased(t, g)
}

func TestGatewayRetryReacquisitionHonorsFullQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	retrying, continueRetry := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	g, _ := NewGateway(retryProviderFunc(func(ctx context.Context, _ *ChatRequest) (*ChatResponse, error) {
		if calls.Add(1) == 1 {
			return nil, &UpstreamError{StatusCode: 503}
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}), 5*time.Second)
	g.SetConcurrency(1)
	if err := g.SetQueueLimit(1); err != nil {
		t.Fatal(err)
	}
	if err := g.SetRetryPolicy(RetryPolicy{3, time.Nanosecond}); err != nil {
		t.Fatal(err)
	}
	req := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "fixture"}}}
	retryCtx := WithObserver(ctx, func(e CallEvent) error {
		if e.Type == "model_retry" {
			close(retrying)
			select {
			case <-continueRetry:
			case <-ctx.Done():
			}
		}
		return nil
	})
	retried := make(chan error, 1)
	go func() { _, err := g.Chat(retryCtx, req); retried <- err }()
	select {
	case <-retrying:
	case <-time.After(time.Second):
		t.Fatal("model did not reach retry")
	}
	var workers sync.WaitGroup
	t.Cleanup(func() { cancel(); workers.Wait() })
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); _, _ = g.Chat(ctx, req) }()
	}
	waitForAdmission(t, func() bool { return g.active.Load() == 1 && g.waiting.Load() == 1 })
	close(continueRetry)
	select {
	case err := <-retried:
		if !errors.Is(err, ErrOverloaded) {
			t.Fatalf("retry ignored queue admission: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retry did not reject a full queue")
	}
	if calls.Load() != 2 {
		t.Fatalf("retry bypassed admission: requests=%d", calls.Load())
	}
	cancel()
	workers.Wait()
	assertAdmissionReleased(t, g)
}

func TestGatewayRetryQueueKeepsTaskDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	retrying, continueRetry := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	g, _ := NewGateway(retryProviderFunc(func(ctx context.Context, _ *ChatRequest) (*ChatResponse, error) {
		if calls.Add(1) == 1 {
			return nil, &UpstreamError{StatusCode: 503}
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}), 2*time.Second)
	g.SetConcurrency(1)
	if err := g.SetQueueLimit(1); err != nil {
		t.Fatal(err)
	}
	if err := g.SetRetryPolicy(RetryPolicy{3, time.Nanosecond}); err != nil {
		t.Fatal(err)
	}
	taskCtx, stopTask := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stopTask()
	retryCtx := WithObserver(taskCtx, func(e CallEvent) error {
		if e.Type == "model_retry" {
			close(retrying)
			select {
			case <-continueRetry:
			case <-ctx.Done():
			}
		}
		return nil
	})
	req := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "fixture"}}}
	retried, blocker := make(chan error, 1), make(chan error, 1)
	go func() { _, err := g.Chat(retryCtx, req); retried <- err }()
	select {
	case <-retrying:
	case <-time.After(time.Second):
		t.Fatal("model did not reach retry")
	}
	go func() { _, err := g.Chat(ctx, req); blocker <- err }()
	waitForAdmission(t, func() bool { return calls.Load() == 2 })
	// Re-entering the queue must retain the task deadline, even though the model
	// timeout is longer. The blocking request has its own two-second budget.
	close(continueRetry)
	select {
	case err := <-retried:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("retry waiting deadline was lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retry queue wait outlived its model deadline")
	}
	if calls.Load() != 2 || g.waiting.Load() != 0 {
		t.Fatalf("expired retry entered provider or retained a waiting position: calls=%d stats=%v", calls.Load(), g.Stats())
	}
	cancel()
	<-blocker
	assertAdmissionReleased(t, g)
}

func waitForAdmission(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("gateway did not reach expected admission state")
		}
		time.Sleep(time.Millisecond)
	}
}

func assertAdmissionReleased(t *testing.T, g *Gateway) {
	t.Helper()
	if g.active.Load() != 0 || g.waiting.Load() != 0 || len(g.slots) != 0 || len(g.queue) != 0 {
		t.Fatalf("gateway retained concurrency or queue slots: %v", g.Stats())
	}
}

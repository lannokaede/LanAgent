package mcphttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testToken = "transport-test-token-1234567890"

type fixture struct {
	t            *testing.T
	server       *Server
	url, session string
	client       *http.Client
}

func newMCPServer() *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{Name: "bounded-test", Version: "1"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{"2025-06-18"}})
}
func newFixture(t *testing.T, opts Options, register func(*mcp.Server)) *fixture {
	t.Helper()
	m := newMCPServer()
	register(m)
	opts.Token = testToken
	if opts.MaxConcurrent == 0 {
		opts.MaxConcurrent = 1
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = 2 * time.Second
	}
	s, err := New(m, opts)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(func() { s.Close(); h.Close() })
	f := &fixture{t: t, server: s, url: h.URL, client: h.Client()}
	f.initialize()
	return f
}
func (f *fixture) request(ctx context.Context, method, body string) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, method, f.url+"/mcp", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("MCP-Protocol-Version", "2025-06-18")
	if f.session != "" {
		r.Header.Set("Mcp-Session-Id", f.session)
	}
	r.Header.Set(requestHeader, "client-forged-marker")
	return f.client.Do(r)
}
func (f *fixture) post(ctx context.Context, body string) (int, string, error) {
	r, err := f.request(ctx, http.MethodPost, body)
	if err != nil {
		return 0, "", err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	return r.StatusCode, string(b), err
}
func (f *fixture) initialize() {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, err := f.request(ctx, http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if err != nil {
		f.t.Fatal(err)
	}
	f.session = r.Header.Get("Mcp-Session-Id")
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil || r.StatusCode != 200 || f.session == "" {
		f.t.Fatalf("initialize: status=%d session=%q body=%s error=%v", r.StatusCode, f.session, body, err)
	}
	if code, body, err := f.post(ctx, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); err != nil || code != 202 {
		f.t.Fatalf("initialized: %d %s %v", code, body, err)
	}
}
func call(id int) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"work","arguments":{}}}`, id)
}
func waitFor(t *testing.T, channel <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(2 * time.Second):
		t.Fatal(description)
	}
}
func countContexts(s *Server) int {
	n := 0
	s.requests.Range(func(_, _ any) bool { n++; return true })
	return n
}

func TestRequestAdmissionRejectsOverloadAndSeparatesSSE(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var callbacks atomic.Int32
	f := newFixture(t, Options{}, func(m *mcp.Server) {
		mcp.AddTool(m, &mcp.Tool{Name: "work"}, func(ctx context.Context, r *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			if callbacks.Add(1) != 1 {
				return nil, nil, errors.New("unexpected concurrent callback")
			}
			if r.Extra.Header.Get(requestHeader) == "client-forged-marker" {
				t.Error("client controlled the transport association")
			}
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			return nil, map[string]bool{"ok": true}, nil
		})
	})
	t.Cleanup(unblock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// An idle GET stream occupies only the separate stream gate.
	stream, err := f.request(ctx, http.MethodGet, "")
	if err != nil || stream.StatusCode != 200 {
		t.Fatalf("GET SSE: %v %v", stream, err)
	}
	defer stream.Body.Close()
	secondStream, err := f.request(ctx, http.MethodGet, "")
	if err != nil {
		t.Fatal(err)
	}
	secondStream.Body.Close()
	if secondStream.StatusCode != 503 || secondStream.Header.Get("Retry-After") != "1" {
		t.Fatal("unbounded SSE admission", secondStream.StatusCode)
	}
	completed := make(chan error, 1)
	go func() {
		code, body, err := f.post(ctx, call(2))
		if err == nil && (code != 200 || !strings.Contains(body, `"ok":true`)) {
			err = fmt.Errorf("work response: %d %s", code, body)
		}
		completed <- err
	}()
	waitFor(t, started, "SSE consumed the POST/tool capacity")
	r, err := f.request(ctx, http.MethodPost, call(3))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 503 || r.Header.Get("Retry-After") != "1" {
		t.Fatal("overload was not refused", r.StatusCode)
	}
	if callbacks.Load() != 1 || countContexts(f.server) > 2 {
		t.Fatal("admission leaked capacity")
	}
	unblock()
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	stream.Body.Close()
	cancel()
}

func TestEmbeddedCloseCancelsIdleSSEBeforeJoiningHandlers(t *testing.T) {
	f := newFixture(t, Options{}, func(*mcp.Server) {})
	stream, err := f.request(context.Background(), http.MethodGet, "")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != 200 {
		t.Fatal("SSE did not open", stream.StatusCode)
	}
	closed := make(chan error, 1)
	go func() { closed <- f.server.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		// Unblock failure cleanup before reporting the regression.
		stream.Body.Close()
		t.Fatal("embedded Close waited for an SSE handler that it had not canceled")
	}
	if countContexts(f.server) != 0 {
		t.Fatal("embedded shutdown retained associations")
	}
	for range f.server.mcp.Sessions() {
		t.Fatal("embedded shutdown retained SDK sessions")
	}
}

func TestActualToolSlotSurvivesHTTPDisconnect(t *testing.T) {
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var callbacks atomic.Int32
	f := newFixture(t, Options{}, func(m *mcp.Server) {
		mcp.AddTool(m, &mcp.Tool{Name: "work"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			if callbacks.Add(1) != 1 {
				return nil, nil, errors.New("unexpected concurrent callback")
			}
			close(started)
			<-ctx.Done()
			close(canceled)
			<-release
			return nil, nil, ctx.Err()
		})
	})
	t.Cleanup(unblock)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { f.post(ctx, call(2)); close(finished) }()
	waitFor(t, started, "tool did not start")
	cancel()
	waitFor(t, canceled, "HTTP cancellation did not reach the SDK callback")
	waitFor(t, finished, "HTTP caller did not return")
	// Wait until the canceled HTTP handler has left its gate. The callback still
	// holds its own slot while performing controlled cancellation cleanup.
	deadline := time.After(time.Second)
	for {
		if countContexts(f.server) == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("canceled request association leaked")
		case <-time.After(time.Millisecond):
		}
	}
	code, body, err := f.post(context.Background(), call(3))
	if err != nil || code != 200 || !strings.Contains(body, "tool capacity reached") || callbacks.Load() != 1 {
		t.Fatalf("actual callback bound lost: %d %s %v callbacks=%d", code, body, err, callbacks.Load())
	}
	unblock()
}

func TestRequestDeadlineCancelsActualToolAndCleansAssociations(t *testing.T) {
	canceled := make(chan struct{})
	f := newFixture(t, Options{RequestTimeout: 80 * time.Millisecond}, func(m *mcp.Server) {
		mcp.AddTool(m, &mcp.Tool{Name: "work"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			<-ctx.Done()
			close(canceled)
			return nil, nil, ctx.Err()
		})
	})
	f.post(context.Background(), call(2))
	waitFor(t, canceled, "request deadline did not cancel the callback")
	for _, body := range []string{`{bad json`, `{"jsonrpc":"2.0","method":"notifications/initialized"}`} {
		f.post(context.Background(), body)
	}
	deadline := time.After(time.Second)
	for countContexts(f.server) != 0 {
		select {
		case <-deadline:
			t.Fatal("parse/notification/deadline association leaked")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestIdleSessionExpires(t *testing.T) {
	f := newFixture(t, Options{SessionTimeout: 40 * time.Millisecond}, func(*mcp.Server) {})
	deadline := time.After(time.Second)
	for {
		count := 0
		for range f.server.mcp.Sessions() {
			count++
		}
		if count == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("idle SDK session retained")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if code, _, err := f.post(context.Background(), `{"jsonrpc":"2.0","id":2,"method":"ping"}`); err != nil || code != 404 {
		t.Fatalf("expired session still accepted: %d %v", code, err)
	}
}

func ownedFixture(t *testing.T, opts Options, register func(*mcp.Server)) (*fixture, context.CancelFunc, <-chan error) {
	t.Helper()
	m := newMCPServer()
	register(m)
	opts.Token = testToken
	s, err := New(m, opts)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, listener) }()
	t.Cleanup(func() { cancel(); s.Close() })
	f := &fixture{t: t, server: s, url: "http://" + listener.Addr().String(), client: &http.Client{}}
	f.initialize()
	return f, cancel, done
}

func TestServeDrainsBeforeReturningAndClosesSessions(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var premature atomic.Bool
	f, cancel, done := ownedFixture(t, Options{MaxConcurrent: 1, ShutdownTimeout: time.Second}, func(m *mcp.Server) {
		mcp.AddTool(m, &mcp.Tool{Name: "work"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				premature.Store(true)
			}
			return nil, map[string]bool{"ok": true}, nil
		})
	})
	t.Cleanup(unblock)
	requestDone := make(chan struct{})
	go func() { f.post(context.Background(), call(2)); close(requestDone) }()
	waitFor(t, started, "tool did not start")
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Serve returned before drain: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	waitFor(t, requestDone, "drained request did not finish")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not finish draining")
	}
	if premature.Load() {
		t.Fatal("graceful drain canceled the active callback")
	}
	for range f.server.mcp.Sessions() {
		t.Fatal("SDK session survived Serve")
	}
	if err := f.server.Close(); err != nil {
		t.Fatal("Close was not idempotent", err)
	}
}

func TestServeDrainTimeoutCancelsAndJoinsCallbacks(t *testing.T) {
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	f, cancel, done := ownedFixture(t, Options{MaxConcurrent: 1, ShutdownTimeout: 40 * time.Millisecond}, func(m *mcp.Server) {
		mcp.AddTool(m, &mcp.Tool{Name: "work"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-release
			return nil, nil, ctx.Err()
		})
	})
	t.Cleanup(unblock)
	requestDone := make(chan struct{})
	go func() { f.post(context.Background(), call(2)); close(requestDone) }()
	waitFor(t, started, "tool did not start")
	cancel()
	waitFor(t, canceled, "drain timeout did not cancel active tool")
	select {
	case err := <-done:
		t.Fatalf("Serve returned while callback still uses dependencies: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	waitFor(t, requestDone, "forced HTTP close did not return")
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("drain timeout lost: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not join callbacks")
	}
	if countContexts(f.server) != 0 {
		t.Fatal("forced shutdown retained request contexts")
	}
	for range f.server.mcp.Sessions() {
		t.Fatal("forced shutdown retained an SDK session")
	}
}

func TestOptionsRejectInvalidPolicyBeforeModifyingServer(t *testing.T) {
	for _, opts := range []Options{{Token: "short"}, {Token: testToken, MaxConcurrent: -1}, {Token: testToken, MaxConcurrent: 1025}, {Token: testToken, RequestTimeout: -1}, {Token: testToken, SessionTimeout: -1}, {Token: testToken, ShutdownTimeout: -1}} {
		if server, err := New(newMCPServer(), opts); err == nil {
			server.Close()
			t.Fatal("invalid options accepted", opts)
		}
	}
	if _, err := New(nil, Options{Token: testToken}); err == nil {
		t.Fatal("nil MCP server accepted")
	}
}

func TestBurstKeepsFourCallbacksAndRejectsTwentyEightExcessRequests(t *testing.T) {
	const capacity, excess = 4, 28
	started := make(chan struct{}, capacity)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var active, peak atomic.Int32
	f := newFixture(t, Options{MaxConcurrent: capacity}, func(m *mcp.Server) {
		mcp.AddTool(m, &mcp.Tool{Name: "work"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for previous := peak.Load(); n > previous; previous = peak.Load() {
				if peak.CompareAndSwap(previous, n) {
					break
				}
			}
			started <- struct{}{}
			select {
			case <-release:
				return nil, map[string]bool{"ok": true}, nil
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		})
	})
	t.Cleanup(unblock)
	completed := make(chan error, capacity)
	for i := 0; i < capacity; i++ {
		go func(id int) {
			code, body, err := f.post(context.Background(), call(10+id))
			if err == nil && (code != 200 || !strings.Contains(body, `"ok":true`)) {
				err = fmt.Errorf("admitted request: status=%d body=%s", code, body)
			}
			completed <- err
		}(i)
	}
	for i := 0; i < capacity; i++ {
		waitFor(t, started, "admitted callback did not start")
	}
	// Flood concurrently while all admitted work is held. Every rejection must
	// complete before release, so excess load is refused rather than queued.
	var burst sync.WaitGroup
	burst.Add(excess)
	errorsSeen := make(chan error, excess)
	for i := 0; i < excess; i++ {
		go func(id int) {
			defer burst.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			code, body, err := f.post(ctx, call(100+id))
			if err == nil && code != 503 {
				err = fmt.Errorf("excess request: status=%d body=%s", code, body)
			}
			errorsSeen <- err
		}(i)
	}
	burst.Wait()
	for i := 0; i < excess; i++ {
		if err := <-errorsSeen; err != nil {
			t.Fatal(err)
		}
	}
	if got := peak.Load(); got != capacity {
		t.Fatalf("actual callback peak=%d, want %d", got, capacity)
	}
	if got := active.Load(); got != capacity {
		t.Fatalf("active callbacks=%d, want %d", got, capacity)
	}
	if got := countContexts(f.server); got != capacity {
		t.Fatalf("request contexts=%d, want %d", got, capacity)
	}
	unblock()
	for i := 0; i < capacity; i++ {
		if err := <-completed; err != nil {
			t.Fatal(err)
		}
	}
	if active.Load() != 0 {
		t.Fatal("callback slots were not released")
	}
}

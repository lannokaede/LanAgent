// Package mcphttp owns the bounded HTTP transport of one MCP server. A supplied
// MCP server belongs exclusively to this transport; do not also serve it over
// stdio or construct a second transport around the same server.
package mcphttp

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const requestHeader = "X-Lanagent-Transport-Request"

// Options controls per-process capacity, not a distributed rate limit. Zero
// capacity/durations select defaults (8 calls, 60s request, 5m session, 5s drain).
// GET SSE connections have a separate capacity equal to MaxConcurrent.
type Options struct {
	Token           string
	AllowedOrigins  []string
	MaxConcurrent   int
	RequestTimeout  time.Duration
	SessionTimeout  time.Duration
	ShutdownTimeout time.Duration
}

func (o *Options) defaults() error {
	if o.MaxConcurrent == 0 {
		o.MaxConcurrent = 8
	}
	if o.RequestTimeout == 0 {
		o.RequestTimeout = time.Minute
	}
	if o.SessionTimeout == 0 {
		o.SessionTimeout = 5 * time.Minute
	}
	if o.ShutdownTimeout == 0 {
		o.ShutdownTimeout = 5 * time.Second
	}
	if len(o.Token) < 24 || o.MaxConcurrent < 1 || o.MaxConcurrent > 1024 || o.RequestTimeout <= 0 || o.SessionTimeout <= 0 || o.ShutdownTimeout <= 0 {
		return errors.New("HTTP transport requires a 24-character token, capacity 1..1024 and positive timeouts")
	}
	return nil
}

// Server implements http.Handler for embedding/tests and Serve for owned
// listeners. Close cancels and joins requests, MCP sessions, and tool callbacks.
// Tools must honor context cancellation: Go cannot forcibly stop an arbitrary
// callback that ignores its context, and Close waits rather than close its data
// dependencies while such a callback might still use them.
type Server struct {
	options                           Options
	mcp                               *mcp.Server
	endpoint                          http.Handler
	http                              *http.Server
	ctx                               context.Context
	cancel                            context.CancelFunc
	postSlots, streamSlots, toolSlots chan struct{}
	mu                                sync.Mutex
	closing                           bool
	handlers, tools                   sync.WaitGroup
	requests                          sync.Map // at most 2*MaxConcurrent entries; removed when HTTP returns
	nextRequest                       atomic.Uint64
	started                           atomic.Bool
	closeOnce                         sync.Once
	closeErr                          error
}

func New(server *mcp.Server, opts Options) (*Server, error) {
	if server == nil {
		return nil, errors.New("MCP server is required")
	}
	if err := opts.defaults(); err != nil {
		return nil, err
	}
	opts.AllowedOrigins = append([]string(nil), opts.AllowedOrigins...)
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{options: opts, mcp: server, ctx: ctx, cancel: cancel,
		postSlots: make(chan struct{}, opts.MaxConcurrent), streamSlots: make(chan struct{}, opts.MaxConcurrent), toolSlots: make(chan struct{}, opts.MaxConcurrent)}
	s.endpoint = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{SessionTimeout: opts.SessionTimeout, MaxRequestBodyBytes: 128 << 10})
	server.AddReceivingMiddleware(s.boundTools)
	s.http = &http.Server{Handler: s, BaseContext: func(net.Listener) context.Context { return s.ctx }, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: opts.RequestTimeout, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 16 << 10}
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		allowed := false
		for _, candidate := range s.options.AllowedOrigins {
			allowed = allowed || (candidate != "" && candidate == origin)
		}
		if !allowed {
			http.Error(w, "origin refused", http.StatusForbidden)
			return
		}
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.options.Token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	slots := s.postSlots
	if r.Method == http.MethodGet {
		slots = s.streamSlots
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		overloaded(w)
		return
	}
	select {
	case slots <- struct{}{}:
		s.handlers.Add(1)
		s.mu.Unlock()
	default:
		s.mu.Unlock()
		overloaded(w)
		return
	}
	defer func() { <-slots; s.handlers.Done() }()
	// Header values, unlike a session's detached context, are attached to each
	// SDK request. Always overwrite the client value, and never echo this marker.
	// The association stays bounded by admitted HTTP requests, including parse
	// failures and notifications, whose deferred cleanup follows the same path.
	ctx, cancelRequest := context.WithCancel(r.Context())
	stopServer := context.AfterFunc(s.ctx, cancelRequest)
	defer stopServer()
	defer cancelRequest()
	if r.Method != http.MethodGet {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.options.RequestTimeout)
		defer cancel()
	}
	r = r.Clone(ctx)
	id := strconv.FormatUint(s.nextRequest.Add(1), 10)
	r.Header.Set(requestHeader, id)
	s.requests.Store(id, ctx)
	defer s.requests.Delete(id)
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	s.endpoint.ServeHTTP(w, r)
}

func overloaded(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, "server busy; retry later", http.StatusServiceUnavailable)
}

// MCP stateful sessions detach from their HTTP request's cancellation. Limit
// actual callbacks as well as POST handlers, retaining a tool slot until the
// callback returns even when the client has already disconnected.
func (s *Server) boundTools(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, request)
		}
		extra := request.GetExtra()
		if extra == nil {
			return nil, errors.New("missing HTTP request context")
		}
		value, ok := s.requests.Load(extra.Header.Get(requestHeader))
		if !ok {
			return nil, context.Canceled
		}
		call, cancel := context.WithTimeout(ctx, s.options.RequestTimeout)
		defer cancel()
		stopRequest := context.AfterFunc(value.(context.Context), cancel)
		defer stopRequest()
		stopServer := context.AfterFunc(s.ctx, cancel)
		defer stopServer()
		if err := value.(context.Context).Err(); err != nil {
			return nil, err
		}
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			return nil, errors.New("server is shutting down")
		}
		select {
		case s.toolSlots <- struct{}{}:
			s.tools.Add(1)
			s.mu.Unlock()
		default:
			s.mu.Unlock()
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "server busy: tool capacity reached; retry later"}}}, nil
		}
		defer func() { <-s.toolSlots; s.tools.Done() }()
		if err := call.Err(); err != nil {
			return nil, err
		}
		return next(call, method, request)
	}
}

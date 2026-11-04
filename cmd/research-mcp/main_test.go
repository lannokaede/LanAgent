package main

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPAuthenticationAndOrigin(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	handler := httpHandler(server, "test-token-long-enough-12345", []string{"https://allowed.example"})
	for _, tc := range []struct {
		auth, origin string
		want         int
	}{{"", "", 401}, {"Bearer test-token-long-enough-12345", "https://evil.example", 403}, {"wrong", "https://allowed.example", 401}} {
		r := httptest.NewRequest("POST", "http://localhost/mcp", strings.NewReader(`{}`))
		r.Header.Set("Authorization", tc.auth)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatal(w.Code, tc.want)
		}
	}
}
func TestInvalidModesDoNotCreateStore(t *testing.T) {
	for _, args := range [][]string{{"-allow-knowledge-write"}, {"-init-store"}, {"-web", "unknown"}} {
		if e := run(context.Background(), args, io.Discard); e == nil {
			t.Fatal("invalid flags accepted", args)
		}
	}
}

type authRoundTripper struct{ token string }

func (a authRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+a.token)
	return http.DefaultTransport.RoundTrip(r)
}
func TestStreamableHTTPHandshakeAndTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := mcp.NewServer(&mcp.Implementation{Name: "http-test", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "ping"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return nil, map[string]any{"status": "pong"}, nil
	})
	httpServer := httptest.NewServer(httpHandler(server, "long-test-token-1234567890", nil))
	defer httpServer.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1"}, nil)
	session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpServer.URL + "/mcp", HTTPClient: &http.Client{Transport: authRoundTripper{"long-test-token-1234567890"}}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "ping", Arguments: map[string]any{}})
	if e != nil || result.IsError {
		t.Fatal(result, e)
	}
}

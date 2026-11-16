package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOTLPEndpointPreservesBaseURLAndExplicitPaths(t *testing.T) {
	for _, tc := range []struct {
		name, suffix, want string
	}{
		{"base URL", "", "/v1/traces"},
		{"explicit collector path", "/collector/traces", "/collector/traces"},
		{"explicit root", "/", "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("OTLP method = %s, want POST", r.Method)
				}
				io.Copy(io.Discard, r.Body)
				requests <- r.URL.Path
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			shutdown, err := Init(ctx, Config{Enabled: true, Endpoint: server.URL + tc.suffix})
			if err != nil {
				t.Fatal(err)
			}
			defer shutdown(context.Background())
			_, span := Start(ctx, "collector-compatibility")
			span.End()
			if err := shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-requests:
				if got != tc.want {
					t.Fatalf("OTLP request path = %q, want %q", got, tc.want)
				}
			case <-ctx.Done():
				t.Fatal("collector did not receive the exported span")
			}
		})
	}
}

func TestOTLPInvalidEndpointDoesNotExposeURL(t *testing.T) {
	_, err := Init(context.Background(), Config{Enabled: true, Endpoint: "http://collector.invalid/%invalid-secret"})
	if err == nil || err.Error() != "invalid telemetry endpoint URL" {
		t.Fatalf("unexpected endpoint validation error: %v", err)
	}
}

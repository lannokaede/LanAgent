package research

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFirecrawlNarrowPayloadAndRedactedErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("missing auth")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid body")
		}
		for _, key := range []string{"alexandria", "actions", "headers"} {
			if _, ok := body[key]; ok {
				t.Errorf("wide capability %s", key)
			}
		}
		if strings.HasSuffix(r.URL.Path, "search") {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"web": []Page{{URL: "https://go.dev/blog/context", Title: "Context"}}}})
			return
		}
		if body["url"] == "https://go.dev/error" {
			http.Error(w, "test-secret", 402)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"markdown": "# Context\nCancellation", "metadata": map[string]any{"title": "Context", "sourceURL": "https://go.dev/blog/context", "statusCode": 200}}})
	}))
	defer server.Close()
	provider := NewFirecrawl("test-secret")
	provider.baseURL = server.URL
	pages, e := provider.Search(context.Background(), "context", 2)
	if e != nil || len(pages) != 1 {
		t.Fatal(pages, e)
	}
	p, e := provider.Fetch(context.Background(), "https://go.dev/blog/context")
	if e != nil || !strings.Contains(p.Text, "Cancellation") {
		t.Fatal(p, e)
	}
	_, e = provider.Fetch(context.Background(), "https://go.dev/error")
	if e == nil || strings.Contains(e.Error(), "test-secret") {
		t.Fatal("secret leaked", e)
	}
}

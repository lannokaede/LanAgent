package corpus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCrawlerHonorsRobotsKeepsCodeAndRecordsPartialFailure(t *testing.T) {
	requests := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests[r.URL.Path]++
		switch r.URL.Path {
		case "/robots.txt":
			_, _ = w.Write([]byte("User-agent: *\nDisallow: /private\nAllow: /private/public\n"))
		case "/doc":
			_, _ = w.Write([]byte("<html><title>Concurrency</title><nav>outside navigation</nav><main><nav>inside navigation</nav><p>" + strings.Repeat("goroutine channels ", 30) + "</p><pre>if x &lt; 2 {\n  return x\n}</pre><script>untrusted_script()</script></main></html>"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	c := &Crawler{Client: server.Client(), allowedHosts: map[string]bool{u.Host: true}}
	dir := filepath.Join(t.TempDir(), "corpus")
	m, err := c.Crawl(context.Background(), []Source{{"good", "go", server.URL + "/doc"}, {"blocked", "go", server.URL + "/private/secret"}}, dir)
	if err == nil || len(m.Documents) != 2 || m.Documents[1].Error == "" {
		t.Fatalf("failed to record rejected source: %+v %v", m, err)
	}
	if requests["/private/secret"] != 0 || requests["/robots.txt"] != 1 {
		t.Fatalf("robots ignored: %v", requests)
	}
	data, err := os.ReadFile(filepath.Join(dir, "good.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "navigation") || strings.Contains(text, "untrusted_script") || !strings.Contains(text, "if x < 2 {\n  return x") {
		t.Fatalf("bad extraction: %s", text)
	}
	if _, err := c.Crawl(context.Background(), Sources(), dir); err == nil {
		t.Fatal("overwrote corpus")
	}
}

func TestRobotsAgentPriorityAndWildcards(t *testing.T) {
	rules := "User-agent: *\nDisallow: /\n\nUser-agent: LanAgentInterviewBot\nDisallow: /secret*\nAllow: /secret/public$\n"
	for path, want := range map[string]bool{"/doc": true, "/secret/a": false, "/secret/public": true, "/secret/public/child": false} {
		if got := robotsAllowed(rules, path); got != want {
			t.Fatalf("%s: %v", path, got)
		}
	}
}

func TestCrawlerRefusesCrossOriginRedirect(t *testing.T) {
	otherHits := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherHits++ }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) }))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	c := &Crawler{Client: server.Client(), allowedHosts: map[string]bool{u.Host: true}}
	if _, _, err := c.get(context.Background(), server.URL); err == nil || otherHits != 0 {
		t.Fatal("followed unrelated redirect")
	}
}

package research

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/lannokaede/LanAgent/internal/corpus"
)

func TestSiteIndexSearchPersistenceAndIncrementalUpdate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	i, e := OpenSiteIndex(dir)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = i.Search(ctx, "context", 5); e == nil || !strings.Contains(e.Error(), "index refresh") {
		t.Fatal(e)
	}
	pages := []Page{
		{URL: "https://go.dev/doc/a", Title: "Context cancellation", Text: "goroutines stop using context cancellation"},
		{URL: "https://go.dev/doc/b", Title: "Other topic", Text: "context cancellation"},
		{URL: "https://go.dev/doc/c", Title: "并发编程指南", Text: "上下文取消与协程调度，事务隔离级别"},
	}
	for _, p := range pages {
		if e = i.Put(ctx, p); e != nil {
			t.Fatal(e)
		}
	}
	for _, q := range []string{"context", "并发编程", "事务", "取消", "go OR NOT \"*"} {
		if _, e = i.Search(ctx, q, 5); e != nil {
			t.Fatalf("%s: %v", q, e)
		}
	}
	matches, e := i.Search(ctx, "context", 5)
	if e != nil || len(matches) != 2 || matches[0].URL != pages[0].URL || matches[0].FetchedAt == "" || matches[0].Origin != "local_site_index" {
		t.Fatalf("%+v %v", matches, e)
	}
	for _, q := range []string{"并发编程", "事务"} {
		m, e := i.Search(ctx, q, 5)
		if e != nil || len(m) != 1 || m[0].URL != pages[2].URL {
			t.Fatal(q, m, e)
		}
	}
	if m, e := i.Search(ctx, "nonexistent", 5); e != nil || len(m) != 0 {
		t.Fatal(m, e)
	}
	if e = i.Put(ctx, pages[0]); e != nil {
		t.Fatal(e)
	}
	var count int
	if e = i.db.QueryRow("SELECT count(*) FROM words").Scan(&count); e != nil || count != 3 {
		t.Fatal(count, e)
	}
	pages[0].Title = "Updated guide"
	pages[0].Text = "replacement zebra content"
	if e = i.Put(ctx, pages[0]); e != nil {
		t.Fatal(e)
	}
	if e = i.Failure(ctx, pages[0].URL, errors.New("temporary HTTP 503")); e != nil {
		t.Fatal(e)
	}
	if e = i.Close(); e != nil {
		t.Fatal(e)
	}
	i, e = OpenSiteIndex(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer i.Close()
	if m, e := i.Search(ctx, "zebra", 5); e != nil || len(m) != 1 {
		t.Fatal(m, e)
	}
	if m, e := i.Search(ctx, "context", 5); e != nil || len(m) != 1 {
		t.Fatal("stale FTS rows", m, e)
	}
	var msg string
	if e = i.db.QueryRow("SELECT error FROM attempts WHERE url=?", pages[0].URL).Scan(&msg); e != nil || !strings.Contains(msg, "503") {
		t.Fatal(msg, e)
	}
}

type rewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c := r.Clone(r.Context())
	u := *r.URL
	u.Scheme = t.target.Scheme
	u.Host = t.target.Host
	c.URL = &u
	return t.base.RoundTrip(c)
}
func fixtureCrawler(server *httptest.Server) *corpus.Crawler {
	target, _ := url.Parse(server.URL)
	c := corpus.NewCrawler()
	c.Delay = 0
	c.Client = &http.Client{Transport: rewriteTransport{target, server.Client().Transport}}
	return c
}
func TestRefreshDiscoveryLimitsFailureAndCancellation(t *testing.T) {
	hits := map[string]int{}
	version := "firstversion"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		switch r.URL.Path {
		case "/robots.txt":
			w.Write([]byte("User-agent: *\nDisallow: /docs/private\n"))
		case "/docs/root":
			w.Write([]byte(`<title>Root</title><main>` + strings.Repeat(version+" ", 30) + `<a href="/docs/child">child</a><a href="/docs/child#x">duplicate</a><a href="/docs/private">blocked</a><a href="/docs/fail">fail</a><a href="https://evil.invalid/docs/x">outside</a></main>`))
		case "/docs/child":
			w.Write([]byte(`<title>Child</title><main>` + strings.Repeat("childcontent ", 30) + `<a href="/docs/grandchild">grandchild</a></main>`))
		default:
			http.Error(w, "unavailable", 503)
		}
	}))
	defer server.Close()
	i, e := OpenSiteIndex(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer i.Close()
	ctx := context.Background()
	report, e := i.refresh(ctx, fixtureCrawler(server), []string{"https://go.dev/docs/root"}, 1, 2)
	if e != nil || report.Attempted != 1 || report.Indexed != 1 || hits["/docs/child"] != 0 {
		t.Fatal(report, hits, e)
	}
	report, e = i.refresh(ctx, fixtureCrawler(server), []string{"https://go.dev/docs/root"}, 10, 1)
	if e == nil || report.Attempted != 4 || report.Indexed != 2 || hits["/docs/private"] != 0 || hits["/docs/grandchild"] != 0 || hits["/docs/child"] != 1 {
		t.Fatal(report, hits, e)
	}
	version = "changedversion"
	if _, e = i.refresh(ctx, fixtureCrawler(server), []string{"https://go.dev/docs/root"}, 1, 0); e != nil {
		t.Fatal(e)
	}
	if m, e := i.Search(ctx, "changedversion", 5); e != nil || len(m) != 1 {
		t.Fatal(m, e)
	}
	if m, e := i.Search(ctx, "firstversion", 5); e != nil || len(m) != 0 {
		t.Fatal(m, e)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = i.refresh(cancelled, fixtureCrawler(server), []string{"https://go.dev/docs/root"}, 5, 1); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if m, e := i.Search(ctx, "childcontent", 5); e != nil || len(m) != 1 {
		t.Fatal("completed page lost", m, e)
	}
}

func TestSiteIndexConcurrentRefreshAndFetchConnections(t *testing.T) {
	dir := t.TempDir()
	a, e := OpenSiteIndex(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	b, e := OpenSiteIndex(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	ctx := context.Background()
	done := make(chan error, 2)
	for _, index := range []*SiteIndex{a, b} {
		go func(i *SiteIndex) {
			for k := 0; k < 10; k++ {
				if e := i.Put(ctx, Page{URL: "https://go.dev/docs/shared", Title: "Concurrent context", Text: strings.Repeat("context ", k+1)}); e != nil {
					done <- e
					return
				}
				if _, e := i.Search(ctx, "context", 5); e != nil {
					done <- e
					return
				}
			}
			done <- nil
		}(index)
	}
	for range 2 {
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}
	var count int
	if e := a.db.QueryRow("SELECT count(*) FROM pages").Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
}

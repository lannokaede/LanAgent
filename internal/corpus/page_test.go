package corpus

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestFetchPageRobotsRedirectBaseAndPacing(t *testing.T) {
	requests := map[string]int{}
	var times []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests[r.URL.Path]++
		times = append(times, time.Now())
		switch r.URL.Path {
		case "/robots.txt":
			w.Write([]byte("User-agent: *\nDisallow: /private\n"))
		case "/start":
			http.Redirect(w, r, "/docs/final/", 302)
		case "/blocked":
			http.Redirect(w, r, "/private", 302)
		case "/docs/final/":
			w.Write([]byte("<title>Docs</title><main>" + strings.Repeat("documentation ", 30) + `<a href="child">child</a></main>`))
		default:
			t.Error("unexpected request", r.URL.Path)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := &Crawler{Client: srv.Client(), Delay: 15 * time.Millisecond, allowedHosts: map[string]bool{u.Host: true}}
	p, e := c.FetchPage(context.Background(), srv.URL+"/start")
	if e != nil || p.URL != srv.URL+"/docs/final/" || len(p.Links) != 1 || p.Links[0] != srv.URL+"/docs/final/child" {
		t.Fatal(p, e)
	}
	if _, e = c.FetchPage(context.Background(), srv.URL+"/blocked"); e == nil || requests["/private"] != 0 {
		t.Fatal(requests, e)
	}
	if requests["/robots.txt"] != 1 {
		t.Fatal(requests)
	}
	for k := 1; k < len(times); k++ {
		if times[k].Sub(times[k-1]) < 12*time.Millisecond {
			t.Fatal("request pacing bypassed", times)
		}
	}
}
func TestPublicTransportRejectsLocalAddresses(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "::1", "10.0.0.1", "169.254.169.254", "100.100.100.200", "192.0.2.1", "fc00::1", "::ffff:127.0.0.1"} {
		if PublicIP(net.ParseIP(raw)) {
			t.Fatal(raw)
		}
	}
	if !PublicIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("public address rejected")
	}
	c := &http.Client{Transport: PublicTransport()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:9", nil)
	if _, e := c.Do(req); e == nil || !strings.Contains(e.Error(), "non-public") {
		t.Fatal(e)
	}
}

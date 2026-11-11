package corpus

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// PublicTransport checks the resolved address at connection time, including
// redirect connections. It deliberately does not use environment HTTP proxies.
func PublicTransport() *http.Transport {
	return &http.Transport{TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("source has no addresses")
		}
		for _, a := range ips {
			if !PublicIP(a.IP) {
				return nil, fmt.Errorf("source resolved to non-public address")
			}
		}
		d := net.Dialer{Timeout: 10 * time.Second}
		var last error
		for _, a := range ips {
			c, e := d.DialContext(ctx, network, net.JoinHostPort(a.IP.String(), port))
			if e == nil {
				return c, nil
			}
			last = e
		}
		return nil, last
	}}
}

func PublicIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, raw := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "2001::/32", "2002::/16", "64:ff9b::/96"} {
		_, n, _ := net.ParseCIDR(raw)
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

type Page struct {
	URL, Title, Text string
	Links            []string
}

// FetchPage shares the existing official allowlist, extractor and robots parser.
// A crawler instance retains per-origin robots rules and request pacing.
func (c *Crawler) FetchPage(ctx context.Context, raw string) (Page, error) {
	u, e := url.Parse(raw)
	if e != nil || !c.allowed(u) || u.User != nil {
		return Page{}, fmt.Errorf("URL outside official source allowlist")
	}
	origin := u.Scheme + "://" + u.Host
	if c.pageRobots == nil {
		c.pageRobots = map[string]string{}
	}
	if c.pageLast == nil {
		c.pageLast = map[string]time.Time{}
	}
	get := func(target string) ([]byte, int, error) {
		delay := c.Delay - time.Since(c.pageLast[origin])
		if delay > 0 {
			t := time.NewTimer(delay)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-t.C:
			}
		}
		c.pageLast[origin] = time.Now()
		return c.get(ctx, target)
	}
	rules, ok := c.pageRobots[origin]
	if !ok {
		b, status, e := get(origin + "/robots.txt")
		if e != nil {
			return Page{}, e
		}
		if status != 200 && status != 404 {
			return Page{}, fmt.Errorf("robots returned HTTP %d", status)
		}
		if status == 200 {
			rules = string(b)
		}
		c.pageRobots[origin] = rules
	}
	if !robotsAllowed(rules, u.EscapedPath()) {
		return Page{}, fmt.Errorf("robots disallows source")
	}
	b, status, e := get(raw)
	if e != nil {
		return Page{}, e
	}
	if status != 200 {
		return Page{}, fmt.Errorf("source returned HTTP %d", status)
	}
	if final, err := url.Parse(c.pageFinal); err == nil && final.Host == u.Host {
		u = final
	}
	title, text, e := Extract(b)
	if e != nil {
		return Page{}, e
	}
	root, e := html.Parse(strings.NewReader(string(b)))
	if e != nil {
		return Page{}, e
	}
	// Resolve relative links against the final same-origin response URL.
	p := Page{URL: u.String(), Title: title, Text: text}
	seen := map[string]bool{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, a := range n.Attr {
				if a.Key == "href" {
					ref, e := url.Parse(a.Val)
					if e != nil {
						continue
					}
					v := u.ResolveReference(ref)
					v.Fragment = ""
					if v.Host != u.Host || !c.allowed(v) || v.User != nil || v.RawQuery != "" {
						continue
					}
					path := strings.ToLower(v.Path)
					if !(strings.HasSuffix(path, "/") || !strings.Contains(path[strings.LastIndex(path, "/")+1:], ".") || strings.HasSuffix(path, ".html")) {
						continue
					}
					key := v.String()
					if !seen[key] && len(p.Links) < 1000 {
						seen[key] = true
						p.Links = append(p.Links, key)
					}
				}
			}
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(root)
	return p, nil
}

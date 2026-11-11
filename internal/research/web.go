package research

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Firecrawl exposes only search and Markdown scrape. Model arguments cannot
// select actions, Alexandria, credentials, arbitrary API paths or headers.
type Firecrawl struct {
	Key     string
	Client  *http.Client
	baseURL string
}

func NewFirecrawl(key string) *Firecrawl {
	return &Firecrawl{Key: key, baseURL: "https://api.firecrawl.dev/v2", Client: &http.Client{Timeout: 55 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("provider redirect refused") }}}
}
func (f *Firecrawl) post(ctx context.Context, route string, in, out any) error {
	b, e := json.Marshal(in)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, f.baseURL+route, bytes.NewReader(b))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	if f.Key != "" {
		req.Header.Set("Authorization", "Bearer "+f.Key)
	}
	resp, e := f.Client.Do(req)
	if e != nil {
		return errors.New("web provider request failed or cancelled")
	}
	defer resp.Body.Close()
	b, e = io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if e != nil {
		return errors.New("web provider response incomplete")
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("web provider HTTP %d; check provider key or quota", resp.StatusCode)
	}
	if len(b) > 8<<20 {
		return errors.New("web provider response too large")
	}
	var envelope struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(b, &envelope) != nil || !envelope.Success {
		return errors.New("web provider rejected request")
	}
	if json.Unmarshal(envelope.Data, out) != nil {
		return errors.New("invalid web provider payload")
	}
	return nil
}
func (f *Firecrawl) Search(ctx context.Context, q string, n int) ([]Page, error) {
	var out struct {
		Web []struct {
			URL         string `json:"url"`
			Title       string `json:"title"`
			Description string `json:"description"`
		} `json:"web"`
	}
	e := f.post(ctx, "/search", map[string]any{"query": q, "limit": n, "sources": []map[string]string{{"type": "web"}}, "timeout": 45000}, &out)
	if e != nil {
		return nil, e
	}
	pages := []Page{}
	for _, x := range out.Web {
		pages = append(pages, Page{URL: x.URL, Title: x.Title, Description: x.Description})
	}
	return pages, nil
}
func (f *Firecrawl) Fetch(ctx context.Context, u string) (Page, error) {
	var out struct {
		Markdown string `json:"markdown"`
		Metadata struct {
			Title      string `json:"title"`
			URL        string `json:"sourceURL"`
			StatusCode int    `json:"statusCode"`
		} `json:"metadata"`
	}
	if e := f.post(ctx, "/scrape", map[string]any{"url": u, "formats": []string{"markdown"}, "onlyMainContent": true, "timeout": 45000}, &out); e != nil {
		return Page{}, e
	}
	if out.Metadata.StatusCode >= 400 {
		return Page{}, errors.New("upstream page rejected or unavailable")
	}
	if out.Metadata.URL == "" {
		out.Metadata.URL = u
	}
	return Page{URL: out.Metadata.URL, Title: out.Metadata.Title, Text: out.Markdown}, nil
}
func officialHost(h string) bool {
	return h == "go.dev" || h == "redis.io" || h == "www.postgresql.org"
}
func (s *Service) canonical(raw string) (string, error) {
	if len(raw) > 2048 {
		return "", errors.New("URL too long")
	}
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Port() != "" {
		return "", errors.New("use a public HTTPS source URL without credentials or custom ports")
	}
	host := strings.ToLower(u.Hostname())
	if net.ParseIP(host) != nil || host == "localhost" {
		return "", errors.New("source address not allowed")
	}
	allowed := false
	for _, h := range s.opts.AllowedHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" && (host == h || strings.HasSuffix(host, "."+h)) {
			allowed = true
		}
	}
	if !allowed {
		return "", errors.New("source host outside configured allowlist")
	}
	u.Host = host
	u.Fragment = ""
	q := u.Query()
	for k := range q {
		if strings.HasPrefix(strings.ToLower(k), "utm_") || k == "gclid" || k == "fbclid" {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}
func (s *Service) SearchWeb(ctx context.Context, q string, n int) ([]Page, error) {
	if strings.TrimSpace(q) == "" || utf8.RuneCountInString(q) > 1024 || n < 1 || n > 5 {
		return nil, errors.New("query requires 1..1024 characters, limit 1..5")
	}
	if s.opts.Web == nil {
		pages, e := s.siteIndex.Search(ctx, q, 5)
		if e != nil {
			return nil, e
		}
		out := []Page{}
		for _, p := range pages {
			u, e := s.canonical(p.URL)
			if e != nil {
				continue
			}
			parsed, _ := url.Parse(u)
			if !officialHost(parsed.Hostname()) {
				continue
			}
			out = append(out, p)
			if len(out) == n {
				break
			}
		}
		return out, nil
	}
	if e := s.reserveWeb(false); e != nil {
		return nil, e
	}
	domains := []string{}
	for _, host := range s.opts.AllowedHosts {
		domains = append(domains, "site:"+host)
	}
	q += " (" + strings.Join(domains, " OR ") + ")"
	pages, e := s.opts.Web.Search(ctx, q, n)
	if e != nil {
		return nil, e
	}
	out := []Page{}
	for _, p := range pages {
		u, e := s.canonical(p.URL)
		if e != nil {
			continue
		}
		p.URL = u
		p.Text = ""
		p.Title = preview(p.Title, 512)
		p.Description = preview(p.Description, 2048)
		p.PublishedAt = ""
		out = append(out, p)
		if len(out) == n {
			break
		}
	}
	return out, nil
}
func (s *Service) Fetch(ctx context.Context, raw string) (Source, error) {
	u, e := s.canonical(raw)
	if e != nil {
		return Source{}, e
	}
	if e := s.reserveWeb(true); e != nil {
		return Source{}, e
	}
	var p Page
	if s.opts.Web != nil {
		p, e = s.opts.Web.Fetch(ctx, u)
	} else {
		parsed, _ := url.Parse(u)
		if !officialHost(parsed.Hostname()) {
			return Source{}, errors.New("non-official pages require configured web provider")
		}
		s.fetchMu.Lock()
		page, fetchErr := s.crawler.FetchPage(ctx, u)
		s.fetchMu.Unlock()
		e = fetchErr
		p = Page{URL: page.URL, Title: page.Title, Text: page.Text}
		if e != nil {
			_ = s.siteIndex.Failure(ctx, u, e)
		}

	}
	if e != nil {
		return Source{}, e
	}
	final, e := s.canonical(p.URL)
	if e != nil {
		return Source{}, errors.New("provider returned URL outside source policy")
	}
	text := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(p.Text, "\ufeff"), "\r\n", "\n"), "\r", "\n"))
	if len(text) == 0 || len(text) > MaxBody || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return Source{}, errors.New("page must contain bounded UTF-8 text")
	}
	title := preview(strings.TrimSpace(p.Title), 512)
	body := []byte("# " + title + "\n\n" + text)
	if len(body) > MaxBody {
		return Source{}, errors.New("page exceeds artifact limit")
	}
	parsed, _ := url.Parse(final)
	kind := "interview_experience"
	if officialHost(parsed.Hostname()) {
		kind = "official"
	}
	src := Source{ID: hash([]byte(final)), URL: final, Title: title, Kind: kind, Hash: hash(body), FetchedAt: now()}
	src.ArtifactID = hashJSON([]string{src.ID, src.Hash})
	s.artifactMu.Lock()
	e = s.atomic("artifacts/"+src.ArtifactID+".md", body)
	s.artifactMu.Unlock()
	if e != nil {
		return Source{}, e
	}
	e = s.update(func(st *state) error {
		if len(st.Artifacts) >= 4096 {
			if _, ok := st.Artifacts[src.ArtifactID]; !ok {
				return errors.New("artifact catalog capacity reached")
			}
		}
		old := st.Sources[src.ID]
		src.DocumentID = old.DocumentID
		src.IndexedRevision = old.IndexedRevision
		src.IndexedArtifact = old.IndexedArtifact
		st.Artifacts[src.ArtifactID] = src
		st.Sources[src.ID] = src
		return nil
	})
	if e == nil && kind == "official" {
		e = s.siteIndex.Put(ctx, Page{URL: final, Title: title, Text: text})
	}
	return src, e
}
func preview(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// Reserve before sending: failed and interrupted requests may still be billed.
func (s *Service) reserveWeb(fetch bool) error {
	return s.update(func(st *state) error {
		if fetch {
			if st.FetchCalls >= s.opts.MaxFetchCalls {
				return errors.New("persistent fetch budget exhausted; operator can raise -max-fetch-calls")
			}
			st.FetchCalls++
		} else {
			if st.SearchCalls >= s.opts.MaxSearchCalls {
				return errors.New("persistent search budget exhausted; operator can raise -max-search-calls")
			}
			st.SearchCalls++
		}
		return nil
	})
}

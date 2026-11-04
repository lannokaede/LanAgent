// Package corpus collects an explicit, bounded set of official interview sources.
// It never discovers links recursively or circumvents robots/access restrictions.
package corpus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const userAgent = "LanAgentInterviewBot/1.0"
const maxBody = 4 << 20

type Source struct {
	ID    string `json:"id"`
	Topic string `json:"topic"`
	URL   string `json:"url"`
}
type Document struct {
	Source
	Title     string    `json:"title"`
	File      string    `json:"file"`
	SHA256    string    `json:"sha256"`
	FetchedAt time.Time `json:"fetched_at"`
	Error     string    `json:"error,omitempty"`
}
type Manifest struct {
	Version   int        `json:"version"`
	Documents []Document `json:"documents"`
}

func Sources() []Source {
	return []Source{
		{"go-faq", "go", "https://go.dev/doc/faq"},
		{"effective-go", "go", "https://go.dev/doc/effective_go"},
		{"memory-model", "concurrency", "https://go.dev/ref/mem"},
		{"context", "concurrency", "https://go.dev/blog/context"},
		{"pipelines", "concurrency", "https://go.dev/blog/pipelines"},
		{"race-detector", "engineering", "https://go.dev/doc/articles/race_detector"},
		{"gc-guide", "runtime", "https://go.dev/doc/gc-guide"},
		{"slices", "go", "https://go.dev/blog/slices-intro"},
		{"maps", "go", "https://go.dev/blog/maps"},
		{"sql-connections", "database", "https://go.dev/doc/database/manage-connections"},
		{"sql-transactions", "database", "https://go.dev/doc/database/execute-transactions"},
		{"sql-cancel", "database", "https://go.dev/doc/database/cancel-operations"},
		{"diagnostics", "engineering", "https://go.dev/doc/diagnostics"},
		{"redis-types", "redis", "https://redis.io/docs/latest/develop/data-types/"},
		{"redis-persistence", "redis", "https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/"},
		{"pg-isolation", "database", "https://www.postgresql.org/docs/current/transaction-iso.html"},
		{"pg-locks", "database", "https://www.postgresql.org/docs/current/explicit-locking.html"},
	}
}

type Crawler struct {
	Client       *http.Client
	Delay        time.Duration
	allowedHosts map[string]bool // test injection; production uses fixed official hosts
}

func NewCrawler() *Crawler {
	return &Crawler{Client: &http.Client{Timeout: 30 * time.Second}, Delay: time.Second}
}
func (c *Crawler) allowed(u *url.URL) bool {
	if c.allowedHosts != nil {
		return c.allowedHosts[u.Host] && (u.Scheme == "http" || u.Scheme == "https")
	}
	return u.Scheme == "https" && (u.Host == "go.dev" || u.Host == "redis.io" || u.Host == "www.postgresql.org")
}
func (c *Crawler) get(ctx context.Context, raw string) ([]byte, int, error) {
	u, err := url.Parse(raw)
	if err != nil || !c.allowed(u) || u.User != nil {
		return nil, 0, fmt.Errorf("URL outside official source allowlist")
	}
	client := *c.Client
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Host != u.Host || !c.allowed(req.URL) {
			return fmt.Errorf("redirect outside source origin")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(body) > maxBody {
		return nil, resp.StatusCode, fmt.Errorf("response exceeds 4 MiB")
	}
	return body, resp.StatusCode, nil
}

// Crawl creates a new corpus directory. Failures remain explicit in manifest.json;
// callers must not replace an existing index unless the corpus is complete.
func (c *Crawler) Crawl(ctx context.Context, sources []Source, dir string) (*Manifest, error) {
	if len(sources) == 0 || len(sources) > 30 {
		return nil, fmt.Errorf("source count must be 1..30")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, fmt.Errorf("corpus directory must not exist: %w", err)
	}
	manifest := &Manifest{Version: 1}
	robots := map[string]string{}
	failures := 0
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return manifest, err
		}
		doc := Document{Source: source, FetchedAt: time.Now().UTC()}
		err := func() error {
			if !regexp.MustCompile(`^[a-z0-9-]{1,64}$`).MatchString(source.ID) {
				return fmt.Errorf("invalid source ID")
			}
			u, err := url.Parse(source.URL)
			if err != nil || !c.allowed(u) {
				return fmt.Errorf("URL outside allowlist")
			}
			origin := u.Scheme + "://" + u.Host
			rules, ok := robots[origin]
			if !ok {
				body, status, err := c.get(ctx, origin+"/robots.txt")
				if err != nil {
					return err
				}
				if status != 200 && status != 404 {
					return fmt.Errorf("robots returned HTTP %d", status)
				}
				if status == 200 {
					rules = string(body)
				}
				robots[origin] = rules
			}
			if !robotsAllowed(rules, u.EscapedPath()) {
				return fmt.Errorf("robots disallows source")
			}
			timer := time.NewTimer(c.Delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
			body, status, err := c.get(ctx, source.URL)
			if err != nil {
				return err
			}
			if status != 200 {
				return fmt.Errorf("source returned HTTP %d", status)
			}
			title, text, err := Extract(body)
			if err != nil {
				return err
			}
			doc.Title = title
			doc.File = source.ID + ".md"
			content := fmt.Sprintf("# %s\n\nSource: %s\nTopic: %s\n\n%s\n", title, source.URL, source.Topic, text)
			sum := sha256.Sum256([]byte(content))
			doc.SHA256 = hex.EncodeToString(sum[:])
			return os.WriteFile(filepath.Join(dir, doc.File), []byte(content), 0600)
		}()
		if err != nil {
			doc.Error = err.Error()
			failures++
		}
		manifest.Documents = append(manifest.Documents, doc)
		data, _ := json.MarshalIndent(manifest, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0600); err != nil {
			return manifest, err
		}
	}
	if failures > 0 {
		return manifest, fmt.Errorf("%d source(s) failed; inspect manifest.json", failures)
	}
	return manifest, nil
}

func Extract(body []byte) (string, string, error) {
	root, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return "", "", err
	}
	var main, title, fallback *html.Node
	var find func(*html.Node)
	find = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if a.Key == "id" && a.Val == "docContent" {
					fallback = n
				}
			}
			if n.Data == "title" && title == nil {
				title = n
			}
			if n.Data == "main" && main == nil {
				main = n
			}
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			find(ch)
		}
	}
	find(root)
	if main == nil {
		main = fallback
	}
	if main == nil {
		return "", "", fmt.Errorf("no main content element")
	}
	var text func(*html.Node, bool) string
	text = func(n *html.Node, pre bool) string {
		if n.Type == html.TextNode {
			if pre {
				return n.Data
			}
			return strings.Join(strings.Fields(n.Data), " ") + " "
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "nav", "footer", "header", "button", "aside":
				return ""
			}
		}
		var b strings.Builder
		block := n.Type == html.ElementNode && strings.Contains(" p div section article h1 h2 h3 h4 h5 h6 li pre br ", " "+n.Data+" ")
		if block {
			b.WriteString("\n\n")
		}
		if n.Data == "pre" {
			b.WriteString("```\n")
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			b.WriteString(text(ch, pre || n.Data == "pre"))
		}
		if n.Data == "pre" {
			b.WriteString("\n```")
		}
		if block {
			b.WriteString("\n\n")
		}
		return b.String()
	}
	content := strings.TrimSpace(regexp.MustCompile(`\n[ \t]*\n(?:[ \t]*\n)+`).ReplaceAllString(text(main, false), "\n\n"))
	if len(content) < 200 {
		return "", "", fmt.Errorf("main text is too short")
	}
	name := "Official documentation"
	if title != nil {
		name = strings.TrimSpace(text(title, false))
	}
	return name, content, nil
}

// Select the most specific applicable User-agent group; longest matching path
// wins and Allow wins ties. Unsupported directives do not grant access.
func robotsAllowed(raw, path string) bool {
	type rule struct {
		path  string
		allow bool
	}
	type group struct {
		agents []string
		rules  []rule
	}
	var groups []group
	current := group{}
	haveRules := false
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := strings.ToLower(strings.TrimSpace(parts[0])), strings.TrimSpace(parts[1])
		if key == "user-agent" {
			if haveRules {
				groups = append(groups, current)
				current = group{}
				haveRules = false
			}
			current.agents = append(current.agents, strings.ToLower(val))
		}
		if (key == "allow" || key == "disallow") && len(current.agents) > 0 {
			haveRules = true
			if val != "" {
				current.rules = append(current.rules, rule{val, key == "allow"})
			}
		}
	}
	groups = append(groups, current)
	bestAgent := -1
	var rules []rule
	for _, g := range groups {
		rank := -1
		for _, a := range g.agents {
			if a == "*" && rank < 0 {
				rank = 0
			} else if a != "" && strings.Contains(strings.ToLower(userAgent), a) {
				rank = max(rank, len(a))
			}
		}
		if rank > bestAgent {
			bestAgent = rank
			rules = nil
		}
		if rank == bestAgent && rank >= 0 {
			rules = append(rules, g.rules...)
		}
	}
	best := -1
	allowed := true
	for _, r := range rules {
		end := strings.HasSuffix(r.path, "$")
		pattern := strings.TrimSuffix(r.path, "$")
		pattern = regexp.QuoteMeta(pattern)
		pattern = strings.ReplaceAll(pattern, `\*`, ".*")
		pattern = "^" + pattern
		if end {
			pattern += "$"
		}
		match, _ := regexp.MatchString(pattern, path)
		specificity := len(strings.ReplaceAll(strings.TrimSuffix(r.path, "$"), "*", ""))
		if match && (specificity > best || specificity == best && r.allow) {
			best = specificity
			allowed = r.allow
		}
	}
	return allowed
}

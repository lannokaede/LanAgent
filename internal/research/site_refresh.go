package research

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/lannokaede/LanAgent/internal/corpus"
)

type RefreshReport struct {
	Attempted int               `json:"attempted"`
	Indexed   int               `json:"indexed"`
	Failures  map[string]string `json:"failures"`
}

// RefreshOfficial discovers bounded same-origin documentation links. Each page
// is committed independently, so cancellation never discards completed work.
func (i *SiteIndex) RefreshOfficial(ctx context.Context, maxPages, maxDepth int) (RefreshReport, error) {
	seeds := []string{}
	for _, s := range corpus.Sources() {
		seeds = append(seeds, s.URL)
	}
	return i.refresh(ctx, corpus.NewCrawler(), seeds, maxPages, maxDepth)
}
func (i *SiteIndex) refresh(ctx context.Context, c *corpus.Crawler, seeds []string, maxPages, maxDepth int) (RefreshReport, error) {
	report := RefreshReport{Failures: map[string]string{}}
	if maxPages < 1 || maxPages > 1000 || maxDepth < 0 || maxDepth > 5 {
		return report, errors.New("max-pages must be 1..1000 and max-depth 0..5")
	}
	type item struct {
		url   string
		depth int
	}
	queue := []item{}
	seen := map[string]bool{}
	enqueue := func(raw string, depth int) {
		u, e := url.Parse(raw)
		if e != nil {
			return
		}
		u.Fragment = ""
		u.RawQuery = ""
		key := u.String()
		if !seen[key] && len(seen) < 10000 {
			seen[key] = true
			queue = append(queue, item{key, depth})
		}
	}
	for _, u := range seeds {
		enqueue(u, 0)
	}
	for len(queue) > 0 && report.Attempted < maxPages {
		if e := ctx.Err(); e != nil {
			return report, e
		}
		next := queue[0]
		queue = queue[1:]
		report.Attempted++
		p, e := c.FetchPage(ctx, next.url)
		if e != nil {
			report.Failures[next.url] = e.Error()
			if saveErr := i.Failure(ctx, next.url, e); saveErr != nil {
				return report, saveErr
			}
			continue
		}
		if e = i.Put(ctx, Page{URL: p.URL, Title: p.Title, Text: p.Text}); e != nil {
			return report, e
		}
		report.Indexed++
		if next.depth < maxDepth {
			for _, link := range p.Links {
				if documentationLink(link) {
					enqueue(link, next.depth+1)
				}
			}
		}
	}
	if e := ctx.Err(); e != nil {
		return report, e
	}
	if len(report.Failures) > 0 {
		return report, fmt.Errorf("%d page(s) failed; completed pages remain searchable", len(report.Failures))
	}
	return report, nil
}
func documentationLink(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil {
		return false
	}
	// Avoid indexing accounts, source listings, forums and unrelated navigation.
	for _, p := range []string{"/doc/", "/docs/", "/ref/", "/blog/"} {
		if strings.HasPrefix(u.Path, p) {
			return true
		}
	}
	return false
}

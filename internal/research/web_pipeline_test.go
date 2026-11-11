package research

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lannokaede/LanAgent/internal/extension"
	"github.com/lannokaede/LanAgent/internal/harness"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type pipelineModel func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error)

func (f pipelineModel) Chat(ctx context.Context, r *llm.ChatRequest) (*llm.ChatResponse, error) {
	return f(ctx, r)
}

func TestOfficialWebPipelineOnDemandAndNoKnowledgeWrites(t *testing.T) {
	ctx := context.Background()
	pageCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Write([]byte("User-agent: *\nAllow: /\n"))
			return
		}
		pageCalls++
		if r.URL.Path == "/docs/fail" {
			http.Error(w, "unavailable", 503)
			return
		}
		w.Write([]byte("<title>Current Context</title><main>" + strings.Repeat("verified-current-value cancellation ", 30) + "</main>"))
	}))
	defer server.Close()
	index := &fakeIndex{}
	s, e := Open(ctx, Options{Directory: t.TempDir(), Namespace: "test-go", Indexer: index})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	s.crawler = fixtureCrawler(server)
	if e = s.siteIndex.Put(ctx, Page{URL: "https://go.dev/docs/context", Title: "Context", Text: "cached-old-value cancellation"}); e != nil {
		t.Fatal(e)
	}
	a, b := mcp.NewInMemoryTransports()
	ss, e := s.Server().Connect(ctx, a, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ss.Close()
	cs, e := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, b, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer cs.Close()
	r := tool.NewRegistry()
	if e = extension.Register(ctx, r, cs, "research", []string{"web_search", "web_fetch", "read_artifact"}); e != nil {
		t.Fatal(e)
	}
	names := map[string]string{}
	for _, spec := range r.ListSpecs() {
		for _, name := range []string{"web_search", "web_fetch", "read_artifact"} {
			if strings.Contains(spec.Description, "research/"+name+":") {
				names[name] = spec.Name
			}
		}
	}
	for _, scenario := range []string{"普通解释", "不要联网，解释 context", "核实最新文档", "抓取失败"} {
		steps := 0
		before := pageCalls
		p := pipelineModel(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
			steps++
			switch scenario {
			case "普通解释", "不要联网，解释 context":
				return &llm.ChatResponse{Content: "直接解释"}, nil
			case "抓取失败":
				if steps == 1 {
					return &llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "f", Name: names["web_fetch"], ArgsJSON: `{"url":"https://go.dev/docs/fail"}`}}}, nil
				}
				last := req.Messages[len(req.Messages)-1]
				if !strings.HasPrefix(last.Content, "error:") {
					t.Fatal(last)
				}
				return &llm.ChatResponse{Content: "读取失败，未完成核实"}, nil
			default:
				if steps == 1 {
					return &llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "s", Name: names["web_search"], ArgsJSON: `{"query":"cancellation"}`}}}, nil
				}
				last := req.Messages[len(req.Messages)-1]
				if steps == 2 {
					if pageCalls != before || !strings.Contains(last.Content, "local_site_index") || !strings.Contains(last.Content, "cached-old-value") {
						t.Fatal("search not local or evidence missing", pageCalls, last)
					}
					return &llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "f", Name: names["web_fetch"], ArgsJSON: `{"url":"https://go.dev/docs/context"}`}}}, nil
				}
				if !strings.Contains(last.Content, "verified-current-value") || !strings.Contains(last.Content, "fetched_at") {
					t.Fatal("live content missing", last)
				}
				return &llm.ChatResponse{Content: "已读取官方文档 https://go.dev/docs/context：verified-current-value"}, nil
			}
		})
		_, response, e := harness.NewRunner(p, r, 5).Run(ctx, []llm.Message{{Role: llm.RoleUser, Content: scenario}})
		if e != nil {
			t.Fatal(scenario, e)
		}
		if steps == 1 && pageCalls != before {
			t.Fatal("unnecessary network call")
		}
		if scenario == "抓取失败" && !strings.Contains(response.Content, "未完成核实") {
			t.Fatal(response)
		}
	}
	if pageCalls != 2 {
		t.Fatal(pageCalls)
	}
	if index.count != 0 || len(s.data.Jobs) != 0 || len(s.data.Candidates) != 0 {
		t.Fatal("browsing wrote knowledge", index.count, s.data)
	}
	matches, e := s.siteIndex.Search(ctx, "verified-current-value", 5)
	if e != nil || len(matches) != 1 {
		t.Fatal("fetch did not update index", matches, e)
	}
}

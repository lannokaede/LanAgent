package extension

import (
	"context"
	"strings"
	"testing"

	"github.com/lannokaede/LanAgent/internal/harness"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPAdapterAllowlistAndLocalSchemaValidation(t *testing.T) {
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	calls := 0
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in struct {
		Text string `json:"text"`
	}) (*mcp.CallToolResult, any, error) {
		calls++
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "forbidden"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		t.Fatal("unlisted tool called")
		return nil, nil, nil
	})
	a, b := mcp.NewInMemoryTransports()
	ss, e := server.Connect(ctx, a, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "lanagent-test", Version: "1"}, nil)
	cs, e := client.Connect(ctx, b, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer cs.Close()
	registry := tool.NewRegistry()
	if e = Register(ctx, registry, cs, "demo", []string{"echo"}); e != nil {
		t.Fatal(e)
	}
	tools := registry.ListTools()
	if len(tools) != 1 {
		t.Fatal("allowlist ignored")
	}
	if _, e = tools[0].Execute(ctx, `{"text":3}`); e == nil || calls != 0 {
		t.Fatal("invalid args reached remote server")
	}
	result, e := tools[0].Execute(ctx, `{"text":"hello"}`)
	if e != nil || !strings.Contains(result, "hello") || calls != 1 {
		t.Fatal(result, e)
	}
}

func TestWebPolicyBlocksModelRequestsBeforeRemoteCall(t *testing.T) {
	ctx := context.Background()
	calls := 0
	server := mcp.NewServer(&mcp.Implementation{Name: "research", Version: "test"}, nil)
	for _, name := range []string{"web_search", "web_fetch"} {
		mcp.AddTool(server, &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			calls++
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "real result"}}}, nil, nil
		})
	}
	a, b := mcp.NewInMemoryTransports()
	ss, e := server.Connect(ctx, a, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ss.Close()
	cs, e := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, b, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer cs.Close()
	profiles := map[string]string{}
	for _, policy := range []string{"off", "auto"} {
		r := tool.NewRegistry()
		if e = registerWithWebPolicy(ctx, r, cs, "research", []string{"web_search", "web_fetch"}, policy); e != nil {
			t.Fatal(e)
		}
		modelCalls := 0
		p := webPolicyModel(func(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
			modelCalls++
			if modelCalls == 1 {
				return &llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "s", Name: req.Tools[0].Name, ArgsJSON: `{}`}, {ID: "f", Name: req.Tools[1].Name, ArgsJSON: `{}`}}}, nil
			}
			for _, msg := range req.Messages {
				if msg.Role == llm.RoleTool {
					if policy == "off" && !strings.Contains(msg.Content, "disabled") {
						t.Fatal(msg)
					}
					if policy == "auto" && !strings.Contains(msg.Content, "real result") {
						t.Fatal(msg)
					}
				}
			}
			return &llm.ChatResponse{Content: "done"}, nil
		})
		runner := harness.NewRunner(p, r, 3)
		profiles[policy] = runner.ProfileID()
		if _, _, e = runner.Run(ctx, []llm.Message{{Role: llm.RoleUser, Content: "verify latest"}}); e != nil {
			t.Fatal(e)
		}
		if policy == "off" && calls != 0 {
			t.Fatal("blocked request reached server", calls)
		}
	}
	if calls != 2 || profiles["off"] == profiles["auto"] {
		t.Fatal("policy not reflected in execution/profile", calls, profiles)
	}
}

type webPolicyModel func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error)

func (f webPolicyModel) Chat(ctx context.Context, r *llm.ChatRequest) (*llm.ChatResponse, error) {
	return f(ctx, r)
}

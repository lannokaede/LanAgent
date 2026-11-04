package extension

import (
	"context"
	"strings"
	"testing"

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

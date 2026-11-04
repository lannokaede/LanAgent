package extension

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/lannokaede/LanAgent/internal/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestExecutionMetadataAndReconciliation(t *testing.T) {
	ctx := context.Background()
	id := fmt.Sprintf("%x", sha256.Sum256([]byte("execution")))
	args := `{"candidate_id":"abc","manifest_hash":"xyz"}`
	var canonical any
	json.Unmarshal([]byte(args), &canonical)
	b, _ := json.Marshal(canonical)
	argsHash := fmt.Sprintf("%x", sha256.Sum256(b))
	calls := 0
	server := mcp.NewServer(&mcp.Implementation{Name: "research", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "knowledge_commit"}, func(_ context.Context, r *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
		calls++
		if r.Params.Meta["io.lanagent/execution_id"] != id {
			t.Error("host metadata missing")
		}
		return nil, map[string]any{"status": "accepted", "job_id": id}, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "operation_status"}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
		return nil, map[string]any{"status": "found", "execution_id": id, "tool": "knowledge_commit", "arguments_sha256": argsHash, "result": map[string]any{"status": "accepted", "job_id": id}}, nil
	})
	a, bTransport := mcp.NewInMemoryTransports()
	ss, e := server.Connect(ctx, a, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ss.Close()
	cs, e := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, bTransport, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer cs.Close()
	registry := tool.NewRegistry()
	if e = Register(ctx, registry, cs, "research", []string{"knowledge_commit", "operation_status"}); e != nil {
		t.Fatal(e)
	}
	var alias string
	for _, v := range registry.ListTools() {
		if strings.HasPrefix(v.Spec().Description, "MCP research/knowledge_commit") {
			alias = v.Spec().Name
			out, e := v.Execute(tool.WithExecutionID(ctx, id), args)
			if e != nil {
				t.Fatal(out, e)
			}
			break
		}
	}
	c := &Connections{remote: map[string]remoteTool{alias: {cs, "knowledge_commit"}, "status": {cs, "operation_status"}}}
	if _, e = c.Reconcile(ctx, alias, id, args); e != nil {
		t.Fatal(e)
	}
	if calls != 1 {
		t.Fatal("reconciliation re-executed mutation")
	}
	if _, e = c.Reconcile(ctx, alias, id, `{"candidate_id":"other"}`); e == nil {
		t.Fatal("argument mismatch accepted")
	}
}

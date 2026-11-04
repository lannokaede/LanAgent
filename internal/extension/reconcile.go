package extension

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type remoteTool struct {
	session *mcp.ClientSession
	name    string
}

// Reconcile runs outside the blocked Runner. Only an allowlisted status tool
// on the original server can supply the original, argument-bound observation.
func (c *Connections) Reconcile(ctx context.Context, alias, id, args string) (string, error) {
	remote, ok := c.remote[alias]
	if !ok || remote.name != "knowledge_commit" {
		return "", errors.New("this uncertain tool has no supported reconciliation contract")
	}
	allowed := false
	for _, r := range c.remote {
		if r.session == remote.session && r.name == "operation_status" {
			allowed = true
		}
	}
	if !allowed {
		return "", errors.New("operation_status must be allowlisted on the original MCP server")
	}
	result, e := remote.session.CallTool(ctx, &mcp.CallToolParams{Name: "operation_status", Arguments: map[string]any{"execution_id": id}})
	if e != nil || result.IsError {
		return "", errors.New("remote outcome unavailable; preserve pending execution")
	}
	var b []byte
	if result.StructuredContent != nil {
		b, e = json.Marshal(result.StructuredContent)
	} else {
		for _, v := range result.Content {
			if t, ok := v.(*mcp.TextContent); ok {
				b = append(b, []byte(t.Text)...)
				break
			}
		}
	}
	if e != nil || len(b) > 256<<10 {
		return "", errors.New("invalid reconciliation result")
	}
	var op struct {
		Status string          `json:"status"`
		ID     string          `json:"execution_id"`
		Tool   string          `json:"tool"`
		Hash   string          `json:"arguments_sha256"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(b, &op) != nil {
		return "", errors.New("invalid operation record")
	}
	var arguments any
	if json.Unmarshal([]byte(args), &arguments) != nil {
		return "", errors.New("invalid original arguments")
	}
	canonical, _ := json.Marshal(arguments)
	digest := sha256.Sum256(canonical)
	if op.Status != "found" || op.ID != id || op.Tool != remote.name || op.Hash != fmt.Sprintf("%x", digest) || len(op.Result) == 0 {
		return "", errors.New("operation record does not match pending invocation")
	}
	var accepted struct {
		Status string `json:"status"`
		JobID  string `json:"job_id"`
	}
	if json.Unmarshal(op.Result, &accepted) != nil || accepted.Status != "accepted" || len(accepted.JobID) != 64 {
		return "", errors.New("operation has no durable accepted job")
	}
	return string(op.Result), nil
}

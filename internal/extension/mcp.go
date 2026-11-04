// Package extension adapts explicitly configured MCP servers into harness tools.
package extension

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Server struct {
	Name     string   `json:"name"`
	URL      string   `json:"url,omitempty"`
	Command  string   `json:"command,omitempty"`
	Args     []string `json:"args,omitempty"`
	TokenEnv string   `json:"token_env,omitempty"`
	Tools    []string `json:"tools"`
}
type Connections struct {
	remote   map[string]remoteTool
	sessions []*mcp.ClientSession
	identity string
}

func (c *Connections) ProfileID() string { return c.identity }

func (c *Connections) Close() error {
	var errs []error
	for _, s := range c.sessions {
		errs = append(errs, s.Close())
	}
	return errors.Join(errs...)
}

func Load(ctx context.Context, path, workspace string, r *tool.Registry) (connections *Connections, err error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	var cfg struct {
		Servers []Server `json:"servers"`
	}
	d := json.NewDecoder(io.LimitReader(f, 256<<10))
	d.DisallowUnknownFields()
	if e = d.Decode(&cfg); e != nil {
		return nil, e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, errors.New("invalid MCP config")
	}
	c := &Connections{remote: map[string]remoteTool{}}
	defer func() {
		if err != nil {
			c.Close()
		}
	}()
	if len(cfg.Servers) > 16 {
		return nil, errors.New("too many MCP servers")
	}
	names := map[string]bool{}
	for _, server := range cfg.Servers {
		if server.Name == "" || names[server.Name] || len(server.Tools) == 0 || (server.URL == "") == (server.Command == "") {
			return nil, errors.New("MCP servers require unique names, explicit tools, and exactly one transport")
		}
		names[server.Name] = true
		client := mcp.NewClient(&mcp.Implementation{Name: "lanagent", Version: "0.2.0"}, nil)
		var transport mcp.Transport
		if server.Command != "" {
			cmd := exec.CommandContext(ctx, server.Command, server.Args...)
			cmd.Dir = workspace
			cmd.Stderr = os.Stderr
			transport = &mcp.CommandTransport{Command: cmd, TerminateDuration: time.Second}
		} else {
			hc := &http.Client{Transport: authTransport{token: os.Getenv(server.TokenEnv)}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("MCP redirect refused") }}
			transport = &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: hc}
		}
		session, e := client.Connect(ctx, transport, nil)
		if e != nil {
			return nil, fmt.Errorf("MCP %s: %w", server.Name, e)
		}
		c.sessions = append(c.sessions, session)
		if e = Register(ctx, r, session, server.Name, server.Tools); e != nil {
			return nil, e
		}
		for _, name := range server.Tools {
			h := sha256.Sum256([]byte(server.Name + "/" + name))
			c.remote[fmt.Sprintf("mcp_%x", h[:10])] = remoteTool{session, name}
		}
	}
	identities := []any{cfg.Servers}
	for _, server := range cfg.Servers {
		if server.TokenEnv != "" {
			digest := sha256.Sum256([]byte(os.Getenv(server.TokenEnv)))
			identities = append(identities, fmt.Sprintf("%x", digest))
		}
	}
	raw, _ := json.Marshal(identities)
	sum := sha256.Sum256(raw)
	c.identity = fmt.Sprintf("%x", sum)
	return c, nil
}

type authTransport struct{ token string }

func (t authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	copy.Header = req.Header.Clone()
	if t.token != "" {
		copy.Header.Set("Authorization", "Bearer "+t.token)
	}
	return http.DefaultTransport.RoundTrip(copy)
}

// Register resolves only local schema references and publishes only operator-
// selected tool names. Server tool annotations never grant permissions.
func Register(ctx context.Context, r *tool.Registry, session *mcp.ClientSession, server string, allowed []string) error {
	want := map[string]bool{}
	for _, n := range allowed {
		if want[n] {
			return errors.New("duplicate MCP allowlist entry")
		}
		want[n] = true
	}
	for remote, e := range session.Tools(ctx, nil) {
		if e != nil {
			return e
		}
		if !want[remote.Name] {
			continue
		}
		raw, e := json.Marshal(remote.InputSchema)
		if e != nil {
			return e
		}
		var schema jsonschema.Schema
		if e = json.Unmarshal(raw, &schema); e != nil {
			return e
		}
		resolved, e := schema.Resolve(nil)
		if e != nil {
			return fmt.Errorf("MCP schema %s: %w", remote.Name, e)
		}
		var params map[string]any
		if e = json.Unmarshal(raw, &params); e != nil {
			return e
		}
		sum := sha256.Sum256([]byte(server + "/" + remote.Name))
		name := fmt.Sprintf("mcp_%x", sum[:10])
		var outputSchema *jsonschema.Resolved
		if remote.OutputSchema != nil {
			encoded, err := json.Marshal(remote.OutputSchema)
			if err != nil {
				return err
			}
			var declared jsonschema.Schema
			if err = json.Unmarshal(encoded, &declared); err != nil {
				return err
			}
			outputSchema, err = declared.Resolve(nil)
			if err != nil {
				return fmt.Errorf("MCP output schema %s: %w", remote.Name, err)
			}
		}
		adapter := mcpTool{outputSchema: outputSchema, session: session, remote: remote.Name, schema: resolved, spec: llm.ToolDef{Name: name, Description: "MCP " + server + "/" + remote.Name + ": " + remote.Description, Parameters: params}}
		if e = r.Register(adapter); e != nil {
			return e
		}
		delete(want, remote.Name)
	}
	if len(want) > 0 {
		return fmt.Errorf("MCP %s is missing allowlisted tools", server)
	}
	return nil
}

type mcpTool struct {
	outputSchema *jsonschema.Resolved
	session      *mcp.ClientSession
	remote       string
	schema       *jsonschema.Resolved
	spec         llm.ToolDef
}

func (t mcpTool) Spec() llm.ToolDef { return t.spec }
func (t mcpTool) Execute(ctx context.Context, raw string) (string, error) {
	if len(raw) > 64<<10 {
		return "", errors.New("MCP arguments too large")
	}
	var args any
	d := json.NewDecoder(strings.NewReader(raw))
	if e := d.Decode(&args); e != nil {
		return "", e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return "", errors.New("invalid MCP arguments")
	}
	if e := t.schema.Validate(args); e != nil {
		return "", fmt.Errorf("MCP input schema: %w", e)
	}
	params := &mcp.CallToolParams{Name: t.remote, Arguments: args}
	if id := tool.ExecutionID(ctx); id != "" {
		params.Meta = mcp.Meta{"io.lanagent/execution_id": id}
	}
	result, e := t.session.CallTool(ctx, params)
	if e != nil {
		return "", fmt.Errorf("%w: MCP %s: %v", tool.ErrUncertain, t.remote, e)
	}
	if !result.IsError && t.outputSchema != nil {
		encoded, err := json.Marshal(result.StructuredContent)
		var value any
		if err == nil {
			err = json.Unmarshal(encoded, &value)
		}
		if err == nil {
			err = t.outputSchema.Validate(value)
		}
		if err != nil {
			return "", fmt.Errorf("%w: MCP output schema mismatch for %s", tool.ErrUncertain, t.remote)
		}
	}
	var text strings.Builder
	if result.StructuredContent == nil {
		for _, item := range result.Content {
			if v, ok := item.(*mcp.TextContent); ok {
				text.WriteString(v.Text)
				text.WriteByte('\n')
			}
		}
	}
	if result.StructuredContent != nil {
		b, e := json.Marshal(result.StructuredContent)
		if e != nil {
			return "", e
		}
		text.Write(b)
	}
	if text.Len() > 256<<10 {
		return "", errors.New("MCP result exceeds limit")
	}
	if result.IsError {
		return "", errors.New(text.String())
	}
	return text.String(), nil
}

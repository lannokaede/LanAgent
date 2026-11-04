package workspace

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/tool"
)

// RegisterActions is an explicit operator permission gate. Never enabled by model
// output, file text, Skills, or MCP tool annotations.
func (w *Workspace) RegisterActions(r *tool.Registry, write, commands bool) error {
	if write {
		if e := r.Register(&writeTool{w: w}); e != nil {
			return e
		}
	}
	if commands {
		return r.Register(commandTool{w: w})
	}
	return nil
}
func sha(s string) string { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:]) }

type writeTool struct {
	w  *Workspace
	mu sync.Mutex
}

func (t *writeTool) Spec() llm.ToolDef {
	return llm.ToolDef{Name: "write_file", Description: "Atomically write UTF-8 text in the workspace. Supply expected_sha256 from the file's current content; empty means create only. Files and content are limited to 32768 bytes. Existing identical content is an idempotent success. Parent directory must exist. Never overwrites a mismatched version.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}, "expected_sha256": map[string]any{"type": "string"}}, "required": []string{"path", "content", "expected_sha256"}, "additionalProperties": false}}
}
func (t *writeTool) Execute(ctx context.Context, raw string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var in struct {
		Path     string  `json:"path"`
		Content  string  `json:"content"`
		Expected *string `json:"expected_sha256"`
	}
	if len(raw) > 128<<10 {
		return "", errors.New("write request too large")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&in); e != nil {
		return "", e
	}
	var extra any
	if d.Decode(&extra) != io.EOF || in.Expected == nil || !fs.ValidPath(in.Path) || strings.Contains(in.Path, "\\") || len(in.Content) > maxReadBytes || !utf8.ValidString(in.Content) || strings.ContainsRune(in.Content, 0) {
		return "", errors.New("invalid write arguments")
	}
	for _, part := range strings.Split(in.Path, "/") {
		if strings.EqualFold(part, ".git") {
			return "", errors.New("repository metadata is not writable through file tools")
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	exists := false
	if info, e := t.w.root.Lstat(in.Path); e == nil {
		exists = true
		if !info.Mode().IsRegular() {
			return "", errors.New("destination must be a regular file")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return "", e
	}
	if exists {
		f, e := t.w.root.Open(in.Path)
		if e != nil {
			return "", e
		}
		b, e := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
		f.Close()
		if e != nil {
			return "", e
		}
		if len(b) > maxReadBytes {
			return "", errors.New("existing file exceeds edit size limit")
		}
		if string(b) == in.Content {
			return encode(map[string]any{"path": in.Path, "sha256": sha(in.Content), "unchanged": true})
		}
		if *in.Expected == "" || sha(string(b)) != *in.Expected {
			return "", errors.New("file version conflict; read current content before writing")
		}
	} else if *in.Expected != "" {
		return "", errors.New("expected file does not exist")
	}
	name := in.Path + ".lanagent-" + rand.Text() + ".tmp"
	f, e := t.w.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return "", e
	}
	defer t.w.root.Remove(name)
	_, e = f.WriteString(in.Content)
	e = errors.Join(e, f.Sync(), f.Close())
	if e != nil {
		return "", e
	}
	if e = ctx.Err(); e != nil {
		return "", e
	}
	if e = t.w.root.Rename(name, in.Path); e != nil {
		return "", errors.Join(tool.ErrUncertain, e)
	}
	return encode(map[string]any{"path": in.Path, "sha256": sha(in.Content), "written": true})
}

type commandTool struct{ w *Workspace }

func (t commandTool) Spec() llm.ToolDef {
	return llm.ToolDef{Name: "run_command", Description: "Execute one fixed diagnostic command without a shell: go_version, git_status, or git_diff. No arbitrary arguments, scripts or environment are accepted. This is a host diagnostic, not a code sandbox.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string", "enum": []string{"go_version", "git_status", "git_diff"}}}, "required": []string{"command"}, "additionalProperties": false}}
}
func (t commandTool) Execute(ctx context.Context, raw string) (string, error) {
	var in struct {
		Command string `json:"command"`
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&in); e != nil {
		return "", e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return "", errors.New("invalid command JSON")
	}
	commands := map[string][]string{"go_version": {"go", "version"}, "git_status": {"git", "--no-optional-locks", "status", "--short"}, "git_diff": {"git", "--no-pager", "diff", "--no-ext-diff", "--no-textconv"}}
	argv, ok := commands[in.Command]
	if !ok {
		return "", errors.New("command is not allowed")
	}
	env := diagnosticEnvironment()
	if argv[0] == "git" {
		overrides, e := disabledGitExtensions(ctx, t.w.path, env)
		if e != nil {
			return "", e
		}
		fixed := []string{"git", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull}
		fixed = append(fixed, overrides...)
		fixed = append(fixed, argv[1:]...)
		fixed = append(fixed, "--ignore-submodules=all")
		argv = fixed
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = t.w.path
	cmd.Env = env
	cmd.WaitDelay = 2 * time.Second
	b := &commandOutput{}
	cmd.Stdout = b
	cmd.Stderr = b
	e := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	exit := 0
	if e != nil {
		var ee *exec.ExitError
		if !errors.As(e, &ee) {
			return "", e
		}
		exit = ee.ExitCode()
	}
	return encode(map[string]any{"command": in.Command, "exit_code": exit, "output": strings.ToValidUTF8(b.text, "�"), "truncated": b.truncated})
}

type commandOutput struct {
	mu        sync.Mutex
	text      string
	truncated bool
}

func (b *commandOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	left := 32<<10 - len(b.text)
	if len(p) > left {
		b.truncated = true
		p = p[:left]
	}
	b.text += string(p)
	return n, nil
}

func diagnosticEnvironment() []string {
	env := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_ATTR_NOSYSTEM=1", "GIT_PAGER=cat", "GOTOOLCHAIN=local")
}

// Read filter names without running them, then override each driver in the
// diagnostic invocation. --no-ext-diff alone does not disable clean filters.
func disabledGitExtensions(ctx context.Context, dir string, env []string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "config", "--local", "--includes", "--null", "--name-only", "--get-regexp", `^filter\..*\.(clean|smudge|process|required)$`)
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = 2 * time.Second
	output := &commandOutput{}
	cmd.Stdout = output
	if e := cmd.Run(); e != nil {
		var status *exec.ExitError
		if errors.As(e, &status) && status.ExitCode() == 1 {
			return nil, nil
		}
		return nil, errors.New("cannot inspect repository diagnostic configuration")
	}
	if output.truncated {
		return nil, errors.New("repository filter configuration exceeds diagnostic limit")
	}
	args := []string{}
	for _, key := range strings.Split(output.text, "\x00") {
		if key == "" {
			continue
		}
		if strings.ContainsAny(key, "\r\n=") {
			return nil, errors.New("invalid repository filter key")
		}
		value := ""
		if strings.HasSuffix(strings.ToLower(key), ".required") {
			value = "false"
		}
		args = append(args, "-c", key+"="+value)
	}
	return args, nil
}

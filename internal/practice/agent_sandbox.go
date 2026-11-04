package practice

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/tool"
)

// AgentSandbox permits fixed exercise tests, not arbitrary commands or tests.
// A shared gate bounds model-triggered Docker executions across review jobs.
type AgentSandbox struct {
	executor Executor
	slots    chan struct{}
}

func NewAgentSandbox(executor Executor, concurrency int) *AgentSandbox {
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 2 {
		concurrency = 2
	}
	return &AgentSandbox{executor: executor, slots: make(chan struct{}, concurrency)}
}

type sandboxTool struct {
	sandbox  *AgentSandbox
	exercise Exercise
	calls    int
	results  map[string]string
}

func (t *sandboxTool) Spec() llm.ToolDef {
	return llm.ToolDef{Name: "run_go_exercise", Description: "Run candidate Go code against this exercise's server-owned tests in an isolated Docker container. At most two executions per review. No commands, test changes, files, network or exercise selection. A result applies only to the exact returned code_hash.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"code": map[string]any{"type": "string", "maxLength": 32768}}, "required": []string{"code"}, "additionalProperties": false}}
}
func (t *sandboxTool) Execute(ctx context.Context, raw string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t.calls >= 2 {
		return "", errors.New("sandbox execution budget exhausted")
	}
	if len(raw) > 64<<10 {
		return "", ErrInput
	}
	var in struct {
		Code string `json:"code"`
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return "", ErrInput
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return "", ErrInput
	}
	if err := ExecutionPolicy(in.Code); err != nil {
		return "", ErrInput
	}
	inspect := Inspect(t.exercise, in.Code)
	if !inspect.SyntaxValid || !inspect.ContractValid || len(inspect.Issues) > 0 {
		return "", errors.New("candidate violates exercise contract")
	}
	if t.sandbox == nil || t.sandbox.executor == nil {
		return "", ErrUnavailable
	}
	select {
	case t.sandbox.slots <- struct{}{}:
		defer func() { <-t.sandbox.slots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	t.calls++
	run := t.sandbox.executor.Execute(ctx, t.exercise, in.Code, "agent-"+rand.Text())
	if err := ctx.Err(); err != nil {
		return "", err
	}
	hash := digest(in.Code)
	t.results[hash] = run.Status
	output := run.Output
	if len(output) > 8192 {
		output = output[:8192]
		run.Truncated = true
	}
	return string(encode(map[string]any{"code_hash": hash, "status": run.Status, "exit_code": run.ExitCode, "output": output, "truncated": run.Truncated, "error": run.Error})), nil
}

// ExerciseTool binds the personal harness to one server-owned exercise. The
// caller selects the exercise, never model-provided tests or shell commands.
func (s *AgentSandbox) ExerciseTool(e Exercise) tool.Tool {
	return &sandboxTool{sandbox: s, exercise: e, results: map[string]string{}}
}

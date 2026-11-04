package practice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lannokaede/LanAgent/internal/llm"
)

type sandboxExecutor struct {
	Executor
	calls int
}

func (e *sandboxExecutor) Execute(_ context.Context, exercise Exercise, code, id string) Run {
	e.calls++
	return Run{Status: "passed", Output: "ok", ExitCode: 0}
}

type sandboxProvider struct {
	t      *testing.T
	calls  int
	code   string
	result Diagnosis
}

func (p *sandboxProvider) Chat(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	p.calls++
	if p.calls == 1 {
		found := false
		for _, tool := range req.Tools {
			if tool.Name == "run_go_exercise" {
				found = true
			}
		}
		if !found {
			p.t.Fatal("sandbox tool not registered")
		}
		return &llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "sandbox-test", Name: "run_go_exercise", ArgsJSON: string(encode(map[string]string{"code": p.code}))}}}, nil
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != llm.RoleTool || !strings.Contains(last.Content, digest(p.code)) {
		p.t.Fatal("missing version-bound sandbox observation")
	}
	return &llm.ChatResponse{Content: string(encode(p.result))}, nil
}
func TestSandboxAgentVerificationMatchesExactFinalCode(t *testing.T) {
	e := catalog(t).List()[0]
	for _, same := range []bool{true, false} {
		result := diagnosis(t, e)
		result.Evidence = []Evidence{}
		tested := result.ProposedCode
		if !same {
			tested += "\n// different code snapshot\n"
		}
		executor := &sandboxExecutor{}
		provider := &sandboxProvider{t: t, code: tested, result: result}
		a := &AgentAnalyzer{Provider: provider, Sandbox: NewAgentSandbox(executor, 1)}
		d, _, err := a.Analyze(context.Background(), e, Attempt{Submit: Submit{Code: e.StarterCode}}, nil)
		if err != nil || executor.calls != 1 {
			t.Fatalf("%v calls=%d", err, executor.calls)
		}
		verification := d.Limitations[len(d.Limitations)-1]
		if (d.Verification.Status == "passed") != same || strings.Contains(verification, "服务端已核实") != same {
			t.Fatal("wrong final snapshot verification", verification)
		}
	}
}
func TestSandboxRejectsArbitraryExecutionAndBoundsCalls(t *testing.T) {
	e := catalog(t).List()[0]
	executor := &sandboxExecutor{}
	tool := &sandboxTool{sandbox: NewAgentSandbox(executor, 1), exercise: e, results: map[string]string{}}
	for _, raw := range []string{`{"code":"package exercise","command":"sh"}`, `{"code":"package exercise; import \"os\""}`, `null`, `{"code":"package exercise"} {}`} {
		if _, err := tool.Execute(context.Background(), raw); err == nil {
			t.Fatal("unsafe input accepted", raw)
		}
	}
	if executor.calls != 0 {
		t.Fatal("invalid input executed")
	}
	raw, _ := json.Marshal(map[string]string{"code": fixture(t, e, "correct")})
	for i := 0; i < 2; i++ {
		if _, err := tool.Execute(context.Background(), string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tool.Execute(context.Background(), string(raw)); err == nil || executor.calls != 2 {
		t.Fatal("unbounded model execution")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Execute(cancelled, string(raw)); err == nil {
		t.Fatal("cancel ignored")
	}
}

func TestAgentSandboxRealDocker(t *testing.T) {
	if os.Getenv("PRACTICE_DOCKER_TEST") != "1" {
		t.Skip("PRACTICE_DOCKER_TEST=1 required")
	}
	e := catalog(t).List()[0]
	result := diagnosis(t, e)
	result.Evidence = []Evidence{}
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(base, "lanagent", "practice-tests")
	if err = os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	work, err := os.MkdirTemp(parent, "agent-suite-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(work)
	executor := NewDockerExecutor("lanagent/practice-go:1.26.9-v1", "agent-sandbox-integration", work)
	defer executor.Cleanup(context.Background())
	analyzer := &AgentAnalyzer{Provider: &sandboxProvider{t: t, code: result.ProposedCode, result: result}, Sandbox: NewAgentSandbox(executor, 1)}
	out, _, err := analyzer.Analyze(context.Background(), e, Attempt{Submit: Submit{Code: e.StarterCode}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Limitations[len(out.Limitations)-1], "服务端已核实") {
		t.Fatal("actual Docker run was not verified", out.Limitations)
	}
}

func TestModelCannotSupplyServerVerification(t *testing.T) {
	e := catalog(t).List()[0]
	d := diagnosis(t, e)
	d.Evidence = []Evidence{}
	d.Verification = &CandidateVerification{CodeHash: digest(d.ProposedCode), Status: "passed"}
	if _, err := ValidateDiagnosis(e, e.StarterCode, string(encode(d)), nil); err == nil {
		t.Fatal("model supplied verification accepted")
	}
}

package practice

import (
	"context"
	"encoding/json"
	"errors"
	assets "github.com/lannokaede/LanAgent/examples/practice"
	"github.com/lannokaede/LanAgent/internal/harness"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/rag"
	"strings"
	"testing"
)

func catalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := NewCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func fixture(t *testing.T, e Exercise, name string) string {
	t.Helper()
	b, err := assets.Files.ReadFile(e.ID + "/" + e.Version + "/" + name + ".go.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func TestCatalogAndContracts(t *testing.T) {
	c := catalog(t)
	if len(c.List()) != 3 {
		t.Fatal("expected 3 exercises")
	}
	for _, e := range c.List() {
		t.Run(e.ID, func(t *testing.T) {
			if e.Tests == "" {
				t.Fatal("missing fixed tests")
			}
			raw := string(encode(e))
			if strings.Contains(raw, "TestNormal") || strings.Contains(raw, "TestTransfer") {
				t.Fatal("test source leaked")
			}
			for _, name := range []string{"starter", "correct"} {
				i := Inspect(e, fixture(t, e, name))
				if !i.SyntaxValid || !i.ContractValid {
					t.Fatalf("%s: %#v", name, i)
				}
			}
			if Inspect(e, fixture(t, e, "syntax-error")).SyntaxValid {
				t.Fatal("accepted syntax error")
			}
			if Inspect(e, fixture(t, e, "wrong-contract")).ContractValid {
				t.Fatal("accepted wrong contract")
			}
		})
	}
	if _, err := c.Get("../context-cancel", "v1"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
func TestExecutionPolicy(t *testing.T) {
	for _, code := range []string{"package exercise\nimport \"os\"\n", "package exercise\nimport \"net/http\"\n", "package exercise\nimport \"example.com/dependency\"\n", "package main\n", "package exercise\nfunc init(){}\n", "package exercise\n//go:linkname evil runtime.evil\n", strings.Repeat("a", MaxCodeBytes+1)} {
		if ExecutionPolicy(code) == nil {
			t.Fatal("accepted restricted code")
		}
	}
}
func diagnosis(t *testing.T, e Exercise) Diagnosis {
	return Diagnosis{Summary: "检查取消传播", Findings: []Finding{{Line: 1, EndLine: 1, Message: "检查上下文"}}, Evidence: []Evidence{{Source: "context.md", ChunkIndex: 0, Quote: "取消需要协作"}}, ProposedCode: fixture(t, e, "correct"), Limitations: []string{"未执行测试"}}
}
func TestStrictDiagnosisRejectsFabricatedResults(t *testing.T) {
	e := catalog(t).List()[0]
	hits := map[string]rag.Hit{"context.md#0": {Source: "context.md", Index: 0, Text: "上下文取消需要协作处理。"}}
	base := diagnosis(t, e)
	if _, err := ValidateDiagnosis(e, e.StarterCode, string(encode(base)), hits); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"fake-source", "fake-quote", "bad-line", "bad-code", "fake-inline"} {
		t.Run(kind, func(t *testing.T) {
			d := diagnosis(t, e)
			switch kind {
			case "fake-source":
				d.Evidence[0].Source = "invented.md"
			case "fake-quote":
				d.Evidence[0].Quote = "不存在"
			case "bad-line":
				d.Findings[0].EndLine = 10000
			case "bad-code":
				d.ProposedCode = "package exercise\nfunc Wrong(){}"
			case "fake-inline":
				d.Summary = "参见[fake.md#8]"
			}
			if _, err := ValidateDiagnosis(e, e.StarterCode, string(encode(d)), hits); err == nil {
				t.Fatal("accepted fabricated result")
			}
		})
	}
	for _, raw := range []string{"```json\n" + string(encode(base)) + "\n```", string(encode(base)) + "{}", `{"summary":"test","extra":1}`} {
		if _, err := ValidateDiagnosis(e, e.StarterCode, raw, hits); err == nil {
			t.Fatal("accepted malformed output")
		}
	}
}

type scriptedProvider struct {
	calls  int
	result Diagnosis
	fail   bool
}

func (p *scriptedProvider) Chat(ctx context.Context, in *llm.ChatRequest) (*llm.ChatResponse, error) {
	p.calls++
	if p.fail {
		return nil, errors.New("upstream failure")
	}
	if p.calls == 1 {
		return &llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "exercise", Name: "exercise_read", ArgsJSON: "{}"}, {ID: "search", Name: "knowledge_search", ArgsJSON: `{"query":"取消传播"}`}}, FinishReason: "tool_calls", Usage: llm.Usage{PromptTokens: 10}}, nil
	}
	return &llm.ChatResponse{Content: string(encode(p.result)), FinishReason: "stop", Usage: llm.Usage{CompletionTokens: 12}}, nil
}

type fakeSearch struct {
	fail bool
	kb   string
}

func (s *fakeSearch) Search(_ context.Context, kb, _ string, _ int) ([]rag.Hit, error) {
	s.kb = kb
	if s.fail {
		return nil, errors.New("unavailable")
	}
	return []rag.Hit{{Source: "context.md", Index: 0, Text: "上下文取消需要协作处理。"}}, nil
}

type failedRecall struct{}

func (failedRecall) Recall(context.Context, string) (string, int64, error) {
	return "", 0, errors.New("preference memory unavailable")
}
func TestAgentDiagnosisToolsIsolationAndMemoryDegradation(t *testing.T) {
	e := catalog(t).List()[0]
	search := &fakeSearch{}
	provider := &scriptedProvider{result: diagnosis(t, e)}
	analyzer := &AgentAnalyzer{Provider: provider, Searcher: search, Memory: failedRecall{}}
	var events []harness.Event
	d, usage, err := analyzer.Analyze(context.Background(), e, Attempt{Submit: Submit{Code: e.StarterCode}}, func(ev harness.Event) error { events = append(events, ev); return nil })
	if err != nil || d == nil || search.kb != "go-code-practice" || usage.PromptTokens != 10 || usage.CompletionTokens != 12 {
		t.Fatalf("%#v %#v %v", d, usage, err)
	}
	if events[0].Type != "inspection" {
		t.Fatal("AST not first")
	}
	for _, ev := range events {
		if ev.Type == "delta" {
			t.Fatal("raw JSON streamed into product")
		}
	}
	if provider.calls != 2 {
		t.Fatal("executor did not iterate")
	}
	provider = &scriptedProvider{fail: true}
	analyzer.Provider = provider
	if _, _, err = analyzer.Analyze(context.Background(), e, Attempt{Submit: Submit{Code: e.StarterCode}}, nil); err == nil {
		t.Fatal("accepted upstream failure")
	}
	provider = &scriptedProvider{result: diagnosis(t, e)}
	provider.result.Evidence = []Evidence{}
	search.fail = true
	analyzer.Provider = provider
	d, _, err = analyzer.Analyze(context.Background(), e, Attempt{Submit: Submit{Code: e.StarterCode}}, nil)
	if err != nil || len(d.Limitations) < 2 {
		t.Fatalf("RAG failure not marked: %v %#v", err, d)
	}
}
func TestBoundToolCannotChooseOtherSource(t *testing.T) {
	tool := boundTool{name: "exercise_read", read: func() any { return "bound" }}
	if _, err := tool.Execute(context.Background(), `{"attempt_id":"other"}`); err == nil {
		t.Fatal("accepted client-controlled source")
	}
}
func TestOutputLimitAndTerminals(t *testing.T) {
	b := &boundedOutput{}
	p := []byte(strings.Repeat("x", 100000))
	if n, err := b.Write(p); err != nil || n != len(p) {
		t.Fatal(n, err)
	}
	out, truncated := b.Text()
	if !truncated || len(out) != MaxOutputBytes {
		t.Fatal("unbounded output")
	}
	invalid := &boundedOutput{}
	_, _ = invalid.Write([]byte(strings.Repeat("\xffx", MaxOutputBytes/2)))
	sanitized, cut := invalid.Text()
	if !cut || len(sanitized) > MaxOutputBytes {
		t.Fatal("UTF8 repair exceeded limit")
	}
	for _, v := range []struct {
		output    string
		exit      int
		truncated bool
		want      string
	}{{`{"Action":"pass","Package":"exercise"}`, 0, false, "passed"}, {`{"Action":"pass","Package":"exercise"}`, 0, true, "infrastructure_error"}, {`{"Action":"fail","Package":"exercise"}`, 1, false, "failed"}, {"WARNING: DATA RACE", 1, false, "race_failed"}, {"panic: test timed out after 10s", 1, false, "timeout"}, {"FAIL exercise [build failed]", 1, false, "compile_failed"}, {"", 0, false, "infrastructure_error"}, {"fork/exec /tmp/exercise.test: permission denied\n{\"Action\":\"fail\",\"Package\":\"exercise\"}", 1, false, "infrastructure_error"}} {
		if got := ClassifyTestOutput(v.output, v.exit, v.truncated); got != v.want {
			t.Fatalf("%s != %s", got, v.want)
		}
	}
}
func TestThreeExerciseCaseManifests(t *testing.T) {
	count := 0
	for _, e := range catalog(t).List() {
		b, err := assets.Files.ReadFile(e.ID + "/" + e.Version + "/cases.json")
		if err != nil {
			t.Fatal(err)
		}
		var cases []struct{ Name, File, Expected string }
		if err = json.Unmarshal(b, &cases); err != nil {
			t.Fatal(err)
		}
		for _, v := range cases {
			if _, err = assets.Files.ReadFile(e.ID + "/" + e.Version + "/" + v.File); err != nil {
				t.Fatal(err)
			}
			count++
		}
	}
	if count < 15 {
		t.Fatal("need at least 15 cases")
	}
}

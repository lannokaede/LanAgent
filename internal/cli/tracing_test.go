package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
)

func TestCLIHybridMetadataAndTelemetryLifecycle(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream         bool
			ResponseFormat any
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		if !req.Stream {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"route\":\"direct\"}"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
			return
		}
		attempts++
		if attempts == 1 {
			w.WriteHeader(503)
			fmt.Fprint(w, "upstream-secret")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer-secret\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	cfg, work, state := t.TempDir(), t.TempDir(), t.TempDir()
	trace := filepath.Join(t.TempDir(), "trace.jsonl")
	body := fmt.Sprintf("llm:\n  timeout: 3s\n  retry:\n    base_delay: 1ms\nrouting:\n  mode: hybrid\n  timeout: 2s\ntelemetry:\n  enabled: true\n  file: %q\n", trace)
	os.WriteFile(filepath.Join(cfg, "config.yaml"), []byte(body), 0600)
	t.Setenv("LANAGENT_API_CONFIG", filepath.Join(cfg, "none.json"))
	t.Setenv("LLM_BASE_URL", srv.URL)
	t.Setenv("LLM_MODEL", "fixture")
	t.Setenv("LLM_API_KEY", "key-secret")
	previous := otel.GetTracerProvider()
	var out, status bytes.Buffer
	if err := Run(context.Background(), []string{"-config", cfg, "-workspace", work, "-state-dir", state, "-session", "trace", "-prompt", "讲解 channel 的语义 prompt-secret"}, strings.NewReader(""), &out, &status); err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() != previous {
		t.Fatal("closed session left global provider installed")
	}
	raw, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"key-secret", "prompt-secret", "answer-secret", "upstream-secret"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("telemetry leaked input or credentials")
		}
	}
	for _, span := range []string{"agent.task", "agent.classify", "llm.call", "llm.attempt"} {
		if !bytes.Contains(raw, []byte(span)) {
			t.Fatal("span missing", span)
		}
	}
	// Validate actual exported parent links, not only the presence of span names.
	type exportedSpan struct {
		Name        string
		SpanContext struct{ TraceID, SpanID string }
		Parent      struct{ SpanID string }
	}
	exported := []exportedSpan{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	for {
		var span exportedSpan
		err := decoder.Decode(&span)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		exported = append(exported, span)
	}
	spans := map[string]exportedSpan{}
	roots := map[string]bool{}
	for _, span := range exported {
		spans[span.SpanContext.SpanID] = span
		if span.Name == "agent.task" {
			roots[span.SpanContext.TraceID] = true
		}
	}
	for _, span := range exported {
		if !roots[span.SpanContext.TraceID] {
			t.Fatal("span disconnected from task", span.Name)
		}
		if span.Name != "agent.task" {
			if _, ok := spans[span.Parent.SpanID]; !ok {
				t.Fatal("missing parent span", span.Name)
			}
		}
	}
	var records []map[string]any
	journal, err := os.ReadFile(filepath.Join(state, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(journal), []byte("\n")) {
		var row map[string]any
		if json.Unmarshal(line, &row) != nil {
			t.Fatal("bad journal")
		}
		records = append(records, row)
	}
	statsFound := false
	for _, row := range records {
		event := row["event"].(map[string]any)
		if event["Type"] == "run_statistics" {
			data := event["Data"].(map[string]any)
			statsFound = true
			if data["attempts"] != float64(3) || data["unknown_usage_attempts"] != float64(1) || data["reported_usage"].(map[string]any)["prompt_tokens"] != float64(13) {
				t.Fatal("incorrect request statistics", data)
			}
		}
	}
	if !statsFound || attempts != 2 {
		t.Fatal("missing run statistics or retry", attempts)
	}
}

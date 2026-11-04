package telemetry

import (
	"context"
	"errors"
	"go.opentelemetry.io/otel"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAsyncTracePropagationAndSanitizedError(t *testing.T) {
	old := otel.GetTracerProvider()
	defer otel.SetTracerProvider(old)
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	shutdown, err := Init(context.Background(), Config{Enabled: true, File: path})
	if err != nil {
		t.Fatal(err)
	}
	ctx, request := Start(context.Background(), "http.request")
	restored := Restore(context.Background(), Capture(ctx))
	taskctx, task := Start(restored, "agent.task")
	_, model := Start(taskctx, "llm.call")
	if request.SpanContext().TraceID() != task.SpanContext().TraceID() || request.SpanContext().TraceID() != model.SpanContext().TraceID() {
		t.Fatal("queue lost parent trace")
	}
	Finish(model, errors.New("secret prompt and key"))
	task.End()
	request.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret prompt and key") || !strings.Contains(string(raw), "operation failed") || !strings.Contains(string(raw), "agent.task") {
		t.Fatal("trace missing or contains raw error")
	}
}

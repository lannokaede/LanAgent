package llm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lannokaede/LanAgent/internal/harness"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/tool"
)

type terminalFixtureTool struct{ calls *atomic.Int64 }

func (terminalFixtureTool) Spec() llm.ToolDef {
	return llm.ToolDef{Name: "effect", Parameters: map[string]any{"type": "object"}}
}
func (t terminalFixtureTool) Execute(context.Context, string) (string, error) {
	t.calls.Add(1)
	return "effect completed", nil
}

func TestNonStreamIncompleteResponsesCannotCompleteOrExecuteTools(t *testing.T) {
	for _, reason := range []string{"length", "content_filter"} {
		for _, tools := range []bool{false, true} {
			name := reason + "/text"
			if tools {
				name = reason + "/tool"
			}
			t.Run(name, func(t *testing.T) {
				var requests, effects atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					message := map[string]any{"role": "assistant", "content": "incomplete answer"}
					if tools {
						message["tool_calls"] = []any{map[string]any{
							"id": "partial", "type": "function",
							"function": map[string]any{"name": "effect", "arguments": "{}"},
						}}
					}
					json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": reason}}})
				}))
				defer server.Close()
				provider, err := llm.NewOpenAIProvider(server.URL, "fixture", "fixture-only-key")
				if err != nil {
					t.Fatal(err)
				}
				defer provider.Close()
				gateway, err := llm.NewGateway(provider, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				registry := tool.NewRegistry()
				if err := registry.Register(terminalFixtureTool{calls: &effects}); err != nil {
					t.Fatal(err)
				}
				runner := harness.NewRunner(gateway, registry, 3)
				_, answer, err := runner.Run(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "fixture"}})
				if err == nil || !strings.Contains(err.Error(), "incomplete or blocked") || answer != nil {
					t.Fatalf("incomplete response was accepted: answer=%+v error=%v", answer, err)
				}
				if requests.Load() != 1 || effects.Load() != 0 {
					t.Fatalf("incomplete response retried or executed: requests=%d effects=%d", requests.Load(), effects.Load())
				}
			})
		}
	}
}

func TestNonStreamOmittedReasonRemainsCompatible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "compatible answer"}}}})
	}))
	defer server.Close()
	provider, err := llm.NewOpenAIProvider(server.URL, "fixture", "fixture-only-key")
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	response, err := provider.Chat(context.Background(), &llm.ChatRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "fixture"}}})
	if err != nil || response == nil || response.Content != "compatible answer" {
		t.Fatalf("omitted finish reason lost compatibility: response=%+v error=%v", response, err)
	}
}

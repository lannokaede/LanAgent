package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStructuredOutputsOnBothTransports(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					ResponseFormat struct {
						Type       string `json:"type"`
						JSONSchema struct {
							Strict bool   `json:"strict"`
							Name   string `json:"name"`
						} `json:"json_schema"`
					} `json:"response_format"`
					MaxTokens int `json:"max_completion_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.ResponseFormat.Type != "json_schema" || !body.ResponseFormat.JSONSchema.Strict || body.ResponseFormat.JSONSchema.Name != "result" || body.MaxTokens != 300 {
					t.Errorf("schema or output reserve lost: %+v", body)
				}
				if stream {
					time.Sleep(25 * time.Millisecond)
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
					w.(http.Flusher).Flush()
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"{}\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}]}`)
				}
			}))
			defer srv.Close()
			p, err := NewOpenAIProvider(srv.URL, "test-model", "test-key")
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			req := &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hello"}}, MaxTokens: 300, ResponseFormat: &ResponseFormat{Type: "json_schema", JSONSchema: JSONSchema{Name: "result", Strict: true, Schema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}, "required": []string{}}}}}
			var out *ChatResponse
			if stream {
				out, err = p.StreamChat(context.Background(), req, func(string) error { return nil })
			} else {
				out, err = p.Chat(context.Background(), req)
			}
			if err != nil {
				t.Fatal(err)
			}
			if stream && out.TTFT < 20*time.Millisecond {
				t.Fatalf("TTFT omitted response header wait: %s", out.TTFT)
			}
			if !stream && out.TTFT != 0 {
				t.Fatal("nonstreaming TTFT must be unavailable")
			}
		})
	}
}

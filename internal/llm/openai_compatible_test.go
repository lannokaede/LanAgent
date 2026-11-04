package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 用本地服务验证框架真正发送的请求，不依赖真实 API 或本地密钥。
func TestLangChainOpenAIWireProtocol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("bad path/auth")
		}
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role, Content string
				ToolCalls     []json.RawMessage `json:"tool_calls"`
				ToolCallID    string            `json:"tool_call_id"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name       string
					Parameters map[string]any
				}
			} `json:"tools"`
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Model != "test-model" || req.Stream || len(req.Messages) != 4 || req.Messages[1].Content != "先计算" || len(req.Messages[1].ToolCalls) != 2 || req.Messages[2].ToolCallID != "one" || req.Messages[3].ToolCallID != "two" {
			t.Errorf("bad wire messages: %+v", req)
		}
		if len(req.Tools) != 1 || req.Tools[0].Function.Parameters["type"] != "object" {
			t.Errorf("schema lost: %+v", req.Tools)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"答案","tool_calls":[{"id":"next","type":"function","function":{"name":"add","arguments":"{\"a\":1,\"b\":2}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":17,"completion_tokens":3,"total_tokens":20}}`)
	}))
	defer srv.Close()
	p, err := NewOpenAIProvider(srv.URL+"/v1/", "test-model", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Chat(context.Background(), &ChatRequest{
		Messages: []Message{{Role: RoleSystem, Content: "系统"}, {Role: RoleAssistant, Content: "先计算", ToolCalls: []ToolCall{{ID: "one", Name: "add", ArgsJSON: `{"a":2,"b":3}`}, {ID: "two", Name: "add", ArgsJSON: `{"a":4,"b":5}`}}}, {Role: RoleTool, ToolCallID: "one", Content: "5"}, {Role: RoleTool, ToolCallID: "two", Content: "9"}},
		Tools:    []ToolDef{{Name: "add", Description: "加法", Parameters: map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "number"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "答案" || resp.FinishReason != "tool_calls" || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "next" || resp.Usage.PromptTokens != 17 || resp.Usage.CompletionTokens != 3 {
		t.Fatalf("bad mapped response: %+v", resp)
	}
}

func TestLangChainOpenAIErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"auth", "private-upstream-body", "HTTP 401", 401},
		{"json", "invalid", "", 200},
		{"empty", `{"choices":[]}`, "", 200},
		{"missing-id", `{"choices":[{"message":{"tool_calls":[{"type":"function","function":{"name":"add","arguments":"{}"}}]}}]}`, "invalid tool call", 200},
		{"large", `{"choices":[{"message":{"content":"` + strings.Repeat("x", (4<<20)+1) + `"}}]}`, "size limit", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			p, err := NewOpenAIProvider(srv.URL, "test", "test-key")
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Chat(context.Background(), &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hello"}}})
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "private-upstream-body") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLangChainOpenAICancel(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 读完请求体，使 net/http 可以检测客户端断开。
		var body any
		json.NewDecoder(r.Body).Decode(&body)
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer srv.Close()
	p, err := NewOpenAIProvider(srv.URL, "test", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := p.Chat(ctx, &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hello"}}})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel lost: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client did not cancel")
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream context not canceled")
	}
}

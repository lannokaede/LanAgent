package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamTextBeforeUpstreamCompletion(t *testing.T) {
	received := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-received:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"！\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	p, _ := NewOpenAIProvider(server.URL, "test", "test-key")
	var text strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := p.StreamChat(ctx, &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hello"}}}, func(s string) error {
		if text.Len() == 0 {
			close(received)
		}
		text.WriteString(s)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "你好！" || response.Content != text.String() || response.Usage.PromptTokens != 3 {
		t.Fatalf("stream result: %+v / %q", response, text.String())
	}
}

func TestStreamInterleavedToolArguments(t *testing.T) {
	stream := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"ad","arguments":"{\"a\":"}},{"index":1,"id":"b","type":"function","function":{"name":"add","arguments":"{\"a\":3,"}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"d","arguments":"1,\"b\":2}"}},{"index":1,"function":{"arguments":"\"b\":4}"}}]},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	response, err := decodeChatStream(context.Background(), strings.NewReader(stream), func(s string) error { t.Fatalf("tool JSON leaked: %s", s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(response.ToolCalls) != 2 || response.ToolCalls[0].Name != "add" || response.ToolCalls[0].ArgsJSON != `{"a":1,"b":2}` || response.ToolCalls[1].ArgsJSON != `{"a":3,"b":4}` {
		t.Fatalf("bad tools: %+v", response)
	}
}

func TestStreamRejectsIncompleteOrInvalidTerminal(t *testing.T) {
	for _, wire := range []string{
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half\"}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n",
		"data: {\"error\":{\"message\":\"private-upstream-detail\"}}\n\n",
		"data: broken-json\n\ndata: [DONE]\n\n",
	} {
		_, err := decodeChatStream(context.Background(), strings.NewReader(wire), func(string) error { return nil })
		if err == nil || strings.Contains(err.Error(), "private-upstream-detail") {
			t.Fatalf("invalid stream accepted/leaked: %v", err)
		}
	}
}

func TestStreamCallbackErrorStopsReading(t *testing.T) {
	sentinel := errors.New("client disconnected")
	_, err := decodeChatStream(context.Background(), strings.NewReader("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"), func(string) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("callback error lost: %v", err)
	}
}

func TestStreamGatewayTimeoutIncludesBodyReading(t *testing.T) {
	stopped := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(stopped)
	}))
	defer server.Close()
	p, _ := NewOpenAIProvider(server.URL, "test", "test-key")
	g, _ := NewGateway(p, 100*time.Millisecond)
	text := ""
	_, err := g.StreamChat(context.Background(), &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "timeout"}}}, func(s string) error { text += s; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || text != "first" {
		t.Fatalf("timeout/body: %q %v", text, err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("upstream request leaked")
	}
}

func TestStreamSizeLimits(t *testing.T) {
	for _, wire := range []string{
		"data: " + strings.Repeat("a", 256<<10) + "\n\n",
		strings.Repeat(":heartbeat\n\n", 400000) + "data: [DONE]\n\n",
	} {
		_, err := decodeChatStream(context.Background(), strings.NewReader(wire), nil)
		if err == nil || (!strings.Contains(err.Error(), "size limit") && !strings.Contains(err.Error(), "token too long")) {
			t.Fatalf("unbounded stream: %v", err)
		}
	}
}

package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

var _ StreamProvider = (*OpenAIProvider)(nil)

// 模块 1：OpenAI-compatible 上游流。框架 v0.1.15 的流式回调把工具 JSON
// 也当作 chunk，且交错工具片段按末尾工具合并。本适配器按 index 收集，
// 只转发 delta.content；Agent 的执行顺序由 LangGraphGo 状态图驱动。
// 非流式 Chat 继续使用框架客户端；两条路径共享鉴权配置及有界 HTTP client。
func (p *OpenAIProvider) StreamChat(ctx context.Context, req *ChatRequest, emit func(string) error) (*ChatResponse, error) {
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("gen_ai.request.model", p.modelName))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil || len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages cannot be empty")
	}
	if err := ValidateMessages(req.Messages); err != nil {
		return nil, err
	}
	type function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	type call struct {
		ID       string   `json:"id"`
		Type     string   `json:"type"`
		Function function `json:"function"`
	}
	type message struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCalls  []call `json:"tool_calls,omitempty"`
		ToolCallID string `json:"tool_call_id,omitempty"`
	}
	type definition struct {
		Type     string  `json:"type"`
		Function ToolDef `json:"function"`
	}
	payload := struct {
		ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
		MaxTokens      int             `json:"max_completion_tokens,omitempty"`
		Model          string          `json:"model"`
		Messages       []message       `json:"messages"`
		Tools          []definition    `json:"tools,omitempty"`
		Stream         bool            `json:"stream"`
		StreamOptions  map[string]bool `json:"stream_options"`
	}{Model: p.modelName, Stream: true, StreamOptions: map[string]bool{"include_usage": true}}
	payload.ResponseFormat, payload.MaxTokens = req.ResponseFormat, req.MaxTokens
	for _, m := range req.Messages {
		wire := message{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, c := range m.ToolCalls {
			wire.ToolCalls = append(wire.ToolCalls, call{ID: c.ID, Type: "function", Function: function{Name: c.Name, Arguments: c.ArgsJSON}})
		}
		payload.Messages = append(payload.Messages, wire)
	}
	for _, d := range req.Tools {
		payload.Tools = append(payload.Tools, definition{Type: "function", Function: d})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode stream request: %w", err)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "text/event-stream")
	r.Header.Set("Authorization", "Bearer "+p.apiKey)
	start := time.Now()
	resp, err := p.client.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var upstream *UpstreamError
		if errors.As(err, &upstream) {
			return nil, upstream
		}
		return nil, fmt.Errorf("chat stream request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &UpstreamError{StatusCode: resp.StatusCode}
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" {
		return nil, fmt.Errorf("chat API did not return an event stream")
	}
	var ttft time.Duration
	response, err := decodeChatStream(ctx, resp.Body, emit, func() {
		ttft = time.Since(start)
		trace.SpanFromContext(ctx).SetAttributes(attribute.Float64("gen_ai.response.ttft_seconds", ttft.Seconds()))
	})
	if response != nil {
		response.TTFT = ttft
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return response, err
}

// 模块 2：SSE 帧解析。网络分包不等于 SSE 帧；Scanner 先收完整行，空行
// 才提交事件。支持 CRLF、注释和多行 data；单帧最大 256 KiB，总体 4 MiB。
// [DONE] 和 finish_reason 缺一不可，EOF/截断/长度上限不冒充成功。
func decodeChatStream(ctx context.Context, reader io.Reader, emit func(string) error, first ...func()) (*ChatResponse, error) {
	scanner := bufio.NewScanner(&boundedBody{ReadCloser: io.NopCloser(reader), remaining: 4 << 20})
	scanner.Buffer(make([]byte, 4096), 256<<10)
	response := &ChatResponse{}
	toolParts := map[int]*ToolCall{}
	var content strings.Builder
	var data []string
	frameSize := 0
	finished := false
	start := time.Now()
	observed := false
	observe := func() {
		if !observed {
			observed = true
			response.TTFT = time.Since(start)
			for _, fn := range first {
				fn()
			}
		}
	}
	process := func(raw string) (bool, error) {
		if raw == "[DONE]" {
			if !finished {
				return false, fmt.Errorf("stream ended without finish_reason")
			}
			return true, nil
		}
		var chunk struct {
			Error   json.RawMessage `json:"error"`
			Usage   *Usage          `json:"usage"`
			Choices []struct {
				Index        int    `json:"index"`
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Refusal      string          `json:"refusal"`
					Content      string          `json:"content"`
					FunctionCall json.RawMessage `json:"function_call"`
					ToolCalls    []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if !utf8.ValidString(raw) || json.Unmarshal([]byte(raw), &chunk) != nil {
			return false, fmt.Errorf("invalid upstream stream JSON")
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return false, fmt.Errorf("upstream stream error")
		}
		if chunk.Usage != nil {
			response.Usage = *chunk.Usage
			response.UsageKnown = true
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Refusal != "" {
				return false, fmt.Errorf("model refused structured response")
			}
			if choice.Index != 0 {
				return false, fmt.Errorf("stream supports one choice only")
			}
			if finished {
				return false, fmt.Errorf("stream contains choices after finish")
			}
			if len(choice.Delta.FunctionCall) > 0 && string(choice.Delta.FunctionCall) != "null" {
				return false, fmt.Errorf("legacy function_call is unsupported")
			}
			if text := choice.Delta.Content; text != "" {
				observe()
				content.WriteString(text)
				if emit != nil {
					if err := emit(text); err != nil {
						return false, fmt.Errorf("stream callback: %w", err)
					}
				}
			}
			for _, delta := range choice.Delta.ToolCalls {
				if delta.ID != "" || delta.Function.Name != "" || delta.Function.Arguments != "" {
					observe()
				}
				if delta.Index < 0 || delta.Index >= 64 || (delta.Type != "" && delta.Type != "function") {
					return false, fmt.Errorf("invalid tool delta")
				}
				part := toolParts[delta.Index]
				if part == nil {
					part = &ToolCall{}
					toolParts[delta.Index] = part
				}
				part.ID += delta.ID
				part.Name += delta.Function.Name
				part.ArgsJSON += delta.Function.Arguments
			}
			if choice.FinishReason != "" {
				if choice.FinishReason != "stop" && choice.FinishReason != "tool_calls" {
					return false, fmt.Errorf("model response incomplete or blocked")
				}
				finished = true
				response.FinishReason = choice.FinishReason
			}
		}
		return false, nil
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := scanner.Text()
		if line != "" {
			frameSize += len(line) + 1
			if frameSize > 256<<10 {
				return nil, fmt.Errorf("stream frame exceeds size limit")
			}
			if value, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimPrefix(value, " "))
			}
			continue
		}
		frameSize = 0
		if len(data) == 0 {
			continue
		}
		done, err := process(strings.Join(data, "\n"))
		data = nil
		if err != nil {
			return nil, err
		}
		if done {
			for index := 0; index < len(toolParts); index++ {
				part := toolParts[index]
				if part == nil || part.ID == "" || part.Name == "" {
					return nil, fmt.Errorf("incomplete tool call")
				}
				response.ToolCalls = append(response.ToolCalls, *part)
			}
			if (response.FinishReason == "tool_calls") != (len(response.ToolCalls) > 0) {
				return nil, fmt.Errorf("finish_reason does not match tool calls")
			}
			response.Content = content.String()
			return response, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read chat stream: %w", err)
	}
	return nil, fmt.Errorf("chat stream truncated before [DONE]")
}

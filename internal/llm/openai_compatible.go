package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/lannokaede/LanAgent/internal/engineering"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/openai"
)

// 模块 1：厂商适配。非流式调用使用 LangChainGo 客户端，流式调用见 stream.go。
// 两者共享配置与有界 HTTP client；Provider 不保存对话历史。
type OpenAIProvider struct {
	model                      *openai.LLM
	baseURL, modelName, apiKey string
	client                     *http.Client
}

func NewOpenAIProvider(baseURL, model, apiKey string) (*OpenAIProvider, error) {
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(model) == "" || strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("base URL, model and API key are required")
	}
	transport := engineering.Transport()
	transport.ResponseHeaderTimeout = 60 * time.Second
	client := &http.Client{Timeout: 60 * time.Second, Transport: boundedTransport{base: transport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	m, err := openai.New(openai.WithBaseURL(strings.TrimRight(baseURL, "/")),
		openai.WithModel(model), openai.WithToken(apiKey),
		openai.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("initialize LangChainGo OpenAI model: %w", err)
	}
	return &OpenAIProvider{model: m, baseURL: strings.TrimRight(baseURL, "/"), modelName: model, apiKey: apiKey, client: client}, nil
}

// 模块 2：一次模型调用。Gateway 在外层控制单次超时；状态图控制是否再次调用。
// 使用现代 tools/tool_calls 格式，真实的 JSON Schema 不降级为 __arg1 字符串。
func (p *OpenAIProvider) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("gen_ai.request.model", p.modelName))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil || len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages cannot be empty")
	}
	messages, err := ToLangChainMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	model := p.model
	if req.ResponseFormat != nil {
		// Per-request configuration avoids mutating a client shared by workers.
		var format openai.ResponseFormat
		raw, e := json.Marshal(req.ResponseFormat)
		if e != nil {
			return nil, e
		}
		if e = json.Unmarshal(raw, &format); e != nil {
			return nil, e
		}
		model, err = openai.New(openai.WithBaseURL(p.baseURL), openai.WithModel(p.modelName), openai.WithToken(p.apiKey), openai.WithHTTPClient(p.client), openai.WithResponseFormat(&format))
		if err != nil {
			return nil, err
		}
	}
	options := []llms.CallOption{llms.WithTools(ToLangChainTools(req.Tools))}
	if req.MaxTokens > 0 {
		options = append(options, llms.WithMaxTokens(req.MaxTokens))
	}
	result, err := model.GenerateContent(ctx, messages, options...)
	if err != nil {
		// 框架会把网络取消转换成普通文本错误；补回可 errors.Is 判断的 context。
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("LangChainGo chat: %w", err)
	}
	return FromLangChainResponse(result)
}

// 模块 3：HTTP 边界。沿用最大 4 MiB 响应限制；非 2xx 只暴露状态码，
// 不把上游原始错误正文（可能含内部信息）交给 CLI 或日志。
type boundedTransport struct{ base http.RoundTripper }

func (t boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		// 让框架按 HTTP 状态分类，同时替换掉不应输出的上游原始正文。
		body := fmt.Sprintf(`{"error":{"message":"chat API returned HTTP %d","type":"upstream_error"}}`, resp.StatusCode)
		resp.Body = io.NopCloser(strings.NewReader(body))
		resp.ContentLength = int64(len(body))
		return resp, nil
	}
	resp.Body = &boundedBody{ReadCloser: resp.Body, remaining: 4 << 20}
	return resp, nil
}

type boundedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.remaining == 0 {
		var probe [1]byte
		n, err := b.ReadCloser.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("chat response exceeds size limit")
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (p *OpenAIProvider) Close() { p.client.CloseIdleConnections() }
func (t boundedTransport) CloseIdleConnections() {
	if base, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		base.CloseIdleConnections()
	}
}

// Configure before sharing. A non-streaming model may send headers only after generation;
// its header budget must match Gateway rather than the shorter RAG transport budget.
func (p *OpenAIProvider) SetTimeout(timeout time.Duration) {
	p.client.Timeout = timeout + 5*time.Second
	if wrapper, ok := p.client.Transport.(boundedTransport); ok {
		if base, ok := wrapper.base.(*http.Transport); ok {
			base.ResponseHeaderTimeout = timeout
		}
	}
}

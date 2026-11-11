// Package llm 定义与编排框架无关的项目模型协议。
// 模块 1：Message/ToolCall 保留真实原始历史，assistant 的调用与 tool 的 ID 配对。
// 模块 2：ToolDef 是可调用能力的 JSON Schema；ChatRequest 是单次模型输入。
// 模块 3：ChatResponse 区分最终答案与工具请求，Usage 表示单次模型调用用量。
// agent 层累计多次 Usage；service/API 不直接依赖框架厂商对象。
// 非流式客户端类型转换统一在 langchain.go 中实现；types.go 不承担请求发送或记忆管理。
package llm

import "time"

// 工具调用请求
type ToolCall struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ArgsJSON string `json:"args"` //参数为json的字符串
}

// 上下文消息
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// 角色常量
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// 告诉模型工具的名字、作用以及参数
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// 一次模型调用的输入
type ChatRequest struct {
	Messages       []Message       `json:"messages"`
	Tools          []ToolDef       `json:"tools,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
}

// ResponseFormat is an explicit provider capability, never a prompt-only fallback.
type ResponseFormat struct {
	Type       string     `json:"type"`
	JSONSchema JSONSchema `json:"json_schema"`
}
type JSONSchema struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

// token消耗量
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// 模型调用的结果
type ChatResponse struct {
	// Zero means unavailable (e.g. non-streaming), never an invented TTFT.
	TTFT         time.Duration `json:"-"`
	Content      string        `json:"content"`
	ToolCalls    []ToolCall    `json:"tool_calls,omitempty"`
	FinishReason string        `json:"finish_reason"`
	Usage        Usage         `json:"usage"`
	UsageKnown   bool          `json:"usage_known,omitempty"`
}

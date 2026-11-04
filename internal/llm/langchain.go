package llm

import (
	"fmt"

	"github.com/tmc/langchaingo/llms"
)

// 模块 1：领域协议 ↔ LangChainGo 协议。HTTP、记忆和工具使用本项目 DTO，
// 框架使用 MessageContent/ContentResponse；这个文件是唯一的消息转换边界。
// 一条 assistant 消息中的文字和所有工具调用必须一起保留，不能拆成多条。
func ToLangChainMessages(messages []Message) ([]llms.MessageContent, error) {
	if err := ValidateMessages(messages); err != nil {
		return nil, err
	}
	result := make([]llms.MessageContent, 0, len(messages))
	for _, m := range messages {
		var role llms.ChatMessageType
		switch m.Role {
		case RoleSystem:
			role = llms.ChatMessageTypeSystem
		case RoleUser:
			role = llms.ChatMessageTypeHuman
		case RoleAssistant:
			role = llms.ChatMessageTypeAI
		case RoleTool:
			role = llms.ChatMessageTypeTool
		default:
			return nil, fmt.Errorf("unsupported message role %q", m.Role)
		}
		parts := []llms.ContentPart{}
		if m.Role == RoleTool {
			parts = append(parts, llms.ToolCallResponse{ToolCallID: m.ToolCallID, Content: m.Content})
		} else if m.Content != "" || len(m.ToolCalls) == 0 {
			parts = append(parts, llms.TextPart(m.Content))
		}
		for _, call := range m.ToolCalls {
			parts = append(parts, llms.ToolCall{ID: call.ID, Type: "function", FunctionCall: &llms.FunctionCall{Name: call.Name, Arguments: call.ArgsJSON}})
		}
		result = append(result, llms.MessageContent{Role: role, Parts: parts})
	}
	return result, nil
}

func ToLangChainTools(defs []ToolDef) []llms.Tool {
	result := make([]llms.Tool, 0, len(defs))
	for _, d := range defs {
		result = append(result, llms.Tool{Type: "function", Function: &llms.FunctionDefinition{Name: d.Name, Description: d.Description, Parameters: d.Parameters}})
	}
	return result
}

func FromLangChainResponse(result *llms.ContentResponse) (*ChatResponse, error) {
	if result == nil || len(result.Choices) == 0 || result.Choices[0] == nil {
		return nil, fmt.Errorf("chat response contains no choices")
	}
	c := result.Choices[0]
	r := &ChatResponse{Content: c.Content, FinishReason: c.StopReason}
	for _, call := range c.ToolCalls {
		if call.ID == "" || call.FunctionCall == nil || call.FunctionCall.Name == "" {
			return nil, fmt.Errorf("chat response contains invalid tool call")
		}
		r.ToolCalls = append(r.ToolCalls, ToolCall{ID: call.ID, Name: call.FunctionCall.Name, ArgsJSON: call.FunctionCall.Arguments})
	}
	if c.FuncCall != nil && len(c.ToolCalls) == 0 {
		return nil, fmt.Errorf("legacy function_call is unsupported; use tool_calls with IDs")
	}
	r.Usage.PromptTokens, _ = c.GenerationInfo["PromptTokens"].(int)
	r.Usage.CompletionTokens, _ = c.GenerationInfo["CompletionTokens"].(int)
	return r, nil
}

package llm

import "fmt"

// ValidateMessages 验证项目消息协议，不依赖具体模型客户端或编排框架。
func ValidateMessages(messages []Message) error {
	for _, m := range messages {
		switch m.Role {
		case RoleSystem, RoleUser, RoleAssistant:
		case RoleTool:
			if m.ToolCallID == "" {
				return fmt.Errorf("tool message is missing tool_call_id")
			}
		default:
			return fmt.Errorf("unsupported message role %q", m.Role)
		}
		if len(m.ToolCalls) > 0 && m.Role != RoleAssistant {
			return fmt.Errorf("only assistant messages can contain tool_calls")
		}
		for _, call := range m.ToolCalls {
			if call.ID == "" || call.Name == "" {
				return fmt.Errorf("tool call is missing id or name")
			}
		}
	}
	return nil
}

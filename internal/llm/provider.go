// 模块：模型边界。Provider 表示一次非流式对话，不负责 ReAct 迭代或历史保存。
// OpenAIProvider 的非流式路径使用框架客户端，流式路径解析正文/工具片段；Gateway 装饰 Provider。
// LangGraphGo 模型节点直接调用 Provider，不暴露框架消息类型。
// 测试可注入脚本 Provider，不依赖真实模型。TODO：需要其他厂商时新增适配器。
package llm

import (
	"context"
)

type Provider interface {
	//Chat非流式对话
	Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
}

// StreamProvider 是可选能力；回调只携带正文，工具参数由适配器收齐后返回。
// 回调同步执行，返回错误即停止上游读取；返回值仍是完整响应，供记忆和图节点使用。
type StreamProvider interface {
	StreamChat(context.Context, *ChatRequest, func(string) error) (*ChatResponse, error)
}

// ChatWithStream 保留离线/非流式 Provider 的兼容性；其正文只能整段发送。
func ChatWithStream(ctx context.Context, p Provider, req *ChatRequest, emit func(string) error) (*ChatResponse, error) {
	if streaming, ok := p.(StreamProvider); ok {
		return streaming.StreamChat(ctx, req, emit)
	}
	response, err := p.Chat(ctx, req)
	if err != nil || response == nil {
		return response, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if response.Content != "" && emit != nil {
		if err := emit(response.Content); err != nil {
			return nil, err
		}
	}
	return response, nil
}

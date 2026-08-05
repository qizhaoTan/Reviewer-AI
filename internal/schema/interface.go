package schema

import "context"

type IProvider interface {
	Generate(ctx context.Context, msgs []Message, availableTools []ToolDefinition) (*Message, error)
}

type ISession interface {
	PushMessage(message ...Message)
	GetMemory() []Message
	// FullMemory 返回未经裁剪的全量会话历史副本。
	// 供 Web 查看器等需要完整上下文的只读场景使用。
	// 注意：此结果不可回填给大模型——它可能超出 token 预算。
	FullMemory() []Message
}

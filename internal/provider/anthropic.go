package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

// DefaultAnthropicMaxTokens 是 Anthropic Messages API 必填的 max_tokens 兜底值。
// 它限制的是「单次回复的输出上限」（不含输入/上下文），不是上下文窗口。
// OpenAI 协议下该参数可选，Anthropic 下却是硬性必填，故在 provider 层给一个默认，由调用方按需覆盖。
// 取 32000：Claude 4.x 系列单次输出官方支持到 64000，留足长回复（如完整审查报告）余量而不轻易截断，
// 又不顶满模型上限。配错小了会让长回复被截（stop_reason=max_tokens），故宁可给宽。
const DefaultAnthropicMaxTokens = 32000

// AnthropicProvider 是 schema.IProvider 的 Anthropic 原生实现，走 /v1/messages 协议。
// 与 OpenAIProvider 对等：在内部 schema.Message 与 Anthropic SDK 类型之间双向翻译，
// 并把思维链（thinking block）剥离到 ReasoningContent，绝不混入对外 Content。
type AnthropicProvider struct {
	client    anthropic.Client // 值类型，与 OpenAIProvider 保持一致
	model     string
	maxTokens int64
}

// NewAnthropicProvider 用显式参数构造，配置加载与校验由调用方（config 包）负责。
// maxTokens <= 0 时兜底为 DefaultAnthropicMaxTokens。baseURL 为空时用 SDK 默认官方端点。
func NewAnthropicProvider(apiKey, baseURL, model string, maxTokens int64) *AnthropicProvider {
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	if maxTokens <= 0 {
		maxTokens = DefaultAnthropicMaxTokens
	}
	return &AnthropicProvider{
		client:    anthropic.NewClient(opts...),
		model:     model,
		maxTokens: maxTokens,
	}
}

func (p *AnthropicProvider) Generate(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, error) {
	// 1. 翻译上下文消息。
	// Anthropic 协议与 OpenAI 的关键差异：
	//   - System 不进 messages 数组，单独走 System 字段；
	//   - 工具结果不是独立 role，而是 user 消息里的 tool_result content block；
	//   - 助手的工具调用是 assistant 消息里的 tool_use content block。
	var system []anthropic.TextBlockParam
	var anthropicMsgs []anthropic.MessageParam

	for _, msg := range msgs {
		switch msg.Role {
		case schema.RoleSystem:
			if msg.Content != "" {
				system = append(system, anthropic.TextBlockParam{Text: msg.Content})
			}

		case schema.RoleUser:
			if msg.ToolCallID != "" {
				// 工具执行结果：作为 tool_result block 放进一条 user 消息。
				block := anthropic.NewToolResultBlock(msg.ToolCallID, msg.Content, false)
				anthropicMsgs = append(anthropicMsgs, anthropic.NewUserMessage(block))
			} else {
				anthropicMsgs = append(anthropicMsgs, anthropic.NewUserMessage(anthropic.NewTextBlock(msg.Content)))
			}

		case schema.RoleAssistant:
			var blocks []anthropic.ContentBlockParamUnion
			if msg.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(msg.Content))
			}
			// 历史里的 ToolCalls 必须原样放回，以维系大模型的逻辑链（与 OpenAIProvider 同理）。
			for _, tc := range msg.ToolCalls {
				// Anthropic 的 tool_use.input 是结构化 JSON 对象，需把 Arguments 反序列化成 any 透传。
				var input any
				if len(tc.Arguments) > 0 {
					if err := json.Unmarshal(tc.Arguments, &input); err != nil {
						return nil, fmt.Errorf("反序列化历史 ToolCall 参数失败 (id=%s): %w", tc.ID, err)
					}
				}
				blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, input, tc.Name))
			}
			if len(blocks) > 0 {
				anthropicMsgs = append(anthropicMsgs, anthropic.NewAssistantMessage(blocks...))
			}
		}
	}

	// 2. 翻译工具定义。InputSchema 是手写的 JSON Schema（map），直接灌进 ToolInputSchemaParam.Properties。
	var anthropicTools []anthropic.ToolUnionParam
	for _, toolDef := range availableTools {
		schemaParam := anthropic.ToolInputSchemaParam{}
		if m, ok := toolDef.InputSchema.(map[string]interface{}); ok {
			// 顶层 JSON Schema 的 properties / required 拆进对应字段，其余键透传到 ExtraFields。
			if props, ok := m["properties"]; ok {
				schemaParam.Properties = props
			}
			if req, ok := m["required"].([]interface{}); ok {
				for _, r := range req {
					if s, ok := r.(string); ok {
						schemaParam.Required = append(schemaParam.Required, s)
					}
				}
			}
		}

		tool := anthropic.ToolParam{
			Name:        toolDef.Name,
			Description: anthropic.String(toolDef.Description),
			InputSchema: schemaParam,
		}
		anthropicTools = append(anthropicTools, anthropic.ToolUnionParam{OfTool: &tool})
	}

	// 3. 构建请求并发送。
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: p.maxTokens,
		Messages:  anthropicMsgs,
	}
	if len(system) > 0 {
		params.System = system
	}
	if len(anthropicTools) > 0 {
		params.Tools = anthropicTools
	}

	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("Anthropic API 请求失败: %w", err)
	}

	// 4. 将 API Response 反向翻译为内部 schema.Message。
	// content 是 block 数组：text → Content，thinking → ReasoningContent，tool_use → ToolCalls。
	resultMsg := &schema.Message{Role: schema.RoleAssistant}
	var answerParts, reasoningParts []string

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			answerParts = append(answerParts, block.Text)
		case "thinking":
			if block.Thinking != "" {
				reasoningParts = append(reasoningParts, block.Thinking)
			}
		case "tool_use":
			resultMsg.ToolCalls = append(resultMsg.ToolCalls, schema.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: json.RawMessage(block.Input), // 响应里 Input 已是 json.RawMessage
			})
		}
	}

	resultMsg.Content = strings.TrimSpace(strings.Join(answerParts, ""))
	resultMsg.ReasoningContent = strings.TrimSpace(strings.Join(reasoningParts, "\n"))

	return resultMsg, nil
}

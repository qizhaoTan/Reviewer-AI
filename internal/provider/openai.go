package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

// thinkTagRe 匹配内联思维链标签 <think>...</think>（跨行）。
// 部分开源模型（如 DeepSeek-R1 蒸馏版）会把推理过程直接写进 content，
// 而非放到独立的 reasoning_content 字段。
var thinkTagRe = regexp.MustCompile(`(?is)<think>.*?</think>`)

// splitReasoning 将模型返回的 content 拆分为「正式回答」和「思维链」。
// reasoningField 是从 reasoning_content 扩展字段提取的原始 JSON 字符串（可能为空）。
func splitReasoning(content, reasoningField string) (answer, reasoning string) {
	// 1. 优先采用独立字段里的思维链
	if reasoningField != "" {
		var rc string
		if err := json.Unmarshal([]byte(reasoningField), &rc); err == nil {
			reasoning = rc
		}
	}

	// 2. 剥离内联的 <think>...</think>，并入 reasoning
	if loc := thinkTagRe.FindString(content); loc != "" {
		inline := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(loc, "<think>"), "</think>"))
		if reasoning == "" {
			reasoning = inline
		} else if inline != "" {
			reasoning = reasoning + "\n" + inline
		}
		content = thinkTagRe.ReplaceAllString(content, "")
	}

	return strings.TrimSpace(content), reasoning
}

type OpenAIProvider struct {
	client openai.Client // 值类型，非指针
	model  string
}

// NewOpenAIProvider 用显式参数构造，配置加载与校验由调用方（config 包）负责。
func NewOpenAIProvider(apiKey, baseURL, model string) *OpenAIProvider {
	return &OpenAIProvider{
		client: openai.NewClient(option.WithAPIKey(apiKey), option.WithBaseURL(baseURL)),
		model:  model,
	}
}

func (p *OpenAIProvider) Generate(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, error) {
	openaiMsgs := translateOpenAIMessage(msgs)

	tools, toolChoice := translateOpenAITools(availableTools)

	// 构建请求并发送
	params := openai.ChatCompletionNewParams{
		Model:             p.model,
		Messages:          openaiMsgs,
		Tools:             tools,
		ToolChoice:        toolChoice,
		ParallelToolCalls: openai.Bool(true), // 并行工具调用，对于该项目默认开启
	}

	resp, err := p.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("OpenAI API 请求失败: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("API 返回了空的 Choices")
	}

	// 4. 将 API Response 反向翻译为内部 schema.Message
	choice := resp.Choices[0].Message

	// 部分模型把思维链放在 reasoning_content 扩展字段，部分则内联在 content 里。
	// 统一剥离，正式回答存 Content，思维链存 ReasoningContent，避免污染对外消息。
	var reasoningRaw string
	if f, ok := choice.JSON.ExtraFields["reasoning_content"]; ok {
		reasoningRaw = f.Raw()
	}
	answer, reasoning := splitReasoning(choice.Content, reasoningRaw)

	resultMsg := &schema.Message{
		Role:             schema.RoleAssistant,
		Content:          answer,
		ReasoningContent: reasoning,
	}

	for _, tc := range choice.ToolCalls {
		if tc.Type == "function" {
			resultMsg.ToolCalls = append(resultMsg.ToolCalls, schema.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: []byte(tc.Function.Arguments), // 提取 JSON 字符串字节
			})
		}
	}

	return resultMsg, nil
}

// 翻译上下文消息
func translateOpenAIMessage(msgs []schema.Message) []openai.ChatCompletionMessageParamUnion {
	openaiMsgs := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, msg := range msgs {
		switch msg.Role {
		case schema.RoleSystem:
			openaiMsgs = append(openaiMsgs, openai.SystemMessage(msg.Content))

		case schema.RoleUser:
			if msg.ToolCallID != "" {
				// 注意：v3 新版参数顺序是 (content, toolCallID)
				openaiMsgs = append(openaiMsgs, openai.ToolMessage(msg.Content, msg.ToolCallID))
			} else {
				openaiMsgs = append(openaiMsgs, openai.UserMessage(msg.Content))
			}

		case schema.RoleAssistant:
			astParam := openai.ChatCompletionAssistantMessageParam{}
			if msg.Content != "" {
				astParam.Content = openai.ChatCompletionAssistantMessageParamContentUnion{
					OfString: openai.String(msg.Content),
				}
			}

			// 【重要】如果历史包含 ToolCalls，必须原样放回，以维系大模型的逻辑链
			toolCalls := make([]openai.ChatCompletionMessageToolCallUnionParam, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				// OfFunction 对应 GetFunction()，字段类型严格要求为指针
				toolCalls = append(toolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID:   tc.ID,
						Type: "function",
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      tc.Name,
							Arguments: string(tc.Arguments),
						},
					},
				})
			}
			astParam.ToolCalls = toolCalls

			openaiMsgs = append(openaiMsgs, openai.ChatCompletionMessageParamUnion{
				OfAssistant: &astParam,
			})
		}
	}

	return openaiMsgs
}

// 翻译工具定义 (v3 新 API 特性适配)
func translateOpenAITools(availableTools []schema.ToolDefinition) ([]openai.ChatCompletionToolUnionParam, openai.ChatCompletionToolChoiceOptionUnionParam) {
	openaiTools := make([]openai.ChatCompletionToolUnionParam, 0, len(availableTools))
	for _, toolDef := range availableTools {
		var params shared.FunctionParameters

		// 尝试直接断言，如果不成功则通过 JSON 往返序列化来保证类型匹配
		if m, ok := toolDef.InputSchema.(map[string]interface{}); ok {
			params = m
		} else {
			// fallback：JSON 往返序列化
			b, _ := json.Marshal(toolDef.InputSchema)
			_ = json.Unmarshal(b, &params)
		}

		openaiTools = append(openaiTools, openai.ChatCompletionFunctionTool(
			shared.FunctionDefinitionParam{
				Name:        toolDef.Name,
				Description: openai.String(toolDef.Description),
				Parameters:  params,
			},
		))
	}

	var toolChoice openai.ChatCompletionToolChoiceOptionUnionParam
	for _, toolDef := range availableTools {
		if toolDef.Force {
			// 强制调用工具
			toolChoice = openai.ToolChoiceOptionFunctionToolChoice(
				openai.ChatCompletionNamedToolChoiceFunctionParam{Name: toolDef.Name},
			)
			break
		}
	}

	return openaiTools, toolChoice
}

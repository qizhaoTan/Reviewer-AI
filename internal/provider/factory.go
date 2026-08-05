package provider

import (
	"fmt"

	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

type Config struct {
	Provider  string `json:"provider"` // Provider 选择大模型协议实现："openai"（默认）或 "anthropic"。留空按 "openai" 处理。
	APIKey    string `json:"api_key"`
	BaseURL   string `json:"base_url"` // BaseURL 兼容协议厂商的 BaseURL。
	Model     string `json:"model"`
	MaxTokens int64  `json:"max_tokens"` // MaxTokens 仅 Anthropic 模式使用（该协议必填）；<=0 时由 provider 兜底。OpenAI 模式忽略。
}

func (c Config) Validate() error {
	var missing []string
	if c.APIKey == "" {
		missing = append(missing, "api_key")
	}
	if c.BaseURL == "" {
		missing = append(missing, "base_url")
	}
	if c.Model == "" {
		missing = append(missing, "model")
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺少必填配置项: %v", missing)
	}
	return nil
}

// New 按配置选择并实例化大模型 provider。
func New(cfg Config) (schema.IProvider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	switch cfg.Provider {
	case "OpenAI", "openai", "":
		return NewOpenAIProvider(cfg.APIKey, cfg.BaseURL, cfg.Model), nil
	case "Anthropic", "anthropic":
		return NewAnthropicProvider(cfg.APIKey, cfg.BaseURL, cfg.Model, cfg.MaxTokens), nil
	default:
		return nil, fmt.Errorf("不支持的大模型 provider: %s", cfg.Provider)
	}
}

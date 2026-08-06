package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/qizhaoTan/Reviewer-AI/internal/provider"
)

// defaultTimeout 在 ModelConfig.TimeoutSeconds 未配置或非法时兜底，
// 作为整个 tool loop（可能含多轮 Generate 调用）的总预算。
const defaultTimeout = 120 * time.Second

// ModelConfig 对应 config.json 里 "models" 下的一个具名模型条目。
// 字段是 provider.Config 的超集：context_window 目前尚无消费方，
// 仅解析保留（encoding/json 对未知字段本就会忽略，留着无害，方便未来接入上下文预算控制）。
type ModelConfig struct {
	Provider       string `json:"provider"`
	APIKey         string `json:"api_key"`
	BaseURL        string `json:"base_url"`
	Model          string `json:"model"`
	ContextWindow  int64  `json:"context_window"`
	MaxTokens      int64  `json:"max_tokens"`
	TimeoutSeconds int64  `json:"timeout_seconds"`
}

// Timeout 返回该模型一次审查运行的总超时预算；未配置或非法值时兜底为 defaultTimeout。
func (m ModelConfig) Timeout() time.Duration {
	if m.TimeoutSeconds <= 0 {
		return defaultTimeout
	}
	return time.Duration(m.TimeoutSeconds) * time.Second
}

// ToProviderConfig 转换为 provider.New 所需的 provider.Config。
func (m ModelConfig) ToProviderConfig() provider.Config {
	return provider.Config{
		Provider:  m.Provider,
		APIKey:    m.APIKey,
		BaseURL:   m.BaseURL,
		Model:     m.Model,
		MaxTokens: m.MaxTokens,
	}
}

// Roles 选择各角色当前生效的模型别名。primary 是唯一审查者角色；
// 未来子 Agent 并发审查可能引入更多角色（如 critic），故用 map 而非单字段。
type Roles struct {
	Primary string `json:"primary"`
}

// File 是 config.json 的顶层结构。
type File struct {
	Models map[string]ModelConfig `json:"models"`
	Roles  Roles                  `json:"roles"`
}

// DefaultPath 返回默认配置文件路径：$REVIEWER_AI_CONFIG（如设置）或 ~/.reviewer/config.json。
func DefaultPath() (string, error) {
	if p := os.Getenv("REVIEWER_AI_CONFIG"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".reviewer", "config.json"), nil
}

// Load 读取并解析指定路径的配置文件。纯函数，不读取任何全局状态，便于测试。
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse config file %q: %w", path, err)
	}
	return &f, nil
}

// Resolve 按 roles.primary 从 models 中查找当前生效的模型配置。
func (f *File) Resolve() (ModelConfig, error) {
	return f.resolveAlias(f.Roles.Primary)
}

// resolveAlias 按 models 中的具名别名（如 "openai-M2.7"）查找对应的模型配置。
func (f *File) resolveAlias(alias string) (ModelConfig, error) {
	if alias == "" {
		return ModelConfig{}, fmt.Errorf("resolve model alias: roles.primary is empty")
	}
	cfg, ok := f.Models[alias]
	if !ok {
		return ModelConfig{}, fmt.Errorf("resolve model alias: %q not found in models", alias)
	}
	return cfg, nil
}

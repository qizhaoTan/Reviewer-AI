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

// 复核阶段的默认参数。做成常量而非命令行 flag，是因为它们属于"这台机器上跑审查
// 的固有参数"，不是每次调用都要变的东西——跟 timeout_seconds 是同一类。
const (
	// defaultCritiqueConcurrency 是同时进行的单条意见复核数量。
	defaultCritiqueConcurrency = 10
	// defaultCritiqueMaxTurns 是单条意见复核时 Generate 调用的最大轮数。
	// 复核只需要查证一个具体的点，通常远用不完；给到和主循环同一量级是为了
	// 不让"轮数不够"成为复核失败的常见原因。
	defaultCritiqueMaxTurns = 30
)

// Critique 是复核阶段的可调参数，对应 config.json 里可选的 "critique" 段。
// 整段缺省时全部走默认值。
type Critique struct {
	// Concurrency 是同时复核的意见条数，<=0 时取默认值。
	Concurrency int `json:"concurrency"`
	// MaxTurns 是单条意见复核的最大轮数，<=0 时取默认值。
	MaxTurns int `json:"max_turns"`
}

// ConcurrencyOrDefault 返回生效的并发上限。
func (c Critique) ConcurrencyOrDefault() int {
	if c.Concurrency <= 0 {
		return defaultCritiqueConcurrency
	}
	return c.Concurrency
}

// MaxTurnsOrDefault 返回生效的单条复核轮数上限。
func (c Critique) MaxTurnsOrDefault() int {
	if c.MaxTurns <= 0 {
		return defaultCritiqueMaxTurns
	}
	return c.MaxTurns
}

// File 是 config.json 的顶层结构。
type File struct {
	Models   map[string]ModelConfig `json:"models"`
	Roles    Roles                  `json:"roles"`
	Critique Critique               `json:"critique"`

	// AutoStage 为 true 时，命令行审查在采集变更前先执行一次 `git add -A`，
	// 省掉"忘了 add 导致审查看到空暂存区"的来回。默认 false：自动改动索引
	// 是有副作用的行为，得由用户显式打开。
	AutoStage bool `json:"auto_stage"`

	// LanguagePrompt 是追加到每段 system prompt 末尾的语言约束，整段照抄进
	// 提示词。用指针是为了区分"配置里没写"（走默认的中文）和"显式配成空串"
	// （不加任何语言约束）。取值请走 LanguagePromptOrDefault。
	LanguagePrompt *string `json:"language_prompt"`
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

// defaultLanguagePrompt 是未配置 language_prompt 时追加到 system prompt 末尾的
// 语言约束。默认要求中文，是因为这个工具当前的使用者就是中文使用者；把整段
// 提示词而不是"语言名"放进配置，是为了让措辞（含括号里的母语强调）完全可控——
// 不同语言想强调的方式不一样，模板拼不出来。
const defaultLanguagePrompt = "- Your response needs to be in Chinese!!!（你的回复必须使用中文）"

// LanguagePromptOrDefault 返回生效的语言提示词。
//
// 未配置时兜底为 defaultLanguagePrompt；显式配成空字符串则表示"不加任何语言
// 约束"，让模型自己决定语言——这是个有意义的选择，所以用指针区分"没写"和
// "写了空串"。
func (f *File) LanguagePromptOrDefault() string {
	if f.LanguagePrompt == nil {
		return defaultLanguagePrompt
	}
	return *f.LanguagePrompt
}

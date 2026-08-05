package provider

import (
	"testing"
)

// New 应按 provider 类型返回对应实现，并对未知值报错。
func TestNew_Dispatch(t *testing.T) {
	if p, err := New(Config{APIKey: "k", BaseURL: "u", Model: "m"}); err != nil {
		t.Fatalf("默认（openai）不应报错: %v", err)
	} else if _, ok := p.(*OpenAIProvider); !ok {
		t.Fatalf("留空 provider 应得到 *OpenAIProvider，实际 %T", p)
	}

	if p, err := New(Config{Provider: "anthropic", BaseURL: "u", APIKey: "k", Model: "m"}); err != nil {
		t.Fatalf("anthropic 不应报错: %v", err)
	} else if _, ok := p.(*AnthropicProvider); !ok {
		t.Fatalf("provider=anthropic 应得到 *AnthropicProvider，实际 %T", p)
	}

	if _, err := New(Config{Provider: "gemini", APIKey: "k", Model: "m"}); err == nil {
		t.Fatal("未知 provider 应报错")
	}
}

// maxTokens <=0 时应兜底为 DefaultAnthropicMaxTokens。
func TestNewAnthropicProvider_MaxTokensFallback(t *testing.T) {
	p := NewAnthropicProvider("k", "", "m", 0)
	if p.maxTokens != DefaultAnthropicMaxTokens {
		t.Fatalf("maxTokens=0 应兜底为 %d，实际 %d", DefaultAnthropicMaxTokens, p.maxTokens)
	}

	p2 := NewAnthropicProvider("k", "", "m", 4096)
	if p2.maxTokens != 4096 {
		t.Fatalf("显式 maxTokens 应保留，实际 %d", p2.maxTokens)
	}
}

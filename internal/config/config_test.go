package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		content string // "" 表示不写文件（测试文件不存在的情况）
		wantErr bool
		want    *File
	}{
		{
			name: "valid file",
			content: `{
				"models": {
					"openai-M2.7": {"provider": "openai", "api_key": "k", "base_url": "https://example.test", "model": "m", "context_window": 128000, "max_tokens": 32000, "timeout_seconds": 180}
				},
				"roles": {"primary": "openai-M2.7"}
			}`,
			want: &File{
				Models: map[string]ModelConfig{
					"openai-M2.7": {
						Provider:       "openai",
						APIKey:         "k",
						BaseURL:        "https://example.test",
						Model:          "m",
						ContextWindow:  128000,
						MaxTokens:      32000,
						TimeoutSeconds: 180,
					},
				},
				Roles: Roles{Primary: "openai-M2.7"},
			},
		},
		{
			name:    "missing file",
			content: "",
			wantErr: true,
		},
		{
			name: "malformed json: trailing comma",
			content: `{
				"models": {},
				"roles": {"primary": ""},
			}`,
			wantErr: true,
		},
		{
			name: "empty models map",
			content: `{
				"models": {},
				"roles": {"primary": "openai-M2.7"}
			}`,
			want: &File{
				Models: map[string]ModelConfig{},
				Roles:  Roles{Primary: "openai-M2.7"},
			},
		},
		{
			name: "multiple models with distinct providers",
			content: `{
				"models": {
					"openai-M2.7": {"provider": "openai", "api_key": "k1", "base_url": "https://a.test", "model": "m1"},
					"anthropic-M3": {"provider": "anthropic", "api_key": "k2", "base_url": "https://b.test", "model": "m2", "max_tokens": 32000}
				},
				"roles": {"primary": "anthropic-M3"}
			}`,
			want: &File{
				Models: map[string]ModelConfig{
					"openai-M2.7":  {Provider: "openai", APIKey: "k1", BaseURL: "https://a.test", Model: "m1"},
					"anthropic-M3": {Provider: "anthropic", APIKey: "k2", BaseURL: "https://b.test", Model: "m2", MaxTokens: 32000},
				},
				Roles: Roles{Primary: "anthropic-M3"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if tt.content != "" {
				if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
			}

			got, err := Load(path)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Load() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Load() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name    string
		file    File
		want    ModelConfig
		wantErr bool
	}{
		{
			name: "primary found",
			file: File{
				Models: map[string]ModelConfig{
					"openai-M2.7": {Provider: "openai", APIKey: "k", BaseURL: "https://example.test", Model: "m"},
				},
				Roles: Roles{Primary: "openai-M2.7"},
			},
			want: ModelConfig{Provider: "openai", APIKey: "k", BaseURL: "https://example.test", Model: "m"},
		},
		{
			name: "primary empty",
			file: File{
				Models: map[string]ModelConfig{
					"openai-M2.7": {Provider: "openai"},
				},
				Roles: Roles{Primary: ""},
			},
			wantErr: true,
		},
		{
			name: "primary points at missing alias",
			file: File{
				Models: map[string]ModelConfig{
					"openai-M2.7": {Provider: "openai"},
				},
				Roles: Roles{Primary: "does-not-exist"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.file.Resolve()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Resolve() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("Resolve() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestModelConfig_Timeout(t *testing.T) {
	tests := []struct {
		name           string
		timeoutSeconds int64
		want           time.Duration
	}{
		{name: "unset falls back to default", timeoutSeconds: 0, want: defaultTimeout},
		{name: "negative falls back to default", timeoutSeconds: -5, want: defaultTimeout},
		{name: "explicit value preserved", timeoutSeconds: 180, want: 180 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := ModelConfig{TimeoutSeconds: tt.timeoutSeconds}
			if got := m.Timeout(); got != tt.want {
				t.Fatalf("Timeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCritique_ConcurrencyOrDefault(t *testing.T) {
	tests := []struct {
		name        string
		concurrency int
		want        int
	}{
		{name: "unset falls back to default", concurrency: 0, want: defaultCritiqueConcurrency},
		{name: "negative falls back to default", concurrency: -3, want: defaultCritiqueConcurrency},
		{name: "explicit value preserved", concurrency: 4, want: 4},
		{name: "one is a valid explicit value meaning fully serial", concurrency: 1, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Critique{Concurrency: tt.concurrency}
			if got := c.ConcurrencyOrDefault(); got != tt.want {
				t.Fatalf("ConcurrencyOrDefault() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCritique_MaxTurnsOrDefault(t *testing.T) {
	tests := []struct {
		name     string
		maxTurns int
		want     int
	}{
		{name: "unset falls back to default", maxTurns: 0, want: defaultCritiqueMaxTurns},
		{name: "negative falls back to default", maxTurns: -1, want: defaultCritiqueMaxTurns},
		{name: "explicit value preserved", maxTurns: 8, want: 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Critique{MaxTurns: tt.maxTurns}
			if got := c.MaxTurnsOrDefault(); got != tt.want {
				t.Fatalf("MaxTurnsOrDefault() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLoadCritiqueSection(t *testing.T) {
	tests := []struct {
		name            string
		body            string
		wantConcurrency int
		wantMaxTurns    int
	}{
		{
			name: "explicit critique settings are parsed",
			body: `{"models":{"m":{"provider":"openai"}},"roles":{"primary":"m"},
				"critique":{"concurrency":3,"max_turns":7}}`,
			wantConcurrency: 3,
			wantMaxTurns:    7,
		},
		{
			name:            "a config without a critique section falls back to defaults",
			body:            `{"models":{"m":{"provider":"openai"}},"roles":{"primary":"m"}}`,
			wantConcurrency: defaultCritiqueConcurrency,
			wantMaxTurns:    defaultCritiqueMaxTurns,
		},
		{
			name: "a partial critique section only overrides what it sets",
			body: `{"models":{"m":{"provider":"openai"}},"roles":{"primary":"m"},
				"critique":{"concurrency":2}}`,
			wantConcurrency: 2,
			wantMaxTurns:    defaultCritiqueMaxTurns,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			f, err := Load(path)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got := f.Critique.ConcurrencyOrDefault(); got != tt.wantConcurrency {
				t.Errorf("Concurrency = %d, want %d", got, tt.wantConcurrency)
			}
			if got := f.Critique.MaxTurnsOrDefault(); got != tt.wantMaxTurns {
				t.Errorf("MaxTurns = %d, want %d", got, tt.wantMaxTurns)
			}
		})
	}
}

func TestFile_LanguagePromptOrDefault(t *testing.T) {
	ptr := func(s string) *string { return &s }

	tests := []struct {
		name string
		file File
		want string
	}{
		{
			name: "unset falls back to the default Chinese prompt",
			file: File{},
			want: defaultLanguagePrompt,
		},
		{
			name: "explicit value preserved",
			file: File{LanguagePrompt: ptr("- Your response needs to be in English!!!")},
			want: "- Your response needs to be in English!!!",
		},
		{
			name: "explicit empty string means no language constraint",
			file: File{LanguagePrompt: ptr("")},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.file.LanguagePromptOrDefault(); got != tt.want {
				t.Fatalf("LanguagePromptOrDefault() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoad_AutoStage(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "absent defaults to false",
			content: `{"models": {}, "roles": {"primary": "m"}}`,
			want:    false,
		},
		{
			name:    "explicit true",
			content: `{"models": {}, "roles": {"primary": "m"}, "auto_stage": true}`,
			want:    true,
		},
		{
			name:    "explicit false",
			content: `{"models": {}, "roles": {"primary": "m"}, "auto_stage": false}`,
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			got, err := Load(path)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got.AutoStage != tt.want {
				t.Fatalf("AutoStage = %v, want %v", got.AutoStage, tt.want)
			}
		})
	}
}

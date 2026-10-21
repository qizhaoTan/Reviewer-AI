package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReasoningEffortConfig(t *testing.T) {
	for _, tt := range []struct {
		name, overlay, want string
		wantErr             bool
	}{
		{name: "inherit", overlay: "{}", want: "high"},
		{name: "override", overlay: `{"reasoning_effort":"low"}`, want: "low"},
		{name: "empty resets", overlay: `{"reasoning_effort":""}`},
		{name: "null resets", overlay: `{"reasoning_effort":null}`},
		{name: "wrong type", overlay: `{"reasoning_effort":42}`, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			base := filepath.Join(dir, "global.json")
			if err := os.WriteFile(base, []byte(`{"models":{"main":{"provider":"openai","reasoning_effort":"high"},"other":{"provider":"anthropic","reasoning_effort":"medium"}},"roles":{"primary":"main"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, ".reviewer"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".reviewer", "config.json"), []byte(`{"models":{"main":`+tt.overlay+`}}`), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadMerged(base, dir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("load error = %v", err)
			}
			if err != nil {
				return
			}
			model, err := cfg.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			if model.ReasoningEffort != tt.want || model.ToProviderConfig().ReasoningEffort != tt.want {
				t.Fatalf("effort not preserved through config: %+v", model)
			}
			if cfg.Models["other"].ToProviderConfig().ReasoningEffort != "medium" {
				t.Fatal("another model's effort was changed")
			}
		})
	}
}

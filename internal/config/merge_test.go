package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadMerged(t *testing.T) {
	tests := []struct {
		name, base, project string
		wantErr             bool
		check               func(*testing.T, *File)
	}{
		{name: "recursive overlay and explicit zero values", base: `{"models":{"main":{"provider":"openai","api_key":"test-key","model":"old","max_tokens":100},"other":{"model":"retained"}},"roles":{"primary":"main"},"critique":{"concurrency":7,"max_turns":20},"auto_stage":true,"language_prompt":"Chinese","ignore":["*.log"]}`, project: `{"models":{"main":{"model":"new","max_tokens":0}},"critique":{"concurrency":0},"auto_stage":false,"language_prompt":"","ignore":[]}`, check: func(t *testing.T, f *File) {
			m, err := f.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			if m.Model != "new" || m.Provider != "openai" || m.APIKey != "test-key" || m.MaxTokens != 0 || f.Models["other"].Model != "retained" {
				t.Fatalf("bad model merge: %+v", f.Models)
			}
			if f.AutoStage || f.LanguagePromptOrDefault() != "" || len(f.Ignore) != 0 || f.Critique.Concurrency != 0 || f.Critique.MaxTurns != 20 {
				t.Fatalf("bad merge: %+v", f)
			}
		}},
		{name: "missing project inherits global", base: `{"ignore":["*.log"]}`, check: func(t *testing.T, f *File) {
			if !reflect.DeepEqual(f.Ignore, []string{"*.log"}) {
				t.Fatal(f.Ignore)
			}
		}},
		{name: "project only", project: `{"ignore":["logs/**"]}`, check: func(t *testing.T, f *File) {
			if !reflect.DeepEqual(f.Ignore, []string{"logs/**"}) {
				t.Fatal(f.Ignore)
			}
		}},
		{name: "arrays replace", base: `{"ignore":["*.log"]}`, project: `{"ignore":["*.tmp"]}`, check: func(t *testing.T, f *File) {
			if !reflect.DeepEqual(f.Ignore, []string{"*.tmp"}) {
				t.Fatal(f.Ignore)
			}
		}},
		{name: "null resets", base: `{"ignore":["*.log"],"auto_stage":true,"language_prompt":"English"}`, project: `{"ignore":null,"auto_stage":null,"language_prompt":null}`, check: func(t *testing.T, f *File) {
			if len(f.Ignore) != 0 || f.AutoStage || f.LanguagePrompt != nil {
				t.Fatalf("%+v", f)
			}
		}},
		{name: "both missing", wantErr: true},
		{name: "invalid project JSON", base: `{}`, project: `{`, wantErr: true},
		{name: "invalid base not hidden by project", base: `{"auto_stage":"yes"}`, project: `{"auto_stage":false}`, wantErr: true},
		{name: "invalid array type", project: `{"ignore":"*.log"}`, wantErr: true},
		{name: "invalid pattern", project: `{"ignore":["../*.log"]}`, wantErr: true},
		{name: "null document", project: `null`, wantErr: true},
		{name: "array document", project: `[]`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			base := filepath.Join(dir, "global.json")
			if tt.base != "" {
				if err := os.WriteFile(base, []byte(tt.base), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.project != "" {
				if err := os.MkdirAll(filepath.Join(dir, ".reviewer"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, ".reviewer", "config.json"), []byte(tt.project), 0600); err != nil {
					t.Fatal(err)
				}
			}
			f, err := LoadMerged(base, dir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error=%v", err)
			}
			if err == nil && tt.check != nil {
				tt.check(t, f)
			}
		})
	}
}

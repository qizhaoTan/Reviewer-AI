package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguredIgnore(t *testing.T) {
	for _, backend := range []string{"stdlib", "ripgrep"} {
		t.Run(backend, func(t *testing.T) {
			if backend == "ripgrep" {
				if _, err := exec.LookPath("rg"); err != nil {
					t.Skip("rg unavailable")
				}
			} else {
				t.Setenv("PATH", t.TempDir())
			}
			root := t.TempDir()
			for _, p := range []string{"keep.go", "app.log", "nested/app.log", "logs/deep/data.txt", "nested/logs/keep.go", "trace1.txt"} {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, p), []byte("needle\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("app.log", filepath.Join(root, "alias.go")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("logs", filepath.Join(root, "aliasdir")); err != nil {
				t.Fatal(err)
			}
			rules := []string{"*.log", "logs/**", "trace?.txt"}
			tests := []struct {
				name    string
				tool    ITool
				args    string
				wantErr bool
			}{
				{"glob", GlobTool{Ignore: rules}, `{"pattern":"**/*","no_ignore":true}`, false},
				{"grep files", GrepTool{Ignore: rules}, `{"pattern":"needle","no_ignore":true}`, false},
				{"grep content", GrepTool{Ignore: rules}, `{"pattern":"needle","output_mode":"content","no_ignore":true}`, false},
				{"read excluded", ReadFileTool{Ignore: rules}, `{"path":"app.log"}`, true},
				{"read nested", ReadFileTool{Ignore: rules}, `{"path":"nested/app.log"}`, true},
				{"read alias", ReadFileTool{Ignore: rules}, `{"path":"alias.go"}`, true},
				{"read directory alias", ReadFileTool{Ignore: rules}, `{"path":"aliasdir/deep/data.txt"}`, true},
				{"read normalized", ReadFileTool{Ignore: rules}, `{"path":"nested/../app.log"}`, true},
				{"read allowed", ReadFileTool{Ignore: rules}, `{"path":"keep.go"}`, false},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					got := tt.tool.Execute(context.Background(), root, json.RawMessage(tt.args))
					if got.IsError != tt.wantErr {
						t.Fatalf("%+v", got)
					}
					if tt.wantErr {
						if !strings.Contains(got.Output, "ignore") {
							t.Fatal(got.Output)
						}
						return
					}
					for _, excluded := range []string{"app.log", "logs/deep/data.txt", "alias.go", "trace1.txt"} {
						if strings.Contains(got.Output, excluded) {
							t.Fatalf("leaked %s: %s", excluded, got.Output)
						}
					}
					if !strings.Contains(got.Output, "keep.go") {
						t.Fatal(got.Output)
					}
					if tt.name != "read allowed" && !strings.Contains(got.Output, "nested/logs/keep.go") {
						t.Fatalf("over-excluded rooted rule: %s", got.Output)
					}
				})
			}
		})
	}
}

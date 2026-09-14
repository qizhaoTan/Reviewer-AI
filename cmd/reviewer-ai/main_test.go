package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/qizhaoTan/Reviewer-AI/internal/tool"
)

func TestReadOnlyToolsIgnore(t *testing.T) {
	for _, name := range []string{"read_file", "glob", "grep"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "app.log"), []byte("secret"), 0600); err != nil {
				t.Fatal(err)
			}
			tools := readOnlyTools("*.log")
			selected, err := tool.FindToolByName(tools, name)
			if err != nil {
				t.Fatal(err)
			}
			args := map[string]string{"read_file": `{"path":"app.log"}`, "glob": `{"pattern":"*.log","no_ignore":true}`, "grep": `{"pattern":"secret","glob":"*.log","no_ignore":true}`}
			result := selected.Execute(context.Background(), root, json.RawMessage(args[name]))
			if name == "read_file" {
				if !result.IsError {
					t.Fatal("read ignored file")
				}
			} else {
				if result.IsError {
					t.Fatal(result.Output)
				}
				// An explicit positive glob must not override configured exclusions.
				if name == "glob" && result.Output != `glob：没有文件匹配模式 "*.log"。` {
					t.Fatal(result.Output)
				}
				if name == "grep" && result.Output != `grep：没有内容匹配模式 "secret"。` {
					t.Fatal(result.Output)
				}
			}
		})
	}
}

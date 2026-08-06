package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveWithinRoot(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(t *testing.T, root string) string // 返回要解析的 path 参数
		wantErr bool
	}{
		{
			name: "plain relative path",
			arrange: func(t *testing.T, root string) string {
				writeFile(t, filepath.Join(root, "a.go"), "package a\n")
				return "a.go"
			},
		},
		{
			name: "nested subdirectory path",
			arrange: func(t *testing.T, root string) string {
				dir := filepath.Join(root, "internal", "pkg")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				writeFile(t, filepath.Join(dir, "b.go"), "package pkg\n")
				return "internal/pkg/b.go"
			},
		},
		{
			name: "parent traversal",
			arrange: func(t *testing.T, root string) string {
				return "../escape.txt"
			},
			wantErr: true,
		},
		{
			name: "disguised traversal",
			arrange: func(t *testing.T, root string) string {
				return "foo/../../bar"
			},
			wantErr: true,
		},
		{
			name: "absolute path",
			arrange: func(t *testing.T, root string) string {
				return "/etc/hosts"
			},
			wantErr: true,
		},
		{
			name: "symlink escaping root",
			arrange: func(t *testing.T, root string) string {
				if runtime.GOOS == "windows" {
					t.Skip("symlink test skipped on windows")
				}
				outside := t.TempDir()
				target := filepath.Join(outside, "secret.txt")
				writeFile(t, target, "secret\n")
				link := filepath.Join(root, "link.txt")
				if err := os.Symlink(target, link); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return "link.txt"
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := tt.arrange(t, root)

			_, err := resolveWithinRoot(root, path)
			if tt.wantErr {
				if err == nil {
					t.Fatal("resolveWithinRoot() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveWithinRoot() error = %v", err)
			}
		})
	}
}

func TestReadFile(t *testing.T) {
	tests := []struct {
		name          string
		arrange       func(t *testing.T, root string) json.RawMessage
		wantIsError   bool
		outputContain []string
	}{
		{
			name: "full file read",
			arrange: func(t *testing.T, root string) json.RawMessage {
				writeFile(t, filepath.Join(root, "a.go"), "package a\n\nconst X = 1\n")
				return rawArgs(t, ReadFileInput{Path: "a.go"})
			},
			outputContain: []string{"const X = 1"},
		},
		{
			name: "ranged read with valid bounds",
			arrange: func(t *testing.T, root string) json.RawMessage {
				writeFile(t, filepath.Join(root, "a.go"), "line1\nline2\nline3\nline4\n")
				return rawArgs(t, ReadFileInput{Path: "a.go", StartLine: 2, EndLine: 3})
			},
			outputContain: []string{"line2", "line3"},
		},
		{
			name: "start_line beyond eof clamps",
			arrange: func(t *testing.T, root string) json.RawMessage {
				writeFile(t, filepath.Join(root, "a.go"), "line1\nline2\n")
				return rawArgs(t, ReadFileInput{Path: "a.go", StartLine: 1, EndLine: 100})
			},
			outputContain: []string{"line1", "line2"},
		},
		{
			name: "end_line before start_line is an error",
			arrange: func(t *testing.T, root string) json.RawMessage {
				writeFile(t, filepath.Join(root, "a.go"), "line1\nline2\n")
				return rawArgs(t, ReadFileInput{Path: "a.go", StartLine: 2, EndLine: 1})
			},
			wantIsError: true,
		},
		{
			name: "nonexistent file",
			arrange: func(t *testing.T, root string) json.RawMessage {
				return rawArgs(t, ReadFileInput{Path: "missing.go"})
			},
			wantIsError: true,
		},
		{
			name: "directory path",
			arrange: func(t *testing.T, root string) json.RawMessage {
				if err := os.MkdirAll(filepath.Join(root, "adir"), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				return rawArgs(t, ReadFileInput{Path: "adir"})
			},
			wantIsError: true,
		},
		{
			name: "malformed json arguments",
			arrange: func(t *testing.T, root string) json.RawMessage {
				return json.RawMessage(`{not valid json`)
			},
			wantIsError: true,
		},
		{
			name: "large file is truncated",
			arrange: func(t *testing.T, root string) json.RawMessage {
				var b strings.Builder
				for range maxReadLines + 50 {
					b.WriteString("x\n")
				}
				writeFile(t, filepath.Join(root, "big.go"), b.String())
				return rawArgs(t, ReadFileInput{Path: "big.go"})
			},
			outputContain: []string{"truncated"},
		},
		{
			name: "binary file is rejected",
			arrange: func(t *testing.T, root string) json.RawMessage {
				data := []byte{0x00, 0x01, 0x02, 'b', 'i', 'n'}
				if err := os.WriteFile(filepath.Join(root, "bin.dat"), data, 0o600); err != nil {
					t.Fatalf("write binary fixture: %v", err)
				}
				return rawArgs(t, ReadFileInput{Path: "bin.dat"})
			},
			wantIsError: true,
		},
		{
			name: "path escaping root is rejected",
			arrange: func(t *testing.T, root string) json.RawMessage {
				return rawArgs(t, ReadFileInput{Path: "../escape.txt"})
			},
			wantIsError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			args := tt.arrange(t, root)

			result := ReadFile(context.Background(), root, args)
			if result.IsError != tt.wantIsError {
				t.Fatalf("ReadFile() isError = %v, want %v (output: %s)", result.IsError, tt.wantIsError, result.Output)
			}
			for _, want := range tt.outputContain {
				if !strings.Contains(result.Output, want) {
					t.Errorf("output does not contain %q:\n%s", want, result.Output)
				}
			}
		})
	}
}

func rawArgs(t *testing.T, input ReadFileInput) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal ReadFileInput: %v", err)
	}
	return b
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

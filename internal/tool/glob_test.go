package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// setupGlobFixture 建一棵小文件树：
//
//	a.go
//	internal/foo.go
//	internal/bar/baz.go
//	internal/bar/qux_test.go
//	.git/ignored.go   (应被忽略)
func setupGlobFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFileWithDirs(t, filepath.Join(root, "a.go"), "package a\n")
	writeFileWithDirs(t, filepath.Join(root, "internal", "foo.go"), "package internal\n")
	writeFileWithDirs(t, filepath.Join(root, "internal", "bar", "baz.go"), "package bar\n")
	writeFileWithDirs(t, filepath.Join(root, "internal", "bar", "qux_test.go"), "package bar\n")
	writeFileWithDirs(t, filepath.Join(root, ".git", "ignored.go"), "package git\n")
	return root
}

// writeFileWithDirs 与 writeFile 类似，但会先创建目标文件所在的父目录链。
func writeFileWithDirs(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	writeFile(t, path, content)
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{name: "exact match", pattern: "a.go", path: "a.go", want: true},
		{name: "single star matches one segment", pattern: "*.go", path: "a.go", want: true},
		{name: "single star does not cross segments", pattern: "*.go", path: "internal/foo.go", want: false},
		{name: "double star matches nested path", pattern: "**/*.go", path: "internal/bar/baz.go", want: true},
		{name: "double star matches zero segments", pattern: "**/*.go", path: "a.go", want: true},
		{name: "prefixed double star restricts subtree", pattern: "internal/**/*.go", path: "internal/bar/baz.go", want: true},
		{name: "prefixed double star excludes outside subtree", pattern: "internal/**/*.go", path: "a.go", want: false},
		{name: "no match different extension", pattern: "*.go", path: "a.txt", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchGlob(tt.pattern, tt.path); got != tt.want {
				t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}

func TestGlobWithStdlib(t *testing.T) {
	tests := []struct {
		name        string
		pattern     string
		wantPaths   []string
		wantExclude []string
	}{
		{
			name:        "all go files recursively",
			pattern:     "**/*.go",
			wantPaths:   []string{"a.go", "internal/foo.go", "internal/bar/baz.go"},
			wantExclude: []string{".git/ignored.go"},
		},
		{
			name:      "restricted to subdirectory",
			pattern:   "internal/**/*.go",
			wantPaths: []string{"internal/foo.go", "internal/bar/baz.go"},
		},
		{
			name:      "top level only",
			pattern:   "*.go",
			wantPaths: []string{"a.go"},
		},
	}

	root := setupGlobFixture(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := globWithStdlib(root, tt.pattern)
			if err != nil {
				t.Fatalf("globWithStdlib() error = %v", err)
			}
			gotSet := make(map[string]bool, len(got))
			for _, p := range got {
				gotSet[p] = true
			}
			for _, want := range tt.wantPaths {
				if !gotSet[want] {
					t.Errorf("expected path %q in result, got %v", want, got)
				}
			}
			for _, unwanted := range tt.wantExclude {
				if gotSet[unwanted] {
					t.Errorf("path %q should have been excluded, got %v", unwanted, got)
				}
			}
		})
	}
}

func TestGlob(t *testing.T) {
	tests := []struct {
		name          string
		args          GlobInput
		wantIsError   bool
		outputContain []string
	}{
		{
			name:          "default pattern lists all files",
			args:          GlobInput{},
			outputContain: []string{"a.go", "internal/foo.go"},
		},
		{
			name:          "explicit recursive pattern",
			args:          GlobInput{Pattern: "internal/**/*.go"},
			outputContain: []string{"internal/foo.go", "internal/bar/baz.go"},
		},
		{
			name:          "no matches",
			args:          GlobInput{Pattern: "**/*.rb"},
			outputContain: []string{"no files matched"},
		},
	}

	root := setupGlobFixture(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.args)
			if err != nil {
				t.Fatalf("marshal GlobInput: %v", err)
			}
			result := Glob(context.Background(), root, b)
			if result.IsError != tt.wantIsError {
				t.Fatalf("Glob() isError = %v, want %v (output: %s)", result.IsError, tt.wantIsError, result.Output)
			}
			for _, want := range tt.outputContain {
				if !strings.Contains(result.Output, want) {
					t.Errorf("output does not contain %q:\n%s", want, result.Output)
				}
			}
		})
	}
}

func TestGlob_MalformedArguments(t *testing.T) {
	root := t.TempDir()
	result := Glob(context.Background(), root, json.RawMessage(`{not valid json`))
	if !result.IsError {
		t.Fatalf("Glob() isError = false, want true (output: %s)", result.Output)
	}
}

func TestGlob_ResultLimitTruncates(t *testing.T) {
	root := t.TempDir()
	for i := range maxGlobResults + 20 {
		writeFile(t, filepath.Join(root, "f"+strconv.Itoa(i)+".go"), "package f\n")
	}

	b, _ := json.Marshal(GlobInput{Pattern: "*.go"})
	result := Glob(context.Background(), root, b)
	if result.IsError {
		t.Fatalf("Glob() isError = true, want false (output: %s)", result.Output)
	}
	if !strings.Contains(result.Output, "result limit") {
		t.Errorf("expected truncation notice in output:\n%s", result.Output)
	}
}

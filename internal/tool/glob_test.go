package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
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

// setupGitignoreFixture 建一个带 .gitignore 的仓库，用于验证 no_ignore 参数：
//
//	kept.go            (未被忽略)
//	build/gen.go       (被 .gitignore 忽略)
//	.hidden/secret.go  (隐藏目录，任何情况下都不应出现)
//
// 只有 rg 会读 .gitignore，所以这里必须初始化成真正的 git 仓库。
func setupGitignoreFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFileWithDirs(t, filepath.Join(root, ".gitignore"), "build/\n")
	writeFileWithDirs(t, filepath.Join(root, "kept.go"), "package kept\n\nfunc Marker() {}\n")
	writeFileWithDirs(t, filepath.Join(root, "build", "gen.go"), "package build\n\nfunc Marker() {}\n")
	writeFileWithDirs(t, filepath.Join(root, ".hidden", "secret.go"), "package hidden\n\nfunc Marker() {}\n")
	cmd := exec.Command("git", "init")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git init unavailable, skipping .gitignore fixture: %v (%s)", err, out)
	}
	return root
}

func TestGlobWithRipgrepNoIgnore(t *testing.T) {
	tests := []struct {
		name        string
		noIgnore    bool
		wantPaths   []string
		wantExclude []string
	}{
		{
			name:        "default respects gitignore",
			noIgnore:    false,
			wantPaths:   []string{"kept.go"},
			wantExclude: []string{"build/gen.go", ".hidden/secret.go"},
		},
		{
			name:        "no_ignore includes gitignored files but not hidden ones",
			noIgnore:    true,
			wantPaths:   []string{"kept.go", "build/gen.go"},
			wantExclude: []string{".hidden/secret.go"},
		},
	}

	if !ripgrepAvailable() {
		t.Skip("rg not on PATH; no_ignore only takes effect on the ripgrep path")
	}
	root := setupGitignoreFixture(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := globWithRipgrep(root, "**/*.go", tt.noIgnore)
			if err != nil {
				t.Fatalf("globWithRipgrep() error = %v", err)
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

func TestGlobWithStdlibSkipsHidden(t *testing.T) {
	tests := []struct {
		name        string
		pattern     string
		wantPaths   []string
		wantExclude []string
	}{
		{
			name:        "hidden dirs and files are never listed",
			pattern:     "**/*.go",
			wantPaths:   []string{"kept.go", "build/gen.go"},
			wantExclude: []string{".hidden/secret.go"},
		},
	}

	root := setupGitignoreFixture(t)
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

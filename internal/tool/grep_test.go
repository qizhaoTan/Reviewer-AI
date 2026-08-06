package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupGrepFixture 建一棵小文件树，包含可搜索的符号和一个二进制文件。
func setupGrepFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFileWithDirs(t, filepath.Join(root, "a.go"), "package a\n\nfunc Foo() {}\n")
	writeFileWithDirs(t, filepath.Join(root, "internal", "bar.go"), "package internal\n\nfunc Foo() int { return 1 }\n")
	writeFileWithDirs(t, filepath.Join(root, "internal", "baz.txt"), "Foo is mentioned here too\n")
	writeFileWithDirs(t, filepath.Join(root, ".git", "ignored.go"), "func Foo() {}\n")
	if err := writeBinaryFile(filepath.Join(root, "bin.dat")); err != nil {
		t.Fatalf("write binary fixture: %v", err)
	}
	return root
}

func writeBinaryFile(path string) error {
	return os.WriteFile(path, []byte{0x00, 0x01, 'F', 'o', 'o'}, 0o600)
}

func TestGrepWithStdlib(t *testing.T) {
	tests := []struct {
		name          string
		pattern       string
		glob          string
		mode          string
		wantPaths     []string
		wantExclude   []string
		wantLineForIn string // path expected to appear with a specific line number, checked via contains on rendered output
	}{
		{
			name:      "files_with_matches finds all matching files",
			pattern:   "func Foo",
			mode:      outputModeFilesWithMatches,
			wantPaths: []string{"a.go", "internal/bar.go"},
		},
		{
			name:        "files_with_matches skips ignored and binary files",
			pattern:     "Foo",
			mode:        outputModeFilesWithMatches,
			wantExclude: []string{".git/ignored.go", "bin.dat"},
		},
		{
			name:      "glob filter restricts search",
			pattern:   "Foo",
			glob:      "*.go",
			mode:      outputModeFilesWithMatches,
			wantPaths: []string{"a.go", "internal/bar.go"},
			wantExclude: []string{
				"internal/baz.txt",
			},
		},
		{
			name:    "content mode returns line numbers",
			pattern: "func Foo",
			mode:    outputModeContent,
			wantPaths: []string{
				"a.go",
			},
		},
		{
			name:      "no matches returns empty result",
			pattern:   "NoSuchSymbolAnywhere",
			mode:      outputModeFilesWithMatches,
			wantPaths: nil,
		},
	}

	root := setupGrepFixture(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := grepWithStdlib(root, tt.pattern, tt.glob, tt.mode)
			if err != nil {
				t.Fatalf("grepWithStdlib() error = %v", err)
			}
			paths := make(map[string]bool, len(got))
			for _, m := range got {
				paths[m.Path] = true
			}
			for _, want := range tt.wantPaths {
				if !paths[want] {
					t.Errorf("expected path %q in matches, got %v", want, got)
				}
			}
			for _, unwanted := range tt.wantExclude {
				if paths[unwanted] {
					t.Errorf("path %q should have been excluded, got %v", unwanted, got)
				}
			}
		})
	}
}

func TestGrepWithStdlib_InvalidPattern(t *testing.T) {
	root := t.TempDir()
	_, err := grepWithStdlib(root, "(unclosed", "", outputModeFilesWithMatches)
	if err == nil {
		t.Fatal("grepWithStdlib() error = nil, want error for invalid regex")
	}
}

func TestGrep(t *testing.T) {
	tests := []struct {
		name          string
		args          GrepInput
		wantIsError   bool
		outputContain []string
	}{
		{
			name:          "default mode lists files",
			args:          GrepInput{Pattern: "func Foo"},
			outputContain: []string{"a.go"},
		},
		{
			name:          "content mode shows line numbers",
			args:          GrepInput{Pattern: "func Foo", OutputMode: outputModeContent},
			outputContain: []string{"a.go:"},
		},
		{
			name:          "glob filter",
			args:          GrepInput{Pattern: "Foo", Glob: "*.go"},
			outputContain: []string{"a.go"},
		},
		{
			name:          "empty pattern is an error",
			args:          GrepInput{Pattern: ""},
			wantIsError:   true,
			outputContain: []string{"pattern must not be empty"},
		},
		{
			name:          "invalid output mode is an error",
			args:          GrepInput{Pattern: "Foo", OutputMode: "bogus"},
			wantIsError:   true,
			outputContain: []string{"not supported"},
		},
		{
			name:          "no matches",
			args:          GrepInput{Pattern: "NoSuchSymbolAnywhere"},
			outputContain: []string{"no matches"},
		},
	}

	root := setupGrepFixture(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.args)
			if err != nil {
				t.Fatalf("marshal GrepInput: %v", err)
			}
			out, isError := Grep(root, b)
			if isError != tt.wantIsError {
				t.Fatalf("Grep() isError = %v, want %v (output: %s)", isError, tt.wantIsError, out)
			}
			for _, want := range tt.outputContain {
				if !strings.Contains(out, want) {
					t.Errorf("output does not contain %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestGrep_MalformedArguments(t *testing.T) {
	root := t.TempDir()
	out, isError := Grep(root, json.RawMessage(`{not valid json`))
	if !isError {
		t.Fatalf("Grep() isError = false, want true (output: %s)", out)
	}
}

func TestParseRipgrepContentLine(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantPath string
		wantLine int
		wantText string
		wantOK   bool
	}{
		{
			name:     "well formed line",
			line:     "internal/bar.go:3:func Foo() int { return 1 }",
			wantPath: "internal/bar.go",
			wantLine: 3,
			wantText: "func Foo() int { return 1 }",
			wantOK:   true,
		},
		{
			name:     "text containing colons",
			line:     "a.go:1:http://example.test:8080",
			wantPath: "a.go",
			wantLine: 1,
			wantText: "http://example.test:8080",
			wantOK:   true,
		},
		{
			name:   "missing line number",
			line:   "a.go:not-a-number:text",
			wantOK: false,
		},
		{
			name:   "no colons at all",
			line:   "malformed",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, lineNo, text, ok := parseRipgrepContentLine(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("parseRipgrepContentLine() ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if path != tt.wantPath || lineNo != tt.wantLine || text != tt.wantText {
				t.Errorf("parseRipgrepContentLine() = (%q, %d, %q), want (%q, %d, %q)",
					path, lineNo, text, tt.wantPath, tt.wantLine, tt.wantText)
			}
		})
	}
}

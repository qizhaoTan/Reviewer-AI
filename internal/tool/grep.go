package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

// outputMode 取值：见 GrepInput.OutputMode 注释。
const (
	outputModeFilesWithMatches = "files_with_matches"
	outputModeContent          = "content"
)

// GrepInput 是 grep ToolCall.Arguments 解码后的形状。
//
// 后续可迭代方向：Claude Code 的 Grep 还支持 count 输出模式、-A/-B/-C 上下文行数、
// ignore_case 等参数；第一版先给最小可用集（pattern + glob + output_mode 两种模式），
// 用起来后如果发现模型经常需要这些，再补齐。
type GrepInput struct {
	Pattern    string `json:"pattern"`               // 正则表达式（RE2 语法）
	Glob       string `json:"glob,omitempty"`        // 文件名过滤，如 "*.go"；为空表示不过滤
	OutputMode string `json:"output_mode,omitempty"` // "files_with_matches"（默认）或 "content"
}

// GrepDefinition 返回 grep 工具向模型公开的元信息。
func GrepDefinition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        "grep",
		Description: "Search file contents in the repository using a regular expression. Use this to find where a symbol, string, or pattern appears before deciding which file to read_file. output_mode \"files_with_matches\" (default) returns just the matching file paths; \"content\" returns matching lines with line numbers.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pattern": map[string]interface{}{
					"type":        "string",
					"description": "Regular expression to search for in file contents",
				},
				"glob": map[string]interface{}{
					"type":        "string",
					"description": "Optional filename filter, e.g. \"*.go\", to restrict which files are searched",
				},
				"output_mode": map[string]interface{}{
					"type":        "string",
					"enum":        []interface{}{outputModeFilesWithMatches, outputModeContent},
					"description": "\"files_with_matches\" (default) returns matching file paths only; \"content\" returns matching lines with line numbers",
				},
			},
			"required": []interface{}{"pattern"},
		},
	}
}

// Grep 执行一次 grep 工具调用，repoRoot 必须是已解析好的绝对路径。
// 有 ripgrep 时优先调用它；否则回退到标准库 regexp + 逐文件扫描的实现。
func Grep(repoRoot string, args json.RawMessage) (string, bool) {
	var input GrepInput
	if err := json.Unmarshal(args, &input); err != nil {
		return fmt.Sprintf("grep arguments are not valid JSON (%v). Call grep again with a JSON object like {\"pattern\": \"func Foo\"}.", err), true
	}
	if input.Pattern == "" {
		return "grep: pattern must not be empty. Provide a regular expression to search for, e.g. {\"pattern\": \"func Foo\"}.", true
	}
	mode := input.OutputMode
	if mode == "" {
		mode = outputModeFilesWithMatches
	}
	if mode != outputModeFilesWithMatches && mode != outputModeContent {
		return fmt.Sprintf("grep: output_mode %q is not supported. Use %q or %q.", mode, outputModeFilesWithMatches, outputModeContent), true
	}

	var (
		matches []grepMatch
		err     error
	)
	if ripgrepAvailable() {
		matches, err = grepWithRipgrep(repoRoot, input.Pattern, input.Glob, mode)
	} else {
		matches, err = grepWithStdlib(repoRoot, input.Pattern, input.Glob, mode)
	}
	if err != nil {
		return fmt.Sprintf("grep: %v. Check that pattern is a valid regular expression and glob (if provided) is a valid filename pattern, then retry.", err), true
	}

	if len(matches) == 0 {
		return fmt.Sprintf("grep: no matches for pattern %q.", input.Pattern), false
	}

	return renderGrepMatches(input.Pattern, matches, mode), false
}

// grepMatch 是一次匹配的统一表示：files_with_matches 模式下 Line/Text 为空。
type grepMatch struct {
	Path string
	Line int
	Text string
}

func renderGrepMatches(pattern string, matches []grepMatch, mode string) string {
	truncated := false
	if len(matches) > maxGrepResults {
		matches = matches[:maxGrepResults]
		truncated = true
	}

	var b strings.Builder
	if mode == outputModeFilesWithMatches {
		fmt.Fprintf(&b, "%d file(s) contain matches for %q:\n", len(matches), pattern)
		for _, m := range matches {
			b.WriteString(m.Path)
			b.WriteByte('\n')
		}
	} else {
		fmt.Fprintf(&b, "%d matching line(s) for %q:\n", len(matches), pattern)
		for _, m := range matches {
			fmt.Fprintf(&b, "%s:%d:%s\n", m.Path, m.Line, m.Text)
		}
	}
	if truncated {
		fmt.Fprintf(&b, "... [result limit of %d reached; narrow the pattern or add a glob filter to see more]\n", maxGrepResults)
	}
	return strings.TrimRight(b.String(), "\n")
}

// grepWithRipgrep 用 rg 执行搜索；files_with_matches 用 -l，content 用 -n。
func grepWithRipgrep(repoRoot, pattern, glob, mode string) ([]grepMatch, error) {
	ctx, cancel := context.WithTimeout(context.Background(), searchTimeoutSeconds*time.Second)
	defer cancel()

	args := []string{"--no-heading"}
	if mode == outputModeFilesWithMatches {
		args = append(args, "-l")
	} else {
		args = append(args, "-n")
	}
	if glob != "" {
		args = append(args, "--glob", glob)
	}
	args = append(args, "--", pattern)

	cmd := exec.CommandContext(ctx, "rg", args...)
	cmd.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		if exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("rg exited with an error: %s", strings.TrimSpace(stderr.String()))
	}
	if err != nil {
		return nil, fmt.Errorf("run rg: %w", err)
	}

	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	matches := make([]grepMatch, 0, len(lines))
	for _, l := range lines {
		if l == "" {
			continue
		}
		if mode == outputModeFilesWithMatches {
			matches = append(matches, grepMatch{Path: filepath.ToSlash(l)})
			continue
		}
		// content 模式下 rg -n 输出形如 "path:lineno:text"。
		path, lineNo, text, ok := parseRipgrepContentLine(l)
		if !ok {
			continue
		}
		matches = append(matches, grepMatch{Path: filepath.ToSlash(path), Line: lineNo, Text: text})
	}
	return matches, nil
}

func parseRipgrepContentLine(line string) (path string, lineNo int, text string, ok bool) {
	first := strings.IndexByte(line, ':')
	if first < 0 {
		return "", 0, "", false
	}
	second := strings.IndexByte(line[first+1:], ':')
	if second < 0 {
		return "", 0, "", false
	}
	second += first + 1

	path = line[:first]
	lineNoStr := line[first+1 : second]
	text = line[second+1:]
	n := 0
	for _, r := range lineNoStr {
		if r < '0' || r > '9' {
			return "", 0, "", false
		}
		n = n*10 + int(r-'0')
	}
	return path, n, text, true
}

// grepWithStdlib 是没有 rg 时的回退实现：编译正则后逐文件用 bufio.Scanner 扫描。
// 二进制文件与 ignoredDirNames 目录会被跳过；glob（若提供）用 matchGlob 对文件名过滤。
func grepWithStdlib(repoRoot, pattern, glob, mode string) ([]grepMatch, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regular expression: %w", err)
	}

	var matches []grepMatch
	walkErr := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != repoRoot && ignoredDirNames[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}

		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return relErr
		}
		relSlash := filepath.ToSlash(rel)
		if glob != "" && !matchGlob(glob, filepath.Base(relSlash)) {
			return nil
		}

		fileMatches, scanErr := scanFileForMatches(path, relSlash, re, mode)
		if scanErr != nil {
			return nil // 跳过读取失败/二进制文件，不中断整体搜索
		}
		matches = append(matches, fileMatches...)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk repository: %w", walkErr)
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Path != matches[j].Path {
			return matches[i].Path < matches[j].Path
		}
		return matches[i].Line < matches[j].Line
	})
	return matches, nil
}

func scanFileForMatches(absPath, relPath string, re *regexp.Regexp, mode string) ([]grepMatch, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	if isBinary(data) {
		return nil, fmt.Errorf("binary file")
	}

	var matches []grepMatch
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if !re.MatchString(scanner.Text()) {
			continue
		}
		if mode == outputModeFilesWithMatches {
			matches = append(matches, grepMatch{Path: relPath})
			break // 该模式只需要知道文件命中过一次
		}
		matches = append(matches, grepMatch{Path: relPath, Line: lineNo, Text: scanner.Text()})
	}
	return matches, nil
}

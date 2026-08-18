package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

// GlobInput 是 glob ToolCall.Arguments 解码后的形状。
type GlobInput struct {
	Pattern  string `json:"pattern"`             // 如 "internal/**/*.go"；为空时默认 "**/*"
	NoIgnore bool   `json:"no_ignore,omitempty"` // true 时不应用 .gitignore 过滤；仅在 ripgrep 路径生效
}

// GlobTool 是 glob 工具的 ITool 实现，方法体直接委托给同包的
// GlobDefinition/Glob 自由函数，不重复业务逻辑。
type GlobTool struct{}

func (GlobTool) Definition() schema.ToolDefinition { return GlobDefinition() }

func (GlobTool) Execute(ctx context.Context, repoRoot string, args json.RawMessage) Result {
	return Glob(ctx, repoRoot, args)
}

// GlobDefinition 返回 glob 工具向模型公开的元信息。
func GlobDefinition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        "glob",
		Description: "List repository files matching a glob pattern (supports ** for recursive matching, e.g. \"internal/**/*.go\"). Returns only file paths, not content — use this to discover which files exist before calling read_file. Omit pattern to list all files. Files ignored by .gitignore are excluded by default; set no_ignore to true to include them. Hidden files and directories (names starting with a dot) are not listed either way.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pattern": map[string]interface{}{
					"type":        "string",
					"description": "Glob pattern relative to the repo root, e.g. \"**/*.go\" or \"internal/tool/*.go\". Defaults to \"**/*\" (all files) if omitted.",
				},
				"no_ignore": map[string]interface{}{
					"type":        "boolean",
					"description": "Set to true to also list files excluded by .gitignore (e.g. build output or generated code you need to inspect). Defaults to false. Hidden files are not listed even when this is true.",
				},
			},
		},
	}
}

// Glob 执行一次 glob 工具调用，repoRoot 必须是已解析好的绝对路径。
// 有 ripgrep 时优先调用它（更快，原生尊重 .gitignore）；否则回退到标准库遍历实现。
func Glob(ctx context.Context, repoRoot string, args json.RawMessage) Result {
	var input GlobInput
	if err := json.Unmarshal(args, &input); err != nil {
		return Result{Output: fmt.Sprintf("glob 的参数不是合法的 JSON（%v）。请用类似 {\"pattern\": \"**/*.go\"} 的 JSON 对象重新调用 glob。", err), IsError: true}
	}
	pattern := input.Pattern
	if pattern == "" {
		pattern = "**/*"
	}

	var (
		paths []string
		err   error
	)
	if ripgrepAvailable() {
		paths, err = globWithRipgrep(repoRoot, pattern, input.NoIgnore)
	} else {
		paths, err = globWithStdlib(repoRoot, pattern)
	}
	if err != nil {
		return Result{Output: fmt.Sprintf("glob：%v。请检查 pattern 是否合法（例如 \"**/*.go\"、\"internal/*.go\"）后重试。", err), IsError: true}
	}

	if len(paths) == 0 {
		return Result{Output: fmt.Sprintf("glob：没有文件匹配模式 %q。", pattern)}
	}

	sort.Strings(paths)
	truncated := false
	if len(paths) > maxGlobResults {
		paths = paths[:maxGlobResults]
		truncated = true
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d file(s) matched %q:\n", len(paths), pattern)
	for _, p := range paths {
		b.WriteString(p)
		b.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&b, "... [result limit of %d reached; narrow the pattern to see more, e.g. add a subdirectory prefix]\n", maxGlobResults)
	}
	return Result{Output: strings.TrimRight(b.String(), "\n")}
}

// globWithRipgrep 用 `rg --files --glob <pattern>` 列出匹配的文件，返回相对 repoRoot 的路径。
//
// 后续可迭代方向：目前只用 --glob 单一模式过滤；Claude Code 的实现还会加 --sort=modified
// 按修改时间排序，这里为了让 rg/标准库两条路径的输出顺序一致（都按路径字符串排序），
// 暂不启用 --sort，如果之后发现"最近改动的文件更相关"这个信号有用，可以加回来。
func globWithRipgrep(repoRoot, pattern string, noIgnore bool) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), searchTimeoutSeconds*time.Second)
	defer cancel()

	args := []string{"--files", "--glob", pattern}
	if noIgnore {
		// 只加 --no-ignore，不加 --hidden：隐藏文件对审查场景价值低、噪声大，
		// 两条路径都保持"永不列出隐藏文件"这一条统一语义（标准库回退实现同样不列）。
		args = append(args, "--no-ignore")
	}

	cmd := exec.CommandContext(ctx, "rg", args...)
	cmd.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	// ripgrep 退出码：0=有匹配，1=无匹配（正常，视为空结果），2=用法错误。
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
	paths := make([]string, 0, len(lines))
	for _, l := range lines {
		if l != "" {
			paths = append(paths, filepath.ToSlash(l))
		}
	}
	return paths, nil
}

// globWithStdlib 是没有 rg 时的回退实现：遍历 repoRoot，用 matchGlob 做 ** 感知的模式匹配。
func globWithStdlib(repoRoot, pattern string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if rel != "." && (ignoredDirNames[d.Name()] || isHiddenName(d.Name())) {
				return fs.SkipDir
			}
			return nil
		}
		if isHiddenName(d.Name()) {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if matchGlob(pattern, relSlash) {
			paths = append(paths, relSlash)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk repository: %w", err)
	}
	return paths, nil
}

// matchGlob 实现一个支持 ** 递归通配的简化 glob 匹配，标准库 filepath.Match 不支持 **。
//
// 后续可迭代方向：这是一个功能子集实现（按 "/" 切分 pattern 和路径逐段匹配，"**" 匹配任意多段），
// 没有实现 {a,b} 花括号展开、字符类等 ripgrep/shell glob 的完整语法；如果没有 rg 时暴露的能力
// 差异造成困扰，可以引入 github.com/bmatcuk/doublestar 之类的小型库替换本函数。
func matchGlob(pattern, name string) bool {
	return matchGlobSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchGlobSegments(patternSegs, nameSegs []string) bool {
	if len(patternSegs) == 0 {
		return len(nameSegs) == 0
	}
	seg := patternSegs[0]
	if seg == "**" {
		// ** 匹配零段或多段：要么跳过 ** 本身（零段），要么消耗一段 name 后继续尝试同一个 **。
		if matchGlobSegments(patternSegs[1:], nameSegs) {
			return true
		}
		if len(nameSegs) == 0 {
			return false
		}
		return matchGlobSegments(patternSegs, nameSegs[1:])
	}
	if len(nameSegs) == 0 {
		return false
	}
	ok, err := filepath.Match(seg, nameSegs[0])
	if err != nil || !ok {
		return false
	}
	return matchGlobSegments(patternSegs[1:], nameSegs[1:])
}

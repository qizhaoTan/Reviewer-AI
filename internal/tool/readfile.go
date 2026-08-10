package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

const (
	// maxReadLines 是整文件读取（未指定行范围）时的行数上限，超出则截断。
	maxReadLines = 2000
	// maxReadBytes 是文件大小的读取上限，超出直接拒绝（避免把超大生成文件塞进模型上下文）。
	maxReadBytes = 200 * 1024
	// binarySniffLen 是用于判断文件是否为二进制的探测字节数。
	binarySniffLen = 8000
)

// ReadFileInput 是 read_file ToolCall.Arguments 解码后的形状。
type ReadFileInput struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"` // 1-based inclusive；0 表示从文件开头开始
	EndLine   int    `json:"end_line,omitempty"`   // 1-based inclusive；0 表示到文件结尾
}

// ReadFileTool 是 read_file 工具的 ITool 实现，方法体直接委托给同包的
// ReadFileDefinition/ReadFile 自由函数，不重复业务逻辑。
type ReadFileTool struct{}

func (ReadFileTool) Definition() schema.ToolDefinition { return ReadFileDefinition() }

func (ReadFileTool) Execute(ctx context.Context, repoRoot string, args json.RawMessage) Result {
	return ReadFile(ctx, repoRoot, args)
}

// ReadFileDefinition 返回 read_file 工具向模型公开的元信息。
func ReadFileDefinition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        "read_file",
		Description: "Read a file within the repository. Provide a repo-relative path. Optionally restrict to a 1-based inclusive line range via start_line/end_line; omit both to read the whole file (subject to a size cap).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Repo-relative file path to read",
				},
				"start_line": map[string]interface{}{
					"type":        "integer",
					"description": "1-based inclusive start line (optional, defaults to file start)",
				},
				"end_line": map[string]interface{}{
					"type":        "integer",
					"description": "1-based inclusive end line (optional, defaults to file end)",
				},
			},
			"required": []interface{}{"path"},
		},
	}
}

// ReadFile 执行一次 read_file 工具调用，repoRoot 必须是已解析好的绝对路径。
// 返回的 Result.IsError 为 true 时 Output 是可读的错误说明，
// 供模型据此自我纠正（如换一个路径重试），而不是让整个审查流程崩溃。
func ReadFile(ctx context.Context, repoRoot string, args json.RawMessage) Result {
	var input ReadFileInput
	if err := json.Unmarshal(args, &input); err != nil {
		return Result{Output: fmt.Sprintf("read_file arguments are not valid JSON (%v). Call read_file again with a JSON object like {\"path\": \"relative/path.go\"}.", err), IsError: true}
	}

	resolved, err := resolveWithinRoot(repoRoot, input.Path)
	if err != nil {
		return Result{Output: err.Error(), IsError: true}
	}

	info, err := os.Stat(resolved)
	if os.IsNotExist(err) {
		return Result{Output: fmt.Sprintf("read_file: %q does not exist in the repository. Double-check the path (it must be relative to the repo root, e.g. \"internal/foo/bar.go\") and retry.", input.Path), IsError: true}
	}
	if err != nil {
		return Result{Output: fmt.Sprintf("read_file: could not stat %q (%v). Try a different path.", input.Path, err), IsError: true}
	}
	if info.IsDir() {
		return Result{Output: fmt.Sprintf("read_file: %q is a directory, not a file. Provide the path to a specific file inside it instead.", input.Path), IsError: true}
	}
	if !info.Mode().IsRegular() {
		return Result{Output: fmt.Sprintf("read_file: %q is not a regular file (e.g. a device or socket) and cannot be read. Choose a different path.", input.Path), IsError: true}
	}
	if info.Size() > maxReadBytes {
		return Result{Output: fmt.Sprintf("read_file: %q is %d bytes, which exceeds the %d byte limit. Re-request it with start_line/end_line to read a smaller slice instead of the whole file.", input.Path, info.Size(), maxReadBytes), IsError: true}
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return Result{Output: fmt.Sprintf("read_file: could not read %q (%v). Try a different path.", input.Path, err), IsError: true}
	}
	if isBinary(data) {
		return Result{Output: fmt.Sprintf("read_file: %q appears to be a binary file and cannot be shown as text. Skip it or pick a different, text-based file.", input.Path), IsError: true}
	}

	return renderLines(input.Path, data, input.StartLine, input.EndLine)
}

// resolveWithinRoot 把 repo 相对路径解析为绝对路径，并确保结果不逃逸出 root。
func resolveWithinRoot(root, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("read_file: path must not be empty. Provide a repo-relative file path, e.g. \"internal/foo/bar.go\"")
	}
	if strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("read_file: path %q contains a NUL byte, which is not a valid file path. Provide a plain repo-relative path", path)
	}
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("read_file: path %q is absolute, but only repo-relative paths are allowed. Strip the leading %q and retry (e.g. use \"internal/foo/bar.go\" instead of the full path)", path, string(filepath.Separator))
	}

	rootAbs := filepath.Clean(root)
	candidate := filepath.Clean(filepath.Join(rootAbs, path))
	if !withinRoot(rootAbs, candidate) {
		return "", fmt.Errorf("read_file: path %q resolves outside the repository root (likely due to \"..\" segments). Only files inside the repository can be read; provide a path relative to the repo root that does not escape it", path)
	}

	// 二次校验：解析软链接后再次确认没有逃逸出仓库根目录。
	// 文件不存在时 EvalSymlinks 会报错，这种情况下交给后续 os.Stat 给出真实的“文件不存在”错误。
	if resolvedRoot, err := filepath.EvalSymlinks(rootAbs); err == nil {
		if resolvedCandidate, err := filepath.EvalSymlinks(candidate); err == nil {
			if !withinRoot(resolvedRoot, resolvedCandidate) {
				return "", fmt.Errorf("read_file: path %q is a symlink that points outside the repository root. Files outside the repository cannot be read; choose a different path", path)
			}
		}
	}

	return candidate, nil
}

// withinRoot 判断 candidate（已 Clean 的绝对路径）是否等于 root 或位于 root 之下。
func withinRoot(root, candidate string) bool {
	if candidate == root {
		return true
	}
	return strings.HasPrefix(candidate, root+string(filepath.Separator))
}

// isBinary 用是否包含 NUL 字节这一简单启发式判断文件是否为二进制。
func isBinary(data []byte) bool {
	sniff := data
	if len(sniff) > binarySniffLen {
		sniff = sniff[:binarySniffLen]
	}
	return bytes.ContainsRune(sniff, 0)
}

// renderLines 按可选的行范围渲染文件内容，越界的 start/end 会被 clamp。
func renderLines(path string, data []byte, startLine, endLine int) Result {
	lines := strings.Split(string(data), "\n")
	total := len(lines)

	if startLine == 0 && endLine == 0 {
		truncated := false
		if total > maxReadLines {
			lines = lines[:maxReadLines]
			truncated = true
		}
		out := fmt.Sprintf("%s (lines 1-%d of %d):\n%s", path, len(lines), total, strings.Join(lines, "\n"))
		if truncated {
			out += fmt.Sprintf("\n... [truncated, %d more line(s) omitted]", total-len(lines))
		}
		return Result{Output: out}
	}

	start := max(startLine, 1)
	end := endLine
	if end == 0 || end > total {
		end = total
	}
	if start > end {
		return Result{Output: fmt.Sprintf("read_file: requested range start_line=%d end_line=%d for %q is invalid because start_line is after end_line (file has %d lines). Swap or fix the two values, e.g. start_line=%d end_line=%d.", startLine, endLine, path, total, end, start), IsError: true}
	}

	selected := lines[start-1 : end]
	return Result{Output: fmt.Sprintf("%s (lines %d-%d of %d):\n%s", path, start, end, total, strings.Join(selected, "\n"))}
}

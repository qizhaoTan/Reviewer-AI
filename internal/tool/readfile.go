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
		return Result{Output: fmt.Sprintf("read_file 的参数不是合法的 JSON（%v）。请用类似 {\"path\": \"relative/path.go\"} 的 JSON 对象重新调用 read_file。", err), IsError: true}
	}

	resolved, err := resolveWithinRoot(repoRoot, input.Path)
	if err != nil {
		return Result{Output: err.Error(), IsError: true}
	}

	info, err := os.Stat(resolved)
	if os.IsNotExist(err) {
		return Result{Output: fmt.Sprintf("read_file：仓库中不存在 %q。请核对路径（必须是相对于仓库根目录的路径，例如 \"internal/foo/bar.go\"）后重试。", input.Path), IsError: true}
	}
	if err != nil {
		return Result{Output: fmt.Sprintf("read_file：无法获取 %q 的文件信息（%v）。请换一个路径试试。", input.Path, err), IsError: true}
	}
	if info.IsDir() {
		return Result{Output: fmt.Sprintf("read_file：%q 是目录而不是文件。请改为提供该目录下某个具体文件的路径。", input.Path), IsError: true}
	}
	if !info.Mode().IsRegular() {
		return Result{Output: fmt.Sprintf("read_file：%q 不是普通文件（例如设备或套接字），无法读取。请换一个路径。", input.Path), IsError: true}
	}
	if info.Size() > maxReadBytes {
		return Result{Output: fmt.Sprintf("read_file：%q 有 %d 字节，超过了 %d 字节的上限。请改用 start_line/end_line 读取其中一小段，而不是整个文件。", input.Path, info.Size(), maxReadBytes), IsError: true}
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return Result{Output: fmt.Sprintf("read_file：无法读取 %q（%v）。请换一个路径试试。", input.Path, err), IsError: true}
	}
	if isBinary(data) {
		return Result{Output: fmt.Sprintf("read_file：%q 看起来是二进制文件，无法以文本形式展示。请跳过它，或换一个文本文件。", input.Path), IsError: true}
	}

	return renderLines(input.Path, data, input.StartLine, input.EndLine)
}

// resolveWithinRoot 把 repo 相对路径解析为绝对路径，并确保结果不逃逸出 root。
func resolveWithinRoot(root, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("read_file：path 不能为空。请提供相对于仓库根目录的文件路径，例如 \"internal/foo/bar.go\"")
	}
	if strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("read_file：路径 %q 含有 NUL 字节，不是合法的文件路径。请提供一个普通的、相对于仓库根目录的路径", path)
	}
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("read_file：路径 %q 是绝对路径，但这里只接受相对于仓库根目录的路径。请去掉开头的 %q 后重试（例如用 \"internal/foo/bar.go\" 而不是完整路径）", path, string(filepath.Separator))
	}

	rootAbs := filepath.Clean(root)
	candidate := filepath.Clean(filepath.Join(rootAbs, path))
	if !withinRoot(rootAbs, candidate) {
		return "", fmt.Errorf("read_file：路径 %q 解析后落在了仓库根目录之外（通常是因为其中的 \"..\"）。只能读取仓库内部的文件；请提供一个不会跳出仓库根目录的相对路径", path)
	}

	// 二次校验：解析软链接后再次确认没有逃逸出仓库根目录。
	// 文件不存在时 EvalSymlinks 会报错，这种情况下交给后续 os.Stat 给出真实的“文件不存在”错误。
	if resolvedRoot, err := filepath.EvalSymlinks(rootAbs); err == nil {
		if resolvedCandidate, err := filepath.EvalSymlinks(candidate); err == nil {
			if !withinRoot(resolvedRoot, resolvedCandidate) {
				return "", fmt.Errorf("read_file：路径 %q 是一个指向仓库根目录之外的软链接。仓库外的文件无法读取；请换一个路径", path)
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
		return Result{Output: fmt.Sprintf("read_file：为 %[3]q 请求的范围 start_line=%[1]d end_line=%[2]d 不合法，因为 start_line 在 end_line 之后（该文件共 %[4]d 行）。请调换或修正这两个值，例如 start_line=%[5]d end_line=%[6]d。", startLine, endLine, path, total, end, start), IsError: true}
	}

	selected := lines[start-1 : end]
	return Result{Output: fmt.Sprintf("%s（第 %d-%d 行，共 %d 行）：\n%s", path, start, end, total, strings.Join(selected, "\n"))}
}

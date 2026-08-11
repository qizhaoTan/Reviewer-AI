package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/review"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

// SubmitReviewName 是 submit_review 的工具名。engine 需要按名字识别这次调用
// 意味着"审查结束"，所以导出成常量，避免调用方硬编码字符串。
const SubmitReviewName = "submit_review"

// SubmitReviewTool 让模型以结构化形式一次性提交完整的审查结论。
//
// 它和 read_file/glob/grep 的不同之处在于：它不读取任何东西，唯一的作用是约束
// 模型的输出格式——模型必须按 findings/summary 的形状作答，而不是吐一段自由
// Markdown 让程序去解析。产出的 Report 通过 Result.ReviewResult 返回，
// Output 只回一句给模型看的确认。
//
// 调用这个工具即表示审查结束，engine 会据此终止 tool loop。
//
// 这个工具是无状态的：结果走返回值而不是实例字段，所以一个实例可以被本次审查
// 里的所有 Agent（主 Agent、阶段五的并发子 Agent）共享，不会互相覆盖。
type SubmitReviewTool struct {
	// Changes 是本次审查的**全量**暂存区快照，用于两件事：校验模型给的 file
	// 确实属于本次改动，以及把 anchor 贴回 diff 算出行号。
	//
	// 这里刻意用全量而不是"该 Agent 负责的那部分文件"：阶段五里子 Agent 的
	// prompt 只会拿到分配给它的文件，但它在审查过程中完全可能发现问题的根源
	// 落在别人负责的文件上，用全量校验才不会把这类有效意见误拒。
	//
	// 本次审查期间它固定不变且只读，因此可以被所有 Agent 安全共享。
	Changes []gitdiff.Change
}

func (t SubmitReviewTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name: SubmitReviewName,
		Description: "Submit your final code review. Call this exactly once, when you have finished investigating and are ready to conclude. " +
			"Report every problem you found as a separate entry in `findings`; if you found no problems at all, still call this with an empty `findings` array and explain in `summary` why the changeset looks fine. " +
			"Do not answer with plain text instead of calling this tool — a review is only recorded when it is submitted here.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"summary": map[string]interface{}{
					"type":        "string",
					"description": "Overall assessment of the changeset in a short paragraph. Required even when there are no findings.",
				},
				"findings": map[string]interface{}{
					"type":        "array",
					"description": "One entry per problem found. Empty array means the changeset looks fine.",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"file": map[string]interface{}{
								"type":        "string",
								"description": "Repo-relative path of the file this finding is about. Must be one of the files in the staged changeset.",
							},
							"anchor": map[string]interface{}{
								"type": "string",
								"description": "The exact source lines this finding is about, copied verbatim from the diff (leading +/- markers are fine, indentation is ignored). " +
									"Do NOT report line numbers — they are computed from this snippet. Copy enough lines to be unambiguous, typically the statement or block at fault. " +
									"Omit this only for findings about the file as a whole.",
							},
							"severity": map[string]interface{}{
								"type":        "string",
								"enum":        severityEnum(),
								"description": "error = a definite defect (logic error, nil dereference, resource leak, security issue). warning = a real risk that may not always bite (edge cases, performance, maintainability). info = optional suggestion (naming, style, readability).",
							},
							"summary": map[string]interface{}{
								"type":        "string",
								"description": "The problem stated in one sentence.",
							},
							"detail": map[string]interface{}{
								"type":        "string",
								"description": "Optional longer explanation and a concrete suggested fix.",
							},
						},
						"required": []interface{}{"file", "severity", "summary"},
					},
				},
			},
			"required": []interface{}{"summary", "findings"},
		},
	}
}

// Execute 解析并校验模型提交的审查结果，成功时通过 Result.ReviewResult 返回。
//
// 校验失败一律返回 IsError=true 且 Output 写明"错在哪 + 该怎么改"，模型看到后
// 会重新调用一次。这里的严格校验是有意的：这是结构化数据进入系统的唯一入口，
// 放进来的脏数据后面每一层（复核、持久化、Web 展示、reply 交互）都要再处理一遍。
func (t SubmitReviewTool) Execute(_ context.Context, _ string, args json.RawMessage) Result {
	var input review.Report
	if err := json.Unmarshal(args, &input); err != nil {
		return Result{
			Output: fmt.Sprintf("%s arguments are not valid JSON (%v). Call it again with an object like "+
				`{"summary": "...", "findings": [{"file": "path/to/file.go", "anchor": "the exact lines at fault", "severity": "warning", "summary": "..."}]}.`,
				SubmitReviewName, err),
			IsError: true,
		}
	}

	normalized, err := review.NormalizeReport(input)
	if err != nil {
		return Result{Output: fmt.Sprintf("%s: %v", SubmitReviewName, err), IsError: true}
	}

	if err := t.validateFiles(normalized); err != nil {
		return Result{Output: fmt.Sprintf("%s: %v", SubmitReviewName, err), IsError: true}
	}

	// 行号在这里算出来，而不是留给调用方：Changes 就在手边，晚一步算就多一处
	// 可能忘记调用 ResolveAnchors 的地方，Finding 也就可能带着空行号流向下游。
	normalized.Findings = review.ResolveAnchors(normalized.Findings, t.Changes)

	return Result{
		Output:       fmt.Sprintf("Review submitted: %d finding(s) recorded.", len(normalized.Findings)),
		ReviewResult: &normalized,
	}
}

// validateFiles 检查每条 Finding 指向的文件确实在本次暂存区改动里。
//
// 拦住的是模型"评论了一个它只是顺路读过、但本次并没有改动的文件"这种情况——
// 那类意见超出了本次审查的范围（用户是来看这次改动的），而且文件不在快照里
// 也就没有 patch 可供 anchor 定位。
func (t SubmitReviewTool) validateFiles(r review.Report) error {
	if len(t.Changes) == 0 {
		return nil // 没有快照可比对时不做限制，避免在测试/复核等场景下误伤。
	}
	changed := make(map[string]struct{}, len(t.Changes))
	for _, c := range t.Changes {
		changed[c.Path] = struct{}{}
	}
	for i, f := range r.Findings {
		if _, ok := changed[f.File]; !ok {
			return fmt.Errorf("findings[%d].file is %q, which is not part of this staged changeset. "+
				"Only report problems in the changed files (%s). "+
				"If the real problem is in an unchanged file, report it against the changed file that depends on it and explain the connection in detail",
				i, f.File, strings.Join(changedPaths(t.Changes), ", "))
		}
	}
	return nil
}

// changedPaths 抽出快照里的全部路径，用于错误信息里提示模型有哪些文件可选。
func changedPaths(changes []gitdiff.Change) []string {
	paths := make([]string, 0, len(changes))
	for _, c := range changes {
		paths = append(paths, strconv.Quote(c.Path))
	}
	return paths
}

// severityEnum 把合法的 severity 取值渲染成 JSON Schema 的 enum 数组。
// 从 review.AllSeverities 派生而不是硬编码，保证档位增减时两边不会不一致。
func severityEnum() []interface{} {
	all := review.AllSeverities()
	out := make([]interface{}, 0, len(all))
	for _, s := range all {
		out = append(out, string(s))
	}
	return out
}

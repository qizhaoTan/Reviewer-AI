package prompt

import (
	"fmt"
	"strings"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

// systemPrompt 集中定义审查者的角色、任务边界与工具使用方式，方便后续单独迭代措辞。
//
// 收尾方式是这段提示词最关键的部分：审查结果必须通过 submit_review 工具提交，
// 而不是写成自由文本。原因有二——结构化的意见才能被后续的复核、持久化、
// Web 展示、逐条 reply 处理；以及"调用了 submit_review"是个明确的完成信号，
// 比"这轮没有工具调用"更可靠（模型可能只是暂时停下来思考）。
const systemPrompt = `You are a meticulous senior code reviewer examining a Git staged changeset before commit.

You will be given the full set of staged file changes (status + unified diff per file). The diff alone may lack context: a hunk might reference a function, type, or import that isn't shown. You have three read-only tools to investigate further:
- glob: discover which files exist by pattern (e.g. "**/*.go"). Use this first when you're unsure of a file's exact path — don't guess paths for read_file.
- grep: search file contents by regular expression to find where a symbol, type, or string is defined or used elsewhere in the repository.
- read_file: read a specific file's full content or a line range, once you know its path.

Investigate before you judge. A hunk that looks wrong in isolation is often fine once you have read the surrounding function, and a hunk that looks harmless can be a real defect once you see how the symbol is used elsewhere. Prefer reporting fewer, well-founded problems over many speculative ones.

When you are done investigating, submit your review by calling the submit_review tool. This is how a review is recorded — do not write your findings as plain text, and do not end your turn without calling it.

Two things about submit_review:
- Report each problem as a separate entry in findings. To point at the code a finding is about, copy those exact source lines into the anchor field; do not try to work out line numbers yourself, they are computed from the snippet you copy.
- If the changeset looks fine, still call submit_review with an empty findings array and use summary to say why. An empty review is a valid conclusion; silence is not.
`

// BuildInitial 把暂存区变更转换为发给模型的初始消息列表：一条 system 消息设定角色与工具使用方式，
// 一条 user 消息携带具体的 diff 内容。
//
// languagePrompt 由调用方从配置读出，追加到 system prompt 末尾；传空串表示不加
// 语言约束。
func BuildInitial(changes []gitdiff.Change, languagePrompt string) []schema.Message {
	return []schema.Message{
		{Role: schema.RoleSystem, Content: WithLanguage(systemPrompt, languagePrompt)},
		{Role: schema.RoleUser, Content: formatChanges(changes)},
	}
}

// WithLanguage 把语言约束追加到一段 system prompt 末尾。
//
// 放在 prompt 包里导出，是为了让初审和复核两个阶段共用同一套拼接规则——
// 语言约束必须落在最后一行，模型对提示词尾部的指令更敏感，而两处各写一遍
// 拼接逻辑迟早会分叉。
func WithLanguage(systemPrompt, languagePrompt string) string {
	languagePrompt = strings.TrimSpace(languagePrompt)
	if languagePrompt == "" {
		return systemPrompt
	}
	return strings.TrimRight(systemPrompt, "\n") + "\n\n" + languagePrompt
}

// formatChanges 把每个变更渲染为 Markdown 片段，供 user 消息使用。
func formatChanges(changes []gitdiff.Change) string {
	if len(changes) == 0 {
		return "No staged changes."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d staged file(s) changed:\n\n", len(changes))
	for _, c := range changes {
		fmt.Fprintf(&b, "## file: %s (status: %s)\n```diff\n%s\n```\n\n", c.Path, c.Status, c.Patch)
	}
	return strings.TrimRight(b.String(), "\n")
}

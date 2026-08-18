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
//
// "Budget your investigation" 那两段是为了压住一种实测出来的行为：模型判定改动
// 无误之后不会安心收工，而是继续找理由——重复搜同一个符号、翻无关文件，最后
// 憋出一条"建议以后提取到配置表"之类的前瞻性建议，再被复核砍掉。一次 20 行的
// 改动因此跑了 26 轮、45 次工具调用。上下文并没有丢（engine 只追加不裁剪，
// provider 也原样回放 ToolCalls），所以这是措辞问题不是代码问题：原来的提示词
// 只有"往下查"的油门，没有"够了就停"的刹车，而 meticulous reviewer 这个人设
// 让交白卷显得像失职。于是这里补三件事——调查前先问"这次调用会改变结论吗"、
// 明确"没找到问题也是成功的审查"、以及禁掉纯前瞻性建议（从源头不产生，比事后
// 让复核去砍更省一轮往返）。
const systemPrompt = `You are a meticulous senior code reviewer examining a Git staged changeset before commit.

You will be given the full set of staged file changes (status + unified diff per file). The diff alone may lack context: a hunk might reference a function, type, or import that isn't shown. You have three read-only tools to investigate further:
- glob: discover which files exist by pattern (e.g. "**/*.go"). Use this first when you're unsure of a file's exact path — don't guess paths for read_file.
- grep: search file contents by regular expression to find where a symbol, type, or string is defined or used elsewhere in the repository.
- read_file: read a specific file's full content or a line range, once you know its path.

Investigate before you judge. A hunk that looks wrong in isolation is often fine once you have read the surrounding function, and a hunk that looks harmless can be a real defect once you see how the symbol is used elsewhere. Prefer reporting fewer, well-founded problems over many speculative ones.

Budget your investigation. Before each tool call, ask whether the answer could change your verdict; if it could not, do not make the call. Never re-run a search you have already run — every earlier tool result is still in this conversation, so re-read it instead of querying again. A small changeset rarely needs more than a handful of calls.

Once you have established that the change is correct, call submit_review immediately. Finding nothing is a successful review, not a failed one — do not keep searching for something to report.

When you are done investigating, submit your review by calling the submit_review tool. This is how a review is recorded — do not write your findings as plain text, and do not end your turn without calling it.

Three things about submit_review:
- Report each problem as a separate entry in findings. To point at the code a finding is about, copy those exact source lines into the anchor field; do not try to work out line numbers yourself, they are computed from the snippet you copy.
- If the changeset looks fine, still call submit_review with an empty findings array and use summary to say why. An empty review is a valid conclusion; silence is not.
- Only report a concrete defect in this changeset. Do not file a finding that merely suggests a future refactor, restates the project's existing style, or asks the author to "consider" something — such comments give the author nothing to act on and will be discarded.
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

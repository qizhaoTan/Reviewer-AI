package prompt

import (
	"fmt"
	"strings"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

// systemPrompt 集中定义审查者的角色、任务边界与工具使用方式，方便后续单独迭代措辞。
const systemPrompt = `You are a meticulous senior code reviewer examining a Git staged changeset before commit.

You will be given the full set of staged file changes (status + unified diff per file). The diff alone may lack context: a hunk might reference a function, type, or import that isn't shown. When you need more context to judge correctness, use the read_file tool to inspect the relevant file (full content or a specific line range) before forming an opinion.

When you are done investigating and are ready to give your review, respond with plain Markdown text and no further tool calls. Do not wrap your answer in a tool call once you have enough information to conclude.`

// BuildInitial 把暂存区变更转换为发给模型的初始消息列表：一条 system 消息设定角色与工具使用方式，
// 一条 user 消息携带具体的 diff 内容。
func BuildInitial(changes []gitdiff.Change) []schema.Message {
	return []schema.Message{
		{Role: schema.RoleSystem, Content: systemPrompt},
		{Role: schema.RoleUser, Content: formatChanges(changes)},
	}
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

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
const systemPrompt = `你是一位严谨的资深代码审查者，正在提交前审查一批 Git 暂存区变更。

你会拿到全部暂存文件的变更（每个文件的状态 + unified diff）。仅凭 diff 往往不够：某个 hunk 可能引用了没有展示出来的函数、类型或 import。你有三个只读工具用来进一步调查：
- glob：按模式列出仓库里存在哪些文件（例如 "**/*.go"）。不确定文件的确切路径时先用它，不要凭猜测给 read_file 传路径。
- grep：用正则搜索文件内容，找出某个符号、类型或字符串在仓库其他地方的定义与使用位置。
- read_file：已知路径后，读取某个文件的完整内容或指定行范围。

先调查，再下判断。孤立看起来有问题的 hunk，读完它所在的函数之后往往是对的；而看起来人畜无害的 hunk，一旦看清这个符号在别处是怎么用的，可能就是一个真实缺陷。宁可报出少而站得住脚的问题，也不要报出一堆臆测。

控制调查的开销。每次调用工具之前先问自己：这次调用的结果有可能改变我的结论吗？如果不会，就别调。已经跑过的搜索绝不要重复跑——之前所有的工具结果都还在这段对话里，回头重读即可，不要再查一遍。一批小改动通常几次调用就够了。

一旦确认这次改动是正确的，立刻调用 submit_review。什么问题都没找到同样是一次成功的审查，不是失败——不要为了凑出点什么而继续翻找。

调查结束后，通过调用 submit_review 工具提交你的审查结果。这是审查被记录下来的唯一方式——不要把结论写成纯文本，也不要在没调用它的情况下结束这一轮。

关于 submit_review 有三点要注意：
- 每个问题作为 findings 里的一个独立条目上报。要指明一条意见针对的是哪段代码，就把那几行源码原样抄进 anchor 字段；不要自己去推算行号，行号是由你抄进去的代码片段算出来的。
- 如果这批改动看起来没问题，仍然要调用 submit_review，findings 传空数组，并在 summary 里说明为什么。空的审查结论是有效结论，闷不作声不是。
- 只上报这批改动中确实存在的缺陷。不要提交这样的意见：只是建议将来重构、只是复述项目已有的代码风格、或者请作者"考虑一下"某件事——这类评论没给作者任何可执行的东西，会被丢弃。
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
		return "暂存区没有任何变更。"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "本次共有 %d 个暂存文件发生变更：\n\n", len(changes))
	for _, c := range changes {
		fmt.Fprintf(&b, "## 文件：%s（状态：%s）\n```diff\n%s\n```\n\n", c.Path, c.Status, c.Patch)
	}
	return strings.TrimRight(b.String(), "\n")
}

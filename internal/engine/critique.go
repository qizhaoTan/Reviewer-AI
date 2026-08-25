package engine

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/log"
	"github.com/qizhaoTan/Reviewer-AI/internal/prompt"
	"github.com/qizhaoTan/Reviewer-AI/internal/review"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
	"github.com/qizhaoTan/Reviewer-AI/internal/tool"
)

// critiqueReport 在初审结果之上跑复核阶段。CritiqueTools 为空时跳过复核，
// 原样返回初审结果（Critiqued 保持 false，下游据此知道这些 Kept 值不作数）。
//
// 注意这里用的是 ctx 而不是主循环的 genCtx：genCtx 的超时预算是给初审的，
// 复核是一批全新的模型调用，不该去分初审剩下的时间。
func critiqueReport(ctx context.Context, deps Deps, report review.Report, repoRoot string, changes []gitdiff.Change) (review.Report, error) {
	if len(deps.CritiqueTools) == 0 {
		log.Info("未配置复核工具，跳过复核阶段")
		return report, nil
	}
	log.Info("开始并发复核", "findings", len(report.Findings), "concurrency", deps.CritiqueConcurrency)
	return Critique(ctx, CritiqueDeps{
		LLM:         deps.LLM,
		Tools:       deps.CritiqueTools,
		RepoRoot:    repoRoot,
		Concurrency: deps.CritiqueConcurrency,
		MaxTurns:    deps.CritiqueMaxTurns,

		LanguagePrompt: deps.LanguagePrompt,
	}, report, changes)
}

// CritiqueDeps 是复核所需的外部依赖，由调用方组装后传入。
type CritiqueDeps struct {
	// LLM 复用初审的 provider。第一版不引入独立的 critic 模型配置——先跑通，
	// 效果不理想再考虑换不同模型做交叉验证。
	LLM schema.IProvider

	// Tools 是复核者可用的只读工具（read_file/glob/grep）加上 submit_verdict。
	// 让复核者能自己查证代码，是复核比"再问一遍模型"更可信的关键：初审说
	// "这个函数没处理 nil"，复核者可以真的去把那个函数读出来看。
	Tools []tool.ITool

	// RepoRoot 是仓库绝对路径，传给工具执行。
	RepoRoot string

	// Concurrency 是同时复核的意见条数，<=0 时不限并发。
	Concurrency int

	// MaxTurns 是单条意见复核的最大轮数，<=0 时取 fallbackCritiqueMaxTurns。
	MaxTurns int

	// LanguagePrompt 是追加到复核 system prompt 末尾的语言约束，留空表示不加。
	// 见 Deps.LanguagePrompt。
	LanguagePrompt string
}

// fallbackCritiqueMaxTurns 只在调用方没有填 CritiqueDeps.MaxTurns 时兜底。
// 正常路径下这个值来自配置（config.Critique.MaxTurnsOrDefault），这里的常量
// 是为了让 engine 在被直接调用（如测试）时也不会退化成"不限轮数"。
const fallbackCritiqueMaxTurns = 30

// Critique 对报告里的每一条意见独立做一次复核，并发执行。
//
// 每条意见起一个自己的对话：复核者看到的是这一条意见 + 它所在文件的 diff，
// 外加只读工具，据此判断这条意见站不站得住脚。各条之间互不共享消息历史——
// 一条意见的裁决不该被另一条的讨论带偏。
//
// 返回的 Report 里 Findings 数量与顺序都和入参一致，只是每条都带上了 Kept 和
// CritiqueReason，且 Critiqued 置为 true。被丢弃的意见不会被删除——它们仍然
// 落盘保留，只是不进入最终展示（见 Report.KeptFindings）。
//
// 复核只做减法：没有任何路径可以新增意见或改写意见内容。复核阶段引入初审
// 没看到的新臆测，比漏掉一条意见更糟。
//
// 单条复核失败分两种，处理方式完全不同：
//
//   - 模型行为问题（超轮数：复核者一直不肯给裁决）按"保留"处理，并在
//     CritiqueReason 里写明原因。重跑也大概率还是这个结果，不值得为它作废
//     整个复核阶段；而因为复核者话多就默默吞掉一条可能有效的审查意见，
//     是最糟的失败方式。
//
//   - 基础设施问题（Generate 报错、上下文取消/超时）则中断整体并上抛，由
//     engine.Run 让运行留在 in_progress、下次重跑时只重跑复核。这类失败跟
//     意见本身无关，把"网络抖了一下"固化成"复核未能完成"写进报告并标记
//     completed，会让这条意见永远失去被真正复核的机会。
func Critique(ctx context.Context, deps CritiqueDeps, report review.Report, changes []gitdiff.Change) (review.Report, error) {
	out := report
	out.Critiqued = true
	out.Findings = append([]review.Finding(nil), report.Findings...)
	if len(out.Findings) == 0 {
		return out, nil
	}

	patchByPath := make(map[string]string, len(changes))
	for _, c := range changes {
		patchByPath[c.Path] = c.Patch
	}

	g, gctx := errgroup.WithContext(ctx)
	if deps.Concurrency > 0 {
		g.SetLimit(deps.Concurrency)
	}

	for i := range out.Findings {
		g.Go(func() error {
			f := out.Findings[i]
			verdict, err := critiqueOne(gctx, deps, f, patchByPath[f.File])
			if err != nil {
				// 基础设施问题：上抛以中断整体，让这次复核有机会重跑。
				// errgroup 会连带取消其余仍在跑的复核。
				if isRecoverable(err) {
					log.Error("复核遇到可恢复的失败，中断本次复核", "findingID", f.ID, "error", err)
					return fmt.Errorf("finding %s: %w", f.ID, err)
				}
				// 模型行为问题：保留而不是丢弃，见函数注释里对失败的处理原则。
				log.Error("复核失败，保留该条意见", "findingID", f.ID, "error", err)
				out.Findings[i].Kept = true
				out.Findings[i].CritiqueReason = fmt.Sprintf("复核未能完成（%v），保留该条意见", err)
				return nil
			}
			out.Findings[i].Kept = verdict.Keep
			out.Findings[i].CritiqueReason = verdict.Reason
			log.Info("复核完成", "findingID", f.ID, "kept", verdict.Keep, "reason", verdict.Reason)
			return nil
		})
	}
	// 只有可恢复的失败会走到这里（其余都被 goroutine 自己消化成"保留该条意见"）。
	// 包成 recoverableError 上抛，让 engine.Run 据此保持 in_progress。
	if err := g.Wait(); err != nil {
		return review.Report{}, recoverable(fmt.Errorf("critique findings: %w", err))
	}
	return out, nil
}

// critiqueOne 对单条意见跑一个独立的 tool loop，直到复核者调用 submit_verdict。
func critiqueOne(ctx context.Context, deps CritiqueDeps, f review.Finding, patch string) (tool.Verdict, error) {
	maxTurns := deps.MaxTurns
	if maxTurns <= 0 {
		maxTurns = fallbackCritiqueMaxTurns
	}

	msgs := buildCritiqueMessages(f, patch, deps.LanguagePrompt)
	toolDefinitions := make([]schema.ToolDefinition, len(deps.Tools))
	for i, t := range deps.Tools {
		toolDefinitions[i] = t.Definition()
	}

	for range maxTurns {
		resp, err := deps.LLM.Generate(ctx, msgs, toolDefinitions)
		if err != nil {
			// 标记为可恢复：调用方据此中断整体而不是把它写成"复核未能完成"。
			return tool.Verdict{}, recoverable(fmt.Errorf("generate verdict: %w", err))
		}
		msgs = append(msgs, *resp)

		if len(resp.ToolCalls) == 0 {
			msgs = append(msgs, schema.Message{
				Role: schema.RoleUser,
				Content: fmt.Sprintf("你的裁决还没有被记录：裁决只能通过 %s 工具提交。现在就调用 %s。",
					tool.CritiqueVerdictName, tool.CritiqueVerdictName),
			})
			continue
		}

		for _, tc := range resp.ToolCalls {
			var result tool.Result
			if t, err := tool.FindToolByName(deps.Tools, tc.Name); err != nil {
				result = tool.Result{
					Output:  fmt.Sprintf("未知工具 %q，不存在这个工具。只能使用本次会话中提供给你的那些工具。", tc.Name),
					IsError: true,
				}
			} else {
				result = t.Execute(ctx, deps.RepoRoot, tc.Arguments)
			}
			if result.CritiqueVerdict != nil {
				return *result.CritiqueVerdict, nil
			}
			msgs = append(msgs, schema.Message{
				Role:       schema.RoleUser,
				Content:    result.Output,
				ToolCallID: tc.ID,
			})
		}
	}

	return tool.Verdict{}, fmt.Errorf("超过复核轮数上限（%d）仍未调用 %s", maxTurns, tool.CritiqueVerdictName)
}

// critiqueSystemPrompt 定义复核者的角色。措辞的重点是压住"复核者倾向于附和
// 初审"的倾向——同一个模型自我批判时，天然不容易否定自己，所以这里明确告诉它
// 删掉没根据的意见是它的本职工作，而不是对同事的冒犯。
const critiqueSystemPrompt = `你是一位严格的"审查意见的审查者"。另一位审查者针对一批暂存区代码改动提出了一条意见，你的工作是判断这条意见值不值得递到作者面前。

只有当这条意见指出了作者确实应该处理的真实问题时，才保留它。如果它属于臆测、建立在对代码的误读之上、其他地方已经处理过了，或者琐碎到不值得占用作者的注意力，就丢弃它。

你有只读工具（glob、grep、read_file），并且被期望真的去用它们。不要把这条意见的说法照单全收：如果它说某个函数没处理好某种输入，就去把那个函数读出来。一条听上去有道理、但一接触真实代码就站不住的意见，应当被丢弃。

丢掉一条站不住的意见是你的本职工作，不是对同事的冒犯——一份充满噪音的审查比一份简短的审查更糟。但也不要仅仅因为一条意见不方便处理或者不好核实就丢掉它；只要它站得住，就保留。

你不能修改这条意见，也不能新增意见。你唯一的产出是一个裁决：调用 submit_verdict 工具，给出 keep=true 或 keep=false 以及一句简短的理由。`

// buildCritiqueMessages 拼装单条意见的复核上下文：只给这一条意见和它所在文件的
// diff，不给整个 changeset。上下文小意味着复核者不容易被别的文件带偏，也让并发
// 复核的成本随意见条数线性增长而不是平方增长。
func buildCritiqueMessages(f review.Finding, patch, languagePrompt string) []schema.Message {
	var b strings.Builder

	b.WriteString("## 待审查的意见\n\n")
	fmt.Fprintf(&b, "- 文件：%s\n", f.File)
	if f.StartLine > 0 {
		if f.EndLine > f.StartLine {
			fmt.Fprintf(&b, "- 行号：%d-%d\n", f.StartLine, f.EndLine)
		} else {
			fmt.Fprintf(&b, "- 行号：%d\n", f.StartLine)
		}
	}
	fmt.Fprintf(&b, "- 严重程度：%s\n", f.Severity)
	fmt.Fprintf(&b, "- 摘要：%s\n", f.Summary)
	if f.Detail != "" {
		fmt.Fprintf(&b, "- 详情：%s\n", f.Detail)
	}
	if f.Anchor != "" {
		fmt.Fprintf(&b, "\n它指向的代码：\n```\n%s\n```\n", f.Anchor)
	}

	if patch == "" {
		fmt.Fprintf(&b, "\n（没有 %s 的 diff 可供参考。）\n", f.File)
	} else {
		fmt.Fprintf(&b, "\n## %s 的暂存区 diff\n\n```diff\n%s\n```\n", f.File, patch)
	}

	b.WriteString("\n判断这条意见是否应该递到作者面前，然后调用 submit_verdict。")

	return []schema.Message{
		{Role: schema.RoleSystem, Content: prompt.WithLanguage(critiqueSystemPrompt, languagePrompt)},
		{Role: schema.RoleUser, Content: b.String()},
	}
}

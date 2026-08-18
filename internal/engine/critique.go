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
// 单条复核失败（模型报错、超轮数、上下文取消）不会中断整体：那一条按"保留"
// 处理并在 CritiqueReason 里写明原因。理由是失败属于系统问题而非意见本身的
// 问题，因为基础设施抖动就默默吞掉一条可能有效的审查意见，是最糟的失败方式。
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
				// 保留而不是丢弃：见函数注释里对失败的处理原则。
				log.Error("复核失败，保留该条意见", "findingID", f.ID, "error", err)
				out.Findings[i].Kept = true
				out.Findings[i].CritiqueReason = fmt.Sprintf("critique did not complete (%v); keeping the finding", err)
				return nil
			}
			out.Findings[i].Kept = verdict.Keep
			out.Findings[i].CritiqueReason = verdict.Reason
			log.Info("复核完成", "findingID", f.ID, "kept", verdict.Keep, "reason", verdict.Reason)
			return nil
		})
	}
	// 每个 goroutine 都吞掉了自己的错误，这里不会拿到非 nil；保留返回值检查是
	// 为了将来真的引入"应当中断整体"的错误时不会漏掉。
	if err := g.Wait(); err != nil {
		return review.Report{}, fmt.Errorf("critique findings: %w", err)
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
			return tool.Verdict{}, fmt.Errorf("generate verdict: %w", err)
		}
		msgs = append(msgs, *resp)

		if len(resp.ToolCalls) == 0 {
			msgs = append(msgs, schema.Message{
				Role: schema.RoleUser,
				Content: fmt.Sprintf("Your verdict has not been recorded: it is only accepted through the %s tool. Call %s now.",
					tool.CritiqueVerdictName, tool.CritiqueVerdictName),
			})
			continue
		}

		for _, tc := range resp.ToolCalls {
			var result tool.Result
			if t, err := tool.FindToolByName(deps.Tools, tc.Name); err != nil {
				result = tool.Result{
					Output:  fmt.Sprintf("unknown tool %q; no such tool is available. Use only the tools provided in this session.", tc.Name),
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

	return tool.Verdict{}, fmt.Errorf("exceeded max critique turns (%d) without calling %s", maxTurns, tool.CritiqueVerdictName)
}

// critiqueSystemPrompt 定义复核者的角色。措辞的重点是压住"复核者倾向于附和
// 初审"的倾向——同一个模型自我批判时，天然不容易否定自己，所以这里明确告诉它
// 删掉没根据的意见是它的本职工作，而不是对同事的冒犯。
const critiqueSystemPrompt = `You are a strict reviewer-of-reviews. Another reviewer has raised one comment about a staged code change, and your job is to decide whether that comment deserves to reach the author.

Keep the comment only if it identifies a real problem the author should act on. Drop it if it is speculative, based on a misreading of the code, already handled elsewhere, or too trivial to be worth the author's attention.

You have read-only tools (glob, grep, read_file) and you are expected to use them. Do not take the comment's claims at face value: if it says a function mishandles some input, go read that function. A comment that sounds plausible but does not survive contact with the actual code should be dropped.

Dropping a weak comment is your job, not a discourtesy — a review full of noise is worse than a short one. But do not drop a comment merely because it is inconvenient or hard to verify; if it holds up, keep it.

You cannot edit the comment or add new ones. Your only output is a verdict: call the submit_verdict tool with keep=true or keep=false and a short reason.`

// buildCritiqueMessages 拼装单条意见的复核上下文：只给这一条意见和它所在文件的
// diff，不给整个 changeset。上下文小意味着复核者不容易被别的文件带偏，也让并发
// 复核的成本随意见条数线性增长而不是平方增长。
func buildCritiqueMessages(f review.Finding, patch, languagePrompt string) []schema.Message {
	var b strings.Builder

	b.WriteString("## Comment under examination\n\n")
	fmt.Fprintf(&b, "- file: %s\n", f.File)
	if f.StartLine > 0 {
		if f.EndLine > f.StartLine {
			fmt.Fprintf(&b, "- lines: %d-%d\n", f.StartLine, f.EndLine)
		} else {
			fmt.Fprintf(&b, "- line: %d\n", f.StartLine)
		}
	}
	fmt.Fprintf(&b, "- severity: %s\n", f.Severity)
	fmt.Fprintf(&b, "- summary: %s\n", f.Summary)
	if f.Detail != "" {
		fmt.Fprintf(&b, "- detail: %s\n", f.Detail)
	}
	if f.Anchor != "" {
		fmt.Fprintf(&b, "\nThe code it points at:\n```\n%s\n```\n", f.Anchor)
	}

	if patch == "" {
		fmt.Fprintf(&b, "\n(No diff is available for %s.)\n", f.File)
	} else {
		fmt.Fprintf(&b, "\n## Staged diff for %s\n\n```diff\n%s\n```\n", f.File, patch)
	}

	b.WriteString("\nDecide whether this comment should reach the author, then call submit_verdict.")

	return []schema.Message{
		{Role: schema.RoleSystem, Content: prompt.WithLanguage(critiqueSystemPrompt, languagePrompt)},
		{Role: schema.RoleUser, Content: b.String()},
	}
}

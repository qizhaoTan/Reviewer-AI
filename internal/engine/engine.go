// Package engine 编排一次完整的审查运行：找到（或恢复）一条 Run 记录，
// 驱动 provider/tool 的 agentic 循环直到给出最终答案，并在每个关键节点落盘。
//
// engine 只关心"如何跑完一次审查"，不关心命令行参数、配置文件怎么加载——
// 这些由调用方（cmd/reviewer-ai）组装好 Deps 后传入，engine 不导入 config/flag。
package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/log"
	"github.com/qizhaoTan/Reviewer-AI/internal/prompt"
	"github.com/qizhaoTan/Reviewer-AI/internal/review"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
	"github.com/qizhaoTan/Reviewer-AI/internal/store"
	"github.com/qizhaoTan/Reviewer-AI/internal/tool"
)

// 三个常量共同描述一次审查运行的轮次预算。刻意写死而不做成配置项：
// 这是一组需要一起调的数字，单独暴露某一个只会让它们失配。
const (
	// maxToolLoopIterations 是调查阶段（模型可以自由使用只读工具）的最大轮数。
	maxToolLoopIterations = 60

	// forcedSubmitIterations 是调查阶段用尽后额外追加的收尾轮数。这几轮里模型
	// 只能看到 submit_review 一个工具——实测模型跑满 60 轮不收工时，手上其实
	// 已经有足够的判断依据，缺的只是"停下来"这个动作，所以把探索的路堵死比
	// 再给它 20 轮自由调查更有效。
	forcedSubmitIterations = 5

	// warnBeforeExhaust 是在调查阶段还剩几轮时开始注入预警。模型不知道自己有
	// 轮次上限，不提醒的话它会一直按"时间无限"的节奏往下查，直到被硬砍。
	warnBeforeExhaust = 5
)

// Deps 是 Run 所需的外部依赖，由调用方组装好后传入。
type Deps struct {
	LLM   schema.IProvider
	Store *store.Store
	Tools []tool.ITool

	// CritiqueTools 是复核阶段可用的工具：只读工具加上 submit_verdict。
	// 与 Tools 分开是因为两个阶段的收尾工具不同——复核者不该看到
	// submit_review（它无权改写审查结论），初审也不该看到 submit_verdict。
	// 留空则跳过复核。
	CritiqueTools []tool.ITool

	// CritiqueConcurrency / CritiqueMaxTurns 见 CritiqueDeps 上的同名字段，
	// 由调用方从配置读出后传入。
	CritiqueConcurrency int
	CritiqueMaxTurns    int

	// LanguagePrompt 是追加到 system prompt 末尾的语言约束，初审和复核两个
	// 阶段共用同一份——否则会出现审查意见是中文、复核理由是英文的割裂结果。
	// 由调用方从配置读出（config.File.LanguagePromptOrDefault）后传入；
	// 留空表示不加语言约束。
	LanguagePrompt string

	// Fresh 为 true 时跳过历史记录复用，强制新建一条运行记录从头审查。
	// 需要它是因为可恢复失败（见 recoverableRun）会让记录留在 in_progress
	// 并被无限续跑：如果一条记录的消息历史本身就是坏的（比如模型钻进了死
	// 胡同），续跑只会一直撞同一堵墙，得有个逃生舱能重开一局。
	Fresh bool
}

// Run 执行一次完整的审查：查找或新建一条 Run 记录，驱动 tool loop 直到模型
// 调用 submit_review 提交结构化结果，期间每轮往返都会落盘。
//
// repoAbs 是仓库的绝对路径，branch 是当前分支名（调用方通过 gitdiff.CurrentBranch
// 获得），两者拼成 runKey 用于隔离不同分支的运行记录，见 buildRunKey 注释。
// changes 是本次采集到的变更快照；timeout 是整个循环共用的总预算。
// baseRev 在 `-base` 模式下是用户指定的基准 revision（快照来自 base...HEAD），
// 暂存区审查时留空——它同时参与 runKey 的构造，并落盘到记录上供增量重审复用。
//
// 如果本次快照内容与某次历史运行（不要求是最近一次）完全一致且那次已经
// completed，直接复用那次的结果、不再调用模型——常见于 git stash 之后又
// stash pop 回同样的内容，或者反复对同一份暂存区改动跑审查的场景。
//
// 返回的 *store.Run 在成功时 Status 为 store.StatusCompleted，审查结果可以从
// run.Report() 取出。失败时返回的 error 会说明具体原因，对应的 Run 记录已经在
// 返回前被标记为 store.StatusFailed 并落盘，不需要调用方再做任何清理。
func Run(ctx context.Context, deps Deps, repoAbs, branch, baseRev string, changes []gitdiff.Change, timeout time.Duration) (*store.Run, error) {
	runKey := buildRunKey(repoAbs, branch, baseRev)

	run, err := resumeOrStartRun(ctx, deps.Store, runKey, baseRev, changes, deps.LanguagePrompt, deps.Fresh)
	if err != nil {
		return nil, fmt.Errorf("resume or start run: %w", err)
	}
	if run.Status == store.StatusCompleted {
		log.Info("命中内容相同的历史审查结果，直接复用", "runID", run.ID, "findings", len(run.Findings))
		return run, nil
	}

	// 命中一条初审已完成但复核没跑完的记录：跳过主循环，直接从复核阶段接着跑，
	// 不让模型把整个 changeset 重审一遍。
	if len(run.Findings) > 0 && !run.Critiqued {
		log.Info("初审结果已存在但复核未完成，直接进入复核阶段", "runID", run.ID, "findings", len(run.Findings))
		if err := finishWithCritique(ctx, deps, run, repoAbs, changes); err != nil {
			return nil, err
		}
		return run, nil
	}
	msgs := run.Messages

	toolDefinitions := make([]schema.ToolDefinition, len(deps.Tools))
	for i, t := range deps.Tools {
		toolDefinitions[i] = t.Definition()
	}

	genCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// deduper 跨轮记录已执行过的工具调用，用于拦截模型的重复搜索，见 dedup.go。
	deduper := newCallDeduper()

	// 收尾阶段只把 submit_review 递给模型：调查阶段的轮次已经花完，再让它看见
	// 只读工具就等于继续邀请它探索。找不到 submit_review（调用方装配 Tools 时
	// 漏了）就退化成"工具集不变"，此时收尾阶段与调查阶段无异，但至少不会让
	// 模型面对一个空工具列表。
	forcedTools := submitOnlyTools(toolDefinitions)

	totalIterations := maxToolLoopIterations + forcedSubmitIterations
	for iteration := 1; iteration <= totalIterations; iteration++ {
		forced := iteration > maxToolLoopIterations

		// 预警和收尾通告都作为一条 user 消息追加进历史，和"未调用工具"的提醒
		// 走同一条路子。它们会随历史落盘、在续跑时留下来——续跑的轮次计数会
		// 重置，这条过期提醒因此会略微失真，但它只是催促模型收尾，方向和续跑
		// 时想要的行为一致，所以不值得为它引入一套"临时消息"机制。
		if notice := budgetNotice(iteration); notice != "" {
			msgs = append(msgs, schema.Message{Role: schema.RoleUser, Content: notice})
			setMessages(ctx, deps.Store, run, msgs)
		}

		activeTools := toolDefinitions
		if forced {
			activeTools = forcedTools
		}

		resp, err := deps.LLM.Generate(genCtx, msgs, activeTools)
		if err != nil {
			// 刻意**不**标记 failed：Generate 出错基本都是网络抖动或超出本次
			// 时间预算这类外部原因，跟已经攒下来的消息历史无关。留在
			// in_progress，下次重跑时 resumeOrStartRun 就能沿用这些历史接着
			// 问下去；标记成 failed 反而会让它被判定为不可复用、重新审一遍，
			// 白白丢掉这一轮之前所有已经付过费的工具调用。
			return nil, recoverableRun(run, "generate review: %w", err)
		}
		msgs = append(msgs, *resp)
		setMessages(ctx, deps.Store, run, msgs)

		// 模型没有发起任何工具调用，说明它在用自由文本作答——但审查结果只认
		// submit_review。把这件事作为工具错误告诉它，让它补上这次调用，
		// 而不是直接把这段文本当成最终结果收下。
		if len(resp.ToolCalls) == 0 {
			log.Info("模型未调用任何工具，提示它用 submit_review 收尾")
			msgs = append(msgs, schema.Message{
				Role: schema.RoleUser,
				Content: fmt.Sprintf("你的审查结果还没有被记录：审查结果只能通过 %s 工具提交。"+
					"现在就调用 %s 并给出你的意见（如果这批改动看起来没问题，findings 传空数组）。",
					tool.SubmitReviewName, tool.SubmitReviewName),
			})
			setMessages(ctx, deps.Store, run, msgs)
			continue
		}

		for _, tc := range resp.ToolCalls {
			log.Info("尝试调用工具", "tool", tc.Name, "arguments", tc.Arguments)
		}

		var report *review.Report
		for _, tc := range resp.ToolCalls {
			// 参数完全相同的调用已经成功跑过：不再执行，回一句提醒让模型去翻上文。
			// 拦下来省的不是这次 grep 的几毫秒，而是它引出的后续模型往返。
			if round, repeated := deduper.firstSeenRound(tc); repeated {
				log.Info("拦截重复的工具调用", "tool", tc.Name, "arguments", tc.Arguments, "firstSeenRound", round)
				msgs = append(msgs, schema.Message{
					Role:       schema.RoleUser,
					Content:    repeatNotice(tc, round),
					ToolCallID: tc.ID,
				})
				continue
			}

			var result tool.Result
			switch t, err := tool.FindToolByName(deps.Tools, tc.Name); {
			case err != nil:
				result = tool.Result{
					Output:  fmt.Sprintf("未知工具 %q，不存在这个工具。只能使用本次会话中提供给你的那些工具。", tc.Name),
					IsError: true,
				}
			case forced && tc.Name != tool.SubmitReviewName:
				// 收尾阶段仍然调只读工具：多半是模型照着上文里的旧工具定义继续
				// 探索。执行它只会再喂出一批新线索、把这几轮也烧光，所以直接拒绝，
				// 并说清楚现在唯一该做的动作是什么。
				log.Info("收尾阶段拒绝只读工具调用", "tool", tc.Name)
				result = tool.Result{Output: forcedToolRejection(tc.Name), IsError: true}
			default:
				result = t.Execute(ctx, repoAbs, tc.Arguments)
			}

			if result.IsError {
				log.Error("调用工具失败", "tool", tc.Name, "arguments", tc.Arguments, "output", result.Output)
			} else {
				log.Info("调用工具成功", "tool", tc.Name, "arguments", tc.Arguments, "outputLen", len(result.Output))
			}
			// ReviewResult 非 nil 即"审查已提交"，不需要按工具名做字符串比较。
			// 同一轮里若提交了多次，以最后一次为准（与工具自身的覆盖语义一致）。
			if result.ReviewResult != nil {
				report = result.ReviewResult
			}

			// 只登记成功的只读调用：失败的调用（比如正则写错）模型有权改了再试；
			// 带 ReviewResult 的 submit_review 是收尾动作，本来也不会有第二次。
			if !result.IsError && result.ReviewResult == nil {
				deduper.record(tc, iteration)
			}
			msgs = append(msgs, schema.Message{
				Role:       schema.RoleUser,
				Content:    result.Output,
				ToolCallID: tc.ID,
			})
		}
		setMessages(ctx, deps.Store, run, msgs)

		if report != nil {
			log.Info("收到结构化审查结果", "findings", len(report.Findings))
			// 先把初审结果落盘再跑复核：复核是一批重活（每条意见一个 tool loop），
			// 跑到一半崩掉却没存初审结果的话，恢复时得让模型把整个 changeset 重审
			// 一遍，白白浪费已经完成的初审。
			setReport(ctx, deps.Store, run, *report)
			if err := finishWithCritique(ctx, deps, run, repoAbs, changes); err != nil {
				return nil, err
			}
			return run, nil
		}
	}

	return nil, failRun(ctx, deps.Store, run,
		"exceeded max tool-call iterations (%d investigation + %d forced submit) without calling %s",
		maxToolLoopIterations, forcedSubmitIterations, tool.SubmitReviewName)
}

// budgetNotice 返回第 iteration 轮开始前要注入的轮次提醒，没有可提醒的返回空串。
//
// 模型看不到轮次上限，只靠自己的节奏感决定查到什么程度。实测这个节奏感在
// 大改动上会失灵：它一路往下查，直到被硬砍在 60 轮，一条意见都没提交。所以
// 在预算快见底时明说还剩几轮，把"该收尾了"变成上下文里的事实而不是暗示。
//
// 预警只在剩余轮数刚好等于 warnBeforeExhaust 的那一轮发一次，不是每轮都发：
// 连着刷同一句话会稀释它的分量，也会把历史撑大。
func budgetNotice(iteration int) string {
	switch {
	case iteration == maxToolLoopIterations-warnBeforeExhaust+1:
		return fmt.Sprintf("提醒：本次审查的调查轮次即将用尽，你还剩 %d 轮可以调用工具。"+
			"请尽快停止进一步探索，用已经掌握的信息调用 %s 提交审查结果"+
			"（如果没发现问题，findings 传空数组）。",
			warnBeforeExhaust, tool.SubmitReviewName)
	case iteration == maxToolLoopIterations+1:
		return fmt.Sprintf("调查轮次已经用尽，现在已禁止使用只读工具，只剩 %s 一个工具可用。"+
			"不要再尝试读取或搜索代码——就用你已经掌握的信息，现在必须调用 %s 提交审查结果"+
			"（如果没发现问题，findings 传空数组）。",
			tool.SubmitReviewName, tool.SubmitReviewName)
	}
	return ""
}

// forcedToolRejection 是收尾阶段拒绝只读工具调用时回喂给模型的错误信息。
// 按项目约定，工具错误必须同时说清"错在哪"和"该怎么改"。
func forcedToolRejection(name string) string {
	return fmt.Sprintf("工具 %q 在当前阶段不可用：调查轮次已经用尽，只剩 %s 可以调用。"+
		"不要再调查代码，直接用已有信息调用 %s 提交审查结果（如果没发现问题，findings 传空数组）。",
		name, tool.SubmitReviewName, tool.SubmitReviewName)
}

// submitOnlyTools 从完整工具定义里挑出 submit_review 一个，作为收尾阶段的工具集。
// 找不到就原样返回——调用方漏装 submit_review 是配置问题，不该由这里静默地把
// 模型的工具列表清空。
func submitOnlyTools(all []schema.ToolDefinition) []schema.ToolDefinition {
	for _, d := range all {
		if d.Name == tool.SubmitReviewName {
			return []schema.ToolDefinition{d}
		}
	}
	return all
}

// buildRunKey 把分支名并入 repo 路径，让同一仓库不同分支的运行记录互不干扰——
// 否则在分支 A 上中断的审查，切到分支 B 后会被误判为"同一次运行"继续恢复，
// 模型会拿着分支 A 的 diff/工具调用历史去审查分支 B 的暂存区。
//
// baseRev 非空（`-base` 模式）时再追加一段，让同一分支上的两种审查各自成链：
// 在 a 分支跑 `cr` 审的是暂存区，跑 `cr -base=dev` 审的是整个分支的改动，
// 两者内容不同、基线不同，共用一个 key 会让增量重审拿错比较对象。
func buildRunKey(repoAbs, branch, baseRev string) string {
	key := repoAbs + "#" + branch
	if baseRev != "" {
		key += baseKeySuffix + baseRev
	}
	return key
}

// baseKeySuffix 是 runKey 里 base 段的前缀。单独定义是因为 buildRunKey 和
// splitRunKey 必须用同一个字面量，散在两处迟早会改漏一个。
const baseKeySuffix = "#base="

// resumeOrStartRun 在 runKey 下按本次快照内容的 hash 查找运行记录（不限制状态、
// 不要求是最新一条，见 store.LoadRunByHash）：
//   - 命中 in_progress：恢复它，沿用已持久化的 Messages 继续跑 tool loop，
//     不重新拼装 prompt，避免重复调用已经问过的工具。
//   - 命中 completed：直接返回，调用方（Run）会据此短路，不再调用模型。
//   - 未命中（包括命中了但状态是 failed，或从未审查过这份内容）：新建一条记录。
//
// 用内容 hash 而不是"最新一条记录"做比较，是为了覆盖 git stash / stash pop
// 这类场景——旧记录不会因为不是最新就被忽略，只要内容相同就能找到。
//
// fresh 为 true 时跳过上面整套匹配，无条件新建一条记录，见 Deps.Fresh。
func resumeOrStartRun(ctx context.Context, db *store.Store, runKey, baseRev string, changes []gitdiff.Change, languagePrompt string, fresh bool) (*store.Run, error) {
	hash := gitdiff.SnapshotHash(changes)
	matched, err := db.LoadRunByHash(ctx, runKey, hash)
	if err != nil {
		return nil, fmt.Errorf("load run by hash: %w", err)
	}
	if fresh && matched != nil {
		log.Info("已指定 fresh，忽略可复用的历史记录，重新开始审查", "ignoredRunID", matched.ID, "ignoredStatus", matched.Status)
		matched = nil
	}
	if matched != nil {
		switch matched.Status {
		case store.StatusInProgress:
			log.Info("恢复未完成的审查运行", "runID", matched.ID, "messages", len(matched.Messages))
			return matched, nil
		case store.StatusCompleted:
			return matched, nil
		}
		// StatusFailed 或其他状态：不复用，走下面的新建分支。
	}

	run := &store.Run{
		ID:       store.NewRunID(),
		RepoPath: runKey,
		Status:   store.StatusInProgress,
		Snapshot: changes,
		Messages: prompt.BuildInitial(changes, languagePrompt),
		BaseRev:  baseRev,
	}
	if err := db.SaveRun(ctx, *run); err != nil {
		return nil, fmt.Errorf("save initial run: %w", err)
	}
	return run, nil
}

// setMessages 更新 run 的 Messages 并立即落盘。engine 内部只应通过这个函数（以及
// setStatus）修改 run 的字段——两者都会自动持久化，杜绝"改了字段忘记 SaveRun"的问题。
// 落盘失败时只记录日志不中断流程：审查本身仍应继续，下一轮往返还会再落盘一次。
func setMessages(ctx context.Context, db *store.Store, run *store.Run, messages []schema.Message) {
	run.Messages = messages
	if err := db.SaveRun(ctx, *run); err != nil {
		log.Error("保存运行状态失败", "runID", run.ID, "error", err)
	}
}

// setReport 把审查结果写进 run 并立即落盘，语义和失败处理方式与 setMessages 一致。
func setReport(ctx context.Context, db *store.Store, run *store.Run, report review.Report) {
	run.SetReport(report)
	if err := db.SaveRun(ctx, *run); err != nil {
		log.Error("保存审查结果失败", "runID", run.ID, "error", err)
	}
}

// finishWithCritique 跑复核阶段，把结果落盘并把运行标记为完成。
//
// 复核失败时刻意**不**把运行标记为 failed，而是让它留在 in_progress：初审结果
// 已经落盘了，这条记录的真实状态就是"初审已完成、复核未完成"。标记成 failed
// 反而会让 resumeOrStartRun 把它当成不可复用的记录、下次重新跑一遍初审
// （见那里对 StatusFailed 的处理），白白丢掉已经付过费的初审结果。
//
// 下次重跑时 Run 会命中"初审已完成但复核未完成"那条分支，直接从复核接着跑。
// 注意复核结果目前不逐条落盘，所以重跑会把所有意见都重新复核一遍——复核单条
// 的成本远低于初审整个 changeset，先这么办；等真觉得贵了再考虑逐条落盘。
func finishWithCritique(ctx context.Context, deps Deps, run *store.Run, repoAbs string, changes []gitdiff.Change) error {
	final, err := critiqueReport(ctx, deps, run.Report(), repoAbs, changes)
	if err != nil {
		// wrapRecoverable 而不是 fmt.Errorf：加了这层上下文之后，
		// 调用方仍然要能看出底下那个失败是不是可恢复的。
		return wrapRecoverable(err, "critique review: %w")
	}
	setReport(ctx, deps.Store, run, final)
	setStatus(ctx, deps.Store, run, store.StatusCompleted)
	return nil
}

// setStatus 更新 run 的 Status 并立即落盘，语义和失败处理方式与 setMessages 一致。
func setStatus(ctx context.Context, db *store.Store, run *store.Run, status store.RunStatus) {
	run.Status = status
	if err := db.SaveRun(ctx, *run); err != nil {
		log.Error("保存运行状态失败", "runID", run.ID, "error", err)
	}
}

// failRun 把 run 标记为 failed 并落盘，然后返回格式化好的错误供调用方处理。
// 只用于不可恢复的失败——重跑同一份消息历史也只会再次撞上同样的墙，
// 典型例子是超出轮数上限。可恢复的失败请用 recoverableRun。
func failRun(ctx context.Context, db *store.Store, run *store.Run, format string, args ...any) error {
	setStatus(ctx, db, run, store.StatusFailed)
	return fmt.Errorf(format, args...)
}

// recoverableRun 处理可恢复的失败：只返回错误，**不动** run 的状态，让它保持
// in_progress，这样下次重跑会被 resumeOrStartRun 认出来、沿用已落盘的消息历史
// 接着跑，而不是从头审一遍。
//
// 之所以是一个什么都不做的函数而不是直接 return fmt.Errorf，是为了在调用点
// 和 failRun 形成对称、让"这里选的是哪一种失败语义"一眼可见——两个分支长得
// 一样的话，很容易在后续修改中不知不觉退化成全都标记 failed。
func recoverableRun(run *store.Run, format string, args ...any) error {
	log.Info("本次运行以可恢复的失败结束，记录保持 in_progress 以便下次续跑", "runID", run.ID, "messages", len(run.Messages))
	return recoverable(fmt.Errorf(format, args...))
}

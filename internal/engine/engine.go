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

// maxToolLoopIterations 是单次审查运行里 Generate 调用的最大轮数，防止模型无限循环调用工具。
const maxToolLoopIterations = 30

// Deps 是 Run 所需的外部依赖，由调用方组装好后传入。
type Deps struct {
	LLM   schema.IProvider
	Store *store.Store
	Tools []tool.ITool
}

// Run 执行一次完整的审查：查找或新建一条 Run 记录，驱动 tool loop 直到模型
// 调用 submit_review 提交结构化结果，期间每轮往返都会落盘。
//
// repoAbs 是仓库的绝对路径，branch 是当前分支名（调用方通过 gitdiff.CurrentBranch
// 获得），两者拼成 runKey 用于隔离不同分支的运行记录，见 buildRunKey 注释。
// changes 是本次采集到的暂存区快照；timeout 是整个循环共用的总预算。
//
// 如果本次快照内容与某次历史运行（不要求是最近一次）完全一致且那次已经
// completed，直接复用那次的结果、不再调用模型——常见于 git stash 之后又
// stash pop 回同样的内容，或者反复对同一份暂存区改动跑审查的场景。
//
// 返回的 *store.Run 在成功时 Status 为 store.StatusCompleted，第二个返回值是模型
// 提交的结构化审查结果。失败时返回的 error 会说明具体原因，对应的 Run 记录已经在
// 返回前被标记为 store.StatusFailed 并落盘，不需要调用方再做任何清理。
//
// TODO(阶段二 2.6)：Report 目前只能随返回值传出，命中 completed 缓存时因此拿不到
// 历史结果（见下方分支）。等 store.Run 加上 Findings/Summary 字段后，Report 就能
// 随记录一起持久化，缓存命中的路径也能返回完整结果。
func Run(ctx context.Context, deps Deps, repoAbs, branch string, changes []gitdiff.Change, timeout time.Duration) (*store.Run, *review.Report, error) {
	runKey := buildRunKey(repoAbs, branch)

	run, err := resumeOrStartRun(ctx, deps.Store, runKey, changes)
	if err != nil {
		return nil, nil, fmt.Errorf("resume or start run: %w", err)
	}
	if run.Status == store.StatusCompleted {
		// Findings 尚未持久化，所以这里只能返回记录本身、Report 为 nil。
		// 调用方需要能处理这种情况，直到 2.6 补上存储。
		log.Info("命中内容相同的历史审查结果，直接复用", "runID", run.ID)
		return run, nil, nil
	}
	msgs := run.Messages

	toolDefinitions := make([]schema.ToolDefinition, len(deps.Tools))
	for i, t := range deps.Tools {
		toolDefinitions[i] = t.Definition()
	}

	genCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for range maxToolLoopIterations {
		resp, err := deps.LLM.Generate(genCtx, msgs, toolDefinitions)
		if err != nil {
			return nil, nil, failRun(ctx, deps.Store, run, "generate review: %w", err)
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
				Content: fmt.Sprintf("Your review has not been recorded: a review is only accepted through the %s tool. "+
					"Call %s now with your findings (or an empty findings array if the changeset looks fine).",
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
			var result tool.Result
			if t, err := tool.FindToolByName(deps.Tools, tc.Name); err != nil {
				result = tool.Result{
					Output:  fmt.Sprintf("unknown tool %q; no such tool is available. Use only the tools provided in this session.", tc.Name),
					IsError: true,
				}
			} else {
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
			msgs = append(msgs, schema.Message{
				Role:       schema.RoleUser,
				Content:    result.Output,
				ToolCallID: tc.ID,
			})
		}
		setMessages(ctx, deps.Store, run, msgs)

		if report != nil {
			log.Info("收到结构化审查结果", "findings", len(report.Findings))
			setStatus(ctx, deps.Store, run, store.StatusCompleted)
			return run, report, nil
		}
	}

	return nil, nil, failRun(ctx, deps.Store, run, "exceeded max tool-call iterations (%d) without calling %s", maxToolLoopIterations, tool.SubmitReviewName)
}

// buildRunKey 把分支名并入 repo 路径，让同一仓库不同分支的运行记录互不干扰——
// 否则在分支 A 上中断的审查，切到分支 B 后会被误判为"同一次运行"继续恢复，
// 模型会拿着分支 A 的 diff/工具调用历史去审查分支 B 的暂存区。
func buildRunKey(repoAbs, branch string) string {
	return repoAbs + "#" + branch
}

// resumeOrStartRun 在 runKey 下按本次快照内容的 hash 查找运行记录（不限制状态、
// 不要求是最新一条，见 store.LoadRunByHash）：
//   - 命中 in_progress：恢复它，沿用已持久化的 Messages 继续跑 tool loop，
//     不重新拼装 prompt，避免重复调用已经问过的工具。
//   - 命中 completed：直接返回，调用方（Run）会据此短路，不再调用模型。
//   - 未命中（包括命中了但状态是 failed，或从未审查过这份内容）：新建一条记录。
//
// 用内容 hash 而不是"最新一条记录"做比较，是为了覆盖 git stash / stash pop
// 这类场景——旧记录不会因为不是最新就被忽略，只要内容相同就能找到。
func resumeOrStartRun(ctx context.Context, db *store.Store, runKey string, changes []gitdiff.Change) (*store.Run, error) {
	hash := gitdiff.SnapshotHash(changes)
	matched, err := db.LoadRunByHash(ctx, runKey, hash)
	if err != nil {
		return nil, fmt.Errorf("load run by hash: %w", err)
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
		Messages: prompt.BuildInitial(changes),
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

// setStatus 更新 run 的 Status 并立即落盘，语义和失败处理方式与 setMessages 一致。
func setStatus(ctx context.Context, db *store.Store, run *store.Run, status store.RunStatus) {
	run.Status = status
	if err := db.SaveRun(ctx, *run); err != nil {
		log.Error("保存运行状态失败", "runID", run.ID, "error", err)
	}
}

// failRun 把 run 标记为 failed 并落盘，然后返回格式化好的错误供调用方处理。
func failRun(ctx context.Context, db *store.Store, run *store.Run, format string, args ...any) error {
	setStatus(ctx, db, run, store.StatusFailed)
	return fmt.Errorf(format, args...)
}

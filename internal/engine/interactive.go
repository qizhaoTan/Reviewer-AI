package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/log"
	"github.com/qizhaoTan/Reviewer-AI/internal/review"
	"github.com/qizhaoTan/Reviewer-AI/internal/store"
	"github.com/qizhaoTan/Reviewer-AI/internal/tool"
)

// Interactive 实现 Web 侧需要的两个交互动作（reply / 增量重审）。
//
// 它是 web.Reviewer 接口的实现，但刻意不 import web：依赖方向是 cmd 把
// *Interactive 注入 web，engine 不知道 web 的存在。
type Interactive struct {
	// Deps 是跑一次完整审查所需的依赖，与命令行路径共用同一份。
	Deps Deps

	// ReplyTools 是 reply 对话可用的工具：只读工具 + withdraw_finding。
	// 与 Deps.Tools 分开的理由同 Deps.CritiqueTools——每个阶段的收尾工具不同，
	// reply 里不该出现 submit_review（它无权重写整份审查结论）。
	ReplyTools []tool.ITool

	// ReplyMaxTurns 是单次 reply 对话的轮数上限，<=0 时取 engine 的默认值。
	ReplyMaxTurns int

	// Timeout 是单次动作的总预算，透传给底层的 tool loop。
	Timeout time.Duration

	// LoadStaged 采集指定仓库当前的暂存区快照，增量重审要拿它跟基线比。
	// 做成字段而不是直接调 gitdiff.LoadStaged，是为了测试能注入一份固定的
	// 快照，不必真的建一个 git 仓库。
	LoadStaged func(ctx context.Context, repoDir string) ([]gitdiff.Change, error)
}

// Reply 就用户对 findingID 这条意见的异议跑一次对话，把结论与讨论记录落盘。
//
// 落盘只写 findings 一列（store.SaveFindings），不做整行 upsert：命令行可能
// 正在同一条记录上跑审查，整行写回会用这份读旧了的快照盖掉它的进展。
func (i *Interactive) Reply(ctx context.Context, runID, findingID, userReply string) (*store.Run, error) {
	run, err := i.Deps.Store.LoadRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("load run %s: %w", runID, err)
	}
	if run == nil {
		return nil, fmt.Errorf("run %s does not exist", runID)
	}

	idx := indexOfFinding(run.Findings, findingID)
	if idx < 0 {
		return nil, fmt.Errorf("run %s has no finding %s", runID, findingID)
	}
	f := run.Findings[idx]

	repoAbs, _ := splitRunKey(run.RepoPath)
	ctx, cancel := i.withTimeout(ctx)
	defer cancel()

	outcome, err := Reply(ctx, ReplyDeps{
		LLM:            i.Deps.LLM,
		Tools:          i.ReplyTools,
		RepoRoot:       repoAbs,
		MaxTurns:       i.ReplyMaxTurns,
		LanguagePrompt: i.Deps.LanguagePrompt,
	}, f, PatchForFile(run.Snapshot, f.File), userReply, f.Discussion)
	if err != nil {
		return nil, fmt.Errorf("reply about finding %s: %w", findingID, err)
	}

	// 讨论记录一律追加，无论模型是撤回还是坚持——用户下次翻这条意见时
	// 该看到完整的往复，而不只是最后的结论。
	run.Findings[idx].Discussion = append(run.Findings[idx].Discussion, outcome.Messages...)
	if outcome.Withdrawn {
		run.Findings[idx].Status = review.StatusWithdrawn
	}

	if err := i.Deps.Store.SaveFindings(ctx, runID, run.Findings); err != nil {
		return nil, fmt.Errorf("persist reply outcome: %w", err)
	}
	log.Info("reply 完成", "runID", runID, "findingID", findingID, "withdrawn", outcome.Withdrawn)
	return run, nil
}

// Rereview 基于 runID 这次审查做一次增量重审：只把真正改动过的文件重新送审，
// 未改动文件的意见原样带进新记录。
//
// 结果写成一条**新的** Run（ParentRunID 指回基线），而不是原地更新旧记录：
// 旧记录是那次审查的事实存档，用户按意见改完代码之后，"上次报了什么、这次
// 还剩什么"是两条独立的信息，覆盖掉前者就没法对比了。
func (i *Interactive) Rereview(ctx context.Context, runID string) (*store.Run, error) {
	base, err := i.Deps.Store.LoadRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("load run %s: %w", runID, err)
	}
	if base == nil {
		return nil, fmt.Errorf("run %s does not exist", runID)
	}

	repoAbs, branch := splitRunKey(base.RepoPath)
	current, err := i.loadStaged(ctx, repoAbs)
	if err != nil {
		return nil, fmt.Errorf("load staged changes for %s: %w", repoAbs, err)
	}
	if len(current) == 0 {
		return nil, fmt.Errorf("nothing is staged in %s; stage your fixes before re-reviewing", repoAbs)
	}

	split := review.DiffSnapshot(base.Snapshot, current)
	carried := review.CarryOverFindings(base.Findings, split.Unchanged)
	log.Info("增量重审",
		"runID", runID, "unchanged", len(split.Unchanged), "changed", len(split.Changed),
		"vanished", len(split.Vanished), "carried", len(carried))

	if len(split.Changed) == 0 {
		return nil, fmt.Errorf("no staged file has changed since run %s; there is nothing to re-review", runID)
	}

	// 只把改动过的文件送审。engine.Run 内部按快照内容 hash 找历史记录，
	// 这个子集的 hash 与任何完整快照都不同，所以不会误命中基线那次的缓存。
	fresh, err := Run(ctx, i.Deps, repoAbs, branch, split.Changed, i.timeout())
	if err != nil {
		return nil, fmt.Errorf("re-review changed files: %w", err)
	}

	// 合并成一条新记录：快照是当前的完整暂存区（下次重审要拿它当基线，
	// 只存改动子集会让未变化的文件下次被误判成"新增"），意见是带过来的
	// 加上新审出来的。
	merged := store.Run{
		ID:          store.NewRunID(),
		RepoPath:    base.RepoPath,
		Status:      store.StatusCompleted,
		Snapshot:    current,
		Messages:    fresh.Messages,
		Findings:    renumber(append(carried, fresh.Findings...)),
		Summary:     fresh.Summary,
		Critiqued:   fresh.Critiqued,
		ParentRunID: base.ID,
	}
	if err := i.Deps.Store.SaveRun(ctx, merged); err != nil {
		return nil, fmt.Errorf("save re-review run: %w", err)
	}
	return &merged, nil
}

// renumber 重新分配 f1/f2/... 的 ID。
//
// 必须做：带过来的意见和新审出来的各自从 f1 开始编号，直接拼起来必然出现
// 重复 ID，而 reply 正是靠 ID 定位意见的——重复就意味着用户的回复会打在
// 另一条意见上。
func renumber(findings []review.Finding) []review.Finding {
	out := make([]review.Finding, len(findings))
	for i, f := range findings {
		f.ID = fmt.Sprintf("f%d", i+1)
		out[i] = f
	}
	return out
}

// indexOfFinding 返回 id 对应意见的下标，找不到时返回 -1。
func indexOfFinding(findings []review.Finding, id string) int {
	for i, f := range findings {
		if f.ID == id {
			return i
		}
	}
	return -1
}

// splitRunKey 把 runKey 拆回仓库路径与分支，是 buildRunKey 的逆操作。
// 用 LastIndex 而不是 Index：仓库路径本身可能含 '#'，分隔符是最后那个。
// 找不到分隔符时整串当仓库路径、分支留空——阶段一之前的旧记录存的是裸路径。
func splitRunKey(key string) (repoAbs, branch string) {
	idx := strings.LastIndex(key, "#")
	if idx < 0 {
		return key, ""
	}
	return key[:idx], key[idx+1:]
}

func (i *Interactive) timeout() time.Duration {
	if i.Timeout <= 0 {
		return defaultInteractiveTimeout
	}
	return i.Timeout
}

func (i *Interactive) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, i.timeout())
}

func (i *Interactive) loadStaged(ctx context.Context, repoDir string) ([]gitdiff.Change, error) {
	if i.LoadStaged != nil {
		return i.LoadStaged(ctx, repoDir)
	}
	return gitdiff.LoadStaged(ctx, repoDir)
}

// defaultInteractiveTimeout 只在调用方没有填 Timeout 时兜底。
const defaultInteractiveTimeout = 10 * time.Minute

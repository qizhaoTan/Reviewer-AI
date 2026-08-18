package review

import "github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"

// SnapshotDiff 是新旧两份暂存区快照的比较结果，用于决定增量重审要送哪些文件
// 给模型。三组都是对 new 的划分（Vanished 除外），合起来正好等于 new 的全集。
type SnapshotDiff struct {
	// Unchanged 是 patch 文本与上次审查时逐字节相同的文件。它们的审查意见
	// 可以原样复用，不必再花一次模型调用。
	Unchanged []gitdiff.Change

	// Changed 是需要重新审查的文件：patch 变了，或者上次审查时根本不存在。
	Changed []gitdiff.Change

	// Vanished 是上次审查过、这次却不在暂存区里的文件路径（已提交、被
	// unstage、或者被删掉）。它们不属于 Unchanged 也不属于 Changed——那两组
	// 装的都是 new 里的元素，而这些文件在 new 里没有对应项。
	//
	// 单独列出来是因为调用方必须处理它：这些文件的旧意见不能跟着 Unchanged
	// 一起被复用。针对一个已经不在暂存区里的文件继续报意见，用户完全无从下手。
	Vanished []string
}

// DiffSnapshot 比较上次审查的快照 old 与当前快照 new，划分出可复用与需重审的文件。
//
// 判据是 patch 文本逐字节相等，而不是文件内容或修改时间。理由：审查的对象本来
// 就是 patch——同一份 patch 意味着模型这次会看到与上次完全相同的输入，那么上次
// 的结论就依然成立。反过来，文件内容变了但 patch 恰好没变（例如改完又改回去）
// 时，也确实不需要重审。
//
// Status 不参与比较：git 生成的 patch 头部已经带上了 new file / deleted file 这类
// 信息，status 变了 patch 必然跟着变，再比一次是多余的。
//
// 三个返回组的顺序都跟随 new（Vanished 跟随 old），调用方拿到的文件顺序因此是
// 稳定的，不受 map 遍历顺序影响。
//
// old 为空（第一次审查）时全部归入 Changed，这正是期望的行为：没有任何历史结论
// 可以复用。
func DiffSnapshot(old, new []gitdiff.Change) SnapshotDiff {
	oldByPath := make(map[string]gitdiff.Change, len(old))
	for _, c := range old {
		oldByPath[c.Path] = c
	}

	out := SnapshotDiff{}
	seen := make(map[string]bool, len(new))
	for _, c := range new {
		seen[c.Path] = true
		if prev, ok := oldByPath[c.Path]; ok && prev.Patch == c.Patch {
			out.Unchanged = append(out.Unchanged, c)
			continue
		}
		out.Changed = append(out.Changed, c)
	}

	for _, c := range old {
		if !seen[c.Path] {
			out.Vanished = append(out.Vanished, c.Path)
		}
	}
	return out
}

// CarryOverFindings 从上一轮的意见里挑出可以原样带进新一轮的那些：属于
// unchanged 文件、且没有被用户说服撤回的。
//
// 被复核丢弃的意见（Kept=false）同样带过来，原因和当初落盘保留它们一样——
// 新一轮的记录应当是一份完整的历史，而不是只剩结论。展示层自会用
// ActiveFindings 把它们挡在外面。
//
// 撤回的意见则**不**带过来：用户已经论证过它不成立，代码又没变，再让它出现
// 一次就是无视用户的输入——这是交互功能最不该犯的错。
func CarryOverFindings(findings []Finding, unchanged []gitdiff.Change) []Finding {
	unchangedPaths := make(map[string]bool, len(unchanged))
	for _, c := range unchanged {
		unchangedPaths[c.Path] = true
	}

	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		if !unchangedPaths[f.File] || f.Status.IsWithdrawn() {
			continue
		}
		out = append(out, f)
	}
	return out
}

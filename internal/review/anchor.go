package review

import (
	"strings"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
)

// ResolveAnchors 用确定性算法把每条 Finding 的 Anchor（模型逐字引用的代码片段）
// 贴回对应文件的 diff，算出真实行号，填进 StartLine/EndLine。
//
// 为什么不让模型直接给行号：模型看到的是 unified diff，行号藏在 "@@ -12,7 +12,9 @@"
// 里，要得出某一行在新文件中的位置，模型必须从 hunk 起点逐行累加、跳过删除行——
// 这是纯计数任务，正是大模型最不可靠的地方，而且算错了看起来毫无异样。反过来，
// "逐字复制一段它正在评论的代码"是模型很擅长的事，把计数交给这里的确定性算法，
// 各自做各自擅长的部分。
//
// 定位失败不是错误：Anchor 匹配不上时 StartLine/EndLine 保持 0，该 Finding 退化为
// 文件级意见照常保留。一条意见的价值在于它指出的问题，不在于坐标——因为定位不到
// 就丢掉整条意见是本末倒置。
//
// 返回新切片，不修改入参。
func ResolveAnchors(findings []Finding, changes []gitdiff.Change) []Finding {
	if len(findings) == 0 {
		return findings
	}

	patchByPath := make(map[string]string, len(changes))
	for _, c := range changes {
		patchByPath[c.Path] = c.Patch
	}

	out := make([]Finding, len(findings))
	copy(out, findings)
	for i := range out {
		if out[i].Anchor == "" {
			continue
		}
		patch, ok := patchByPath[out[i].File]
		if !ok {
			continue
		}
		if start, end, found := resolveAnchor(patch, out[i].Anchor); found {
			out[i].StartLine = start
			out[i].EndLine = end
		}
	}
	return out
}

// resolveAnchor 在 patch 中定位 anchor，返回新文件坐标系下的起止行号。
//
// 先在新文件一侧（上下文行 + 新增行）找，找不到再退到旧文件一侧（上下文行 +
// 删除行）。绝大多数意见针对的是改动后的代码，落在新侧；但"你不该删掉这段"
// 这类意见只能在旧侧找到，此时返回的是旧文件的行号——聊胜于无，
// 至少能让人定位到大致位置。
func resolveAnchor(patch, anchor string) (startLine, endLine int, found bool) {
	target := splitAndNormalize(anchor)
	if len(target) == 0 {
		return 0, 0, false
	}
	hunks := gitdiff.ParseHunks(patch)
	if len(hunks) == 0 {
		return 0, 0, false
	}

	for _, newSide := range []bool{true, false} {
		for i := range hunks {
			side := extractSideLines(&hunks[i], newSide)
			if start, end, ok := matchConsecutive(side, target); ok {
				return start, end, true
			}
		}
	}
	return 0, 0, false
}

// indexedLine 把一行归一化后的内容和它的绝对行号绑在一起。
type indexedLine struct {
	lineNum int
	content string
}

// extractSideLines 抽取 hunk 的一侧。
// newSide 为 true 时返回"上下文行 + 新增行"并带新文件行号；
// 为 false 时返回"上下文行 + 删除行"并带旧文件行号。
//
// 关键点在于两个计数器要各自独立推进：新增行只推进 newLine，删除行只推进
// oldLine，上下文行两个都推进——这正是模型自己数不对的地方。
func extractSideLines(hunk *gitdiff.Hunk, newSide bool) []indexedLine {
	result := make([]indexedLine, 0, len(hunk.Lines))
	oldLine, newLine := hunk.OldStart, hunk.NewStart

	for _, l := range hunk.Lines {
		switch l.Type {
		case gitdiff.HunkContext:
			if newSide {
				result = append(result, indexedLine{newLine, normalizeLine(l.Content)})
			} else {
				result = append(result, indexedLine{oldLine, normalizeLine(l.Content)})
			}
			oldLine++
			newLine++
		case gitdiff.HunkAdded:
			if newSide {
				result = append(result, indexedLine{newLine, normalizeLine(l.Content)})
			}
			newLine++
		case gitdiff.HunkDeleted:
			if !newSide {
				result = append(result, indexedLine{oldLine, normalizeLine(l.Content)})
			}
			oldLine++
		}
	}
	return result
}

// matchConsecutive 在 sideLines 里滑动窗口查找与 target 完全一致的连续片段，
// 返回首个匹配的起止行号。
//
// 匹配到多处时取第一处而不是报错：anchor 是多行片段，多行连续完全相同的概率
// 很低；即使真撞上了，给出第一处也比没有行号有用。
func matchConsecutive(sideLines []indexedLine, target []string) (startLine, endLine int, found bool) {
	if len(target) == 0 || len(sideLines) < len(target) {
		return 0, 0, false
	}
	for i := 0; i <= len(sideLines)-len(target); i++ {
		matched := true
		for j, want := range target {
			if sideLines[i+j].content != want {
				matched = false
				break
			}
		}
		if matched {
			return sideLines[i].lineNum, sideLines[i+len(target)-1].lineNum, true
		}
	}
	return 0, 0, false
}

// splitAndNormalize 把 anchor 按行切开并逐行归一化，丢弃空行。
//
// 丢弃空行意味着"连续"指的是相邻的非空行——模型复制代码时容易多带或吞掉空行，
// 对空行敏感会让大量本该匹配上的 anchor 失败。
func splitAndNormalize(code string) []string {
	raw := strings.Split(code, "\n")
	result := make([]string, 0, len(raw))
	for _, line := range raw {
		if n := normalizeLine(line); n != "" {
			result = append(result, n)
		}
	}
	return result
}

// normalizeLine 去掉首尾空白，并剥掉行首残留的 +/- diff 标记。
//
// 剥标记这一步很实用：模型经常直接从 diff 里连着 "+" 号一起复制代码当 anchor，
// 不剥的话这类 anchor 一律匹配不上。忽略缩进则让匹配对空格/tab 差异免疫。
func normalizeLine(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "+")
	s = strings.TrimPrefix(s, "-")
	return strings.TrimSpace(s)
}

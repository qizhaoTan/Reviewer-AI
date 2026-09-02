package review

import (
	"strings"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
)

// FileReader 按仓库相对路径取一个文件的完整内容，取不到时返回 ok=false。
//
// 这是 ResolveAnchors 的二级定位数据源，做成函数类型而不是让 review 包直接读
// 磁盘，是为了把两件事留在调用方：一是 IO 与错误处理，二是**判断这个文件现在
// 能不能读**。后者没有普适答案——`-base` 模式要求工作区干净，文件随便读；
// 暂存区模式下工作区可能领先于索引，读到的就不是 diff 描述的那个版本。这个
// 策略差异属于命令行入口，不该渗进 review 包。
type FileReader func(path string) ([]byte, bool)

// ResolveAnchors 用确定性算法把每条 Finding 的 Anchor（模型逐字引用的代码片段）
// 贴回源码，算出真实行号，填进 StartLine/EndLine。
//
// 为什么不让模型直接给行号：模型看到的是 unified diff，行号藏在 "@@ -12,7 +12,9 @@"
// 里，要得出某一行在新文件中的位置，模型必须从 hunk 起点逐行累加、跳过删除行——
// 这是纯计数任务，正是大模型最不可靠的地方，而且算错了看起来毫无异样。反过来，
// "逐字复制一段它正在评论的代码"是模型很擅长的事，把计数交给这里的确定性算法，
// 各自做各自擅长的部分。
//
// 定位分两级：先在 diff 里找，找不到再用 readFile 读整个文件找。
//
// 之所以需要第二级：hunk 只带前后各 3 行上下文，而模型常常是先用 read_file 读到
// 完整函数、再从那里抄 anchor 的，抄进来的行很容易落在这 3 行之外——那些行在
// diff 里压根不存在，改多少匹配算法都救不回来。读全文顺带解决了 anchor 横跨多个
// hunk 的情况：文件里没有 hunk 边界这回事。
//
// readFile 为 nil 表示不启用第二级，此时行为与只在 diff 内定位完全一致。
//
// 定位失败不是错误：两级都匹配不上时 StartLine/EndLine 保持 0，该 Finding 退化为
// 文件级意见照常保留。一条意见的价值在于它指出的问题，不在于坐标——因为定位不到
// 就丢掉整条意见是本末倒置。
//
// 返回新切片，不修改入参。
func ResolveAnchors(findings []Finding, changes []gitdiff.Change, readFile FileReader) []Finding {
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
			continue
		}
		if readFile == nil {
			continue
		}
		content, ok := readFile(out[i].File)
		if !ok {
			continue
		}
		if start, end, found := resolveAnchorInFile(string(content), out[i].Anchor); found {
			out[i].StartLine = start
			out[i].EndLine = end
		}
	}
	return out
}

// resolveAnchorInFile 在文件全文里定位 anchor，返回 1-based 起止行号。
//
// 和 resolveAnchor 共用同一套归一化与滑窗逻辑，差别只在数据来源：这里没有
// hunk、没有新旧两侧，就是文件从第 1 行铺到最后一行。
func resolveAnchorInFile(content, anchor string) (startLine, endLine int, found bool) {
	target := splitAndNormalize(anchor)
	if len(target) == 0 {
		return 0, 0, false
	}

	raw := strings.Split(content, "\n")
	lines := make([]indexedLine, 0, len(raw))
	for i, l := range raw {
		// 空行同样不进匹配序列，但行号照常推进——理由见 extractSideLines。
		if n := normalizeLine(l); n != "" {
			lines = append(lines, indexedLine{i + 1, n})
		}
	}
	return matchConsecutive(lines, target)
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
//
// 空行不进入结果，与 splitAndNormalize 丢弃 anchor 空行保持对称：两侧的序列
// 形状必须一致，否则滑动窗口一撞上代码里的空行就断，anchor 只要含一个空行
// 就永远匹配不上。但行号计数器照常推进——空行在文件里仍然占一行，跳过计数
// 会让它之后的所有行号偏移。
func extractSideLines(hunk *gitdiff.Hunk, newSide bool) []indexedLine {
	result := make([]indexedLine, 0, len(hunk.Lines))
	oldLine, newLine := hunk.OldStart, hunk.NewStart

	// 收拢成一个闭包，让"过滤空行"只存在于一处：三个 case 各写一遍的话，
	// 将来加分支很容易漏掉其中一个，而漏掉的症状（某类 anchor 静默失配）
	// 恰恰是最难发现的。
	keep := func(lineNum int, content string) {
		if n := normalizeLine(content); n != "" {
			result = append(result, indexedLine{lineNum, n})
		}
	}

	for _, l := range hunk.Lines {
		switch l.Type {
		case gitdiff.HunkContext:
			if newSide {
				keep(newLine, l.Content)
			} else {
				keep(oldLine, l.Content)
			}
			oldLine++
			newLine++
		case gitdiff.HunkAdded:
			if newSide {
				keep(newLine, l.Content)
			}
			newLine++
		case gitdiff.HunkDeleted:
			if !newSide {
				keep(oldLine, l.Content)
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

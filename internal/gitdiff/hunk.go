package gitdiff

import (
	"regexp"
	"strconv"
	"strings"
)

// HunkLineType 是 unified diff 中一行的类型。
type HunkLineType int

const (
	HunkContext HunkLineType = iota // ' ' 前缀：未改动的上下文行
	HunkAdded                       // '+' 前缀：新增行
	HunkDeleted                     // '-' 前缀：删除行
)

// HunkLine 是 hunk 内的一行，Content 已经剥掉了行首的 +/-/空格 标记。
type HunkLine struct {
	Type    HunkLineType
	Content string
}

// Hunk 对应 unified diff 里一个 @@ ... @@ 块。
type Hunk struct {
	OldStart int // 旧文件中的起始行号（1-based）
	OldCount int
	NewStart int // 新文件中的起始行号（1-based）
	NewCount int
	Lines    []HunkLine
}

// hunkHeaderRe 匹配 "@@ -12,7 +12,9 @@" 形式的 hunk 头。
// 行数部分可省略（@@ -1 +1 @@ 表示单行），所以两个计数都是可选捕获组。
var hunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// ParseHunks 解析单个文件的 unified diff 文本。
// 第一个 @@ 之前的文件级头部（diff --git、---、+++）会被忽略。
//
// 这是纯文本解析，不执行 git 也不读文件——放在 gitdiff 包里是因为它属于
// "确定性地理解 diff"这一职责，与大模型无关。
func ParseHunks(rawDiffText string) []Hunk {
	lines := strings.Split(rawDiffText, "\n")
	var hunks []Hunk
	var current *Hunk

	flush := func() {
		if current != nil {
			hunks = append(hunks, *current)
			current = nil
		}
	}

	for _, line := range lines {
		if m := hunkHeaderRe.FindStringSubmatch(line); m != nil {
			flush()
			oldStart, _ := strconv.Atoi(m[1])
			oldCount := 1
			if m[2] != "" {
				oldCount, _ = strconv.Atoi(m[2])
			}
			newStart, _ := strconv.Atoi(m[3])
			newCount := 1
			if m[4] != "" {
				newCount, _ = strconv.Atoi(m[4])
			}
			current = &Hunk{OldStart: oldStart, OldCount: oldCount, NewStart: newStart, NewCount: newCount}
			continue
		}

		if current == nil {
			continue // 还没进入任何 hunk，跳过文件级头部
		}

		// "\ No newline at end of file" 是元信息而非内容行，计入会让后续行号错位。
		if strings.HasPrefix(line, `\ No newline at end of file`) {
			continue
		}
		// 一份 Change.Patch 理论上只含一个文件，但真遇到多文件拼接时就此打住，
		// 避免把下一个文件的行算到当前文件的行号上。
		if strings.HasPrefix(line, "diff --git ") {
			break
		}

		switch {
		case strings.HasPrefix(line, "+"):
			current.Lines = append(current.Lines, HunkLine{Type: HunkAdded, Content: line[1:]})
		case strings.HasPrefix(line, "-"):
			current.Lines = append(current.Lines, HunkLine{Type: HunkDeleted, Content: line[1:]})
		default:
			content := strings.TrimPrefix(line, " ")
			current.Lines = append(current.Lines, HunkLine{Type: HunkContext, Content: content})
		}
	}
	flush()

	return hunks
}

package review

import (
	"fmt"
	"sort"
	"strings"
)

// Render 把审查结果渲染成给人看的终端文本。
//
// 只渲染最终有效的意见（见 Report.ActiveFindings）——被复核判定为过度解读的、
// 以及用户提出异议后模型同意撤回的，都仍然留在数据库里备查，但不该打扰用户。
//
// 排序规则：先按严重程度从重到轻，同级再按文件路径、行号排——用户最该先看到的
// 是 error，而同一个文件里的多条意见挨在一起读起来才连贯。
func Render(r Report) string {
	var b strings.Builder

	// ActiveFindings 保证返回新切片，所以下面就地排序不会打乱调用方持有的 Report。
	findings := r.ActiveFindings()
	sortFindings(findings)

	if len(findings) == 0 {
		b.WriteString("No issues found.\n")
	} else {
		fmt.Fprintf(&b, "%s\n\n", pluralizeFindings(len(findings)))
		for _, f := range findings {
			writeFinding(&b, f)
		}
	}

	if r.Summary != "" {
		b.WriteString("Summary\n")
		b.WriteString(indent(r.Summary, "  "))
		b.WriteString("\n")
	}
	return b.String()
}

// writeFinding 渲染单条意见，形如：
//
//	[ERROR] internal/store/store.go:88-92  (f1)
//	  SaveRun 未处理 ctx 取消
//	  长时间写入时无法中断，建议……
func writeFinding(b *strings.Builder, f Finding) {
	fmt.Fprintf(b, "[%s] %s%s  (%s)\n", strings.ToUpper(string(f.Severity)), f.File, formatLines(f), f.ID)
	fmt.Fprintf(b, "%s\n", indent(f.Summary, "  "))
	if f.Detail != "" {
		fmt.Fprintf(b, "%s\n", indent(f.Detail, "  "))
	}
	b.WriteString("\n")
}

// formatLines 把起止行号渲染成 ":88"、":88-92" 或空串（文件级意见）。
func formatLines(f Finding) string {
	switch {
	case f.StartLine <= 0:
		return ""
	case f.EndLine > f.StartLine:
		return fmt.Sprintf(":%d-%d", f.StartLine, f.EndLine)
	default:
		return fmt.Sprintf(":%d", f.StartLine)
	}
}

// sortFindings 就地排序：severity 降序，其次 file、StartLine、ID 升序。
// 末位用 ID 兜底是为了让输出稳定——同文件同行的两条意见不应该每次跑出来顺序都不同。
func sortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		a, c := findings[i], findings[j]
		if a.Severity.Rank() != c.Severity.Rank() {
			return a.Severity.Rank() > c.Severity.Rank()
		}
		if a.File != c.File {
			return a.File < c.File
		}
		if a.StartLine != c.StartLine {
			return a.StartLine < c.StartLine
		}
		return a.ID < c.ID
	})
}

// pluralizeFindings 生成 "1 finding:" / "3 findings:" 这样的标题。
func pluralizeFindings(n int) string {
	if n == 1 {
		return "1 finding:"
	}
	return fmt.Sprintf("%d findings:", n)
}

// indent 给每一行加上前缀，空行保持为空——给空行补空格会在终端里留下
// 看不见但会被复制走的尾随空白。
func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}

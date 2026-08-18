// Package review 定义结构化审查结果的数据模型。
//
// 这一层是"模型输出"和"程序处理"之间的契约：模型通过 submit_review 工具按
// Report 的形状提交意见，之后的复核、持久化、终端渲染、Web 展示都只跟这里的
// 类型打交道，不再解析自由文本。
//
// review 包保持纯数据 + 纯逻辑，不导入 provider/store/tool——校验和规范化是
// 无副作用的纯函数，方便单独测试，也避免与调用方形成循环依赖。
package review

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

// Severity 是一条审查意见的严重程度。取值刻意只有三档：档位再多，模型的
// 判断反而会飘忽不定，人也说不清 "major" 和 "critical" 的界线在哪。
type Severity string

const (
	// SeverityInfo 是可改可不改的建议（命名、风格、可读性）。
	SeverityInfo Severity = "info"
	// SeverityWarning 是有隐患但不一定出错的问题（边界情况、性能、可维护性）。
	SeverityWarning Severity = "warning"
	// SeverityError 是确定的缺陷（逻辑错误、空指针、资源泄漏、安全问题）。
	SeverityError Severity = "error"
)

// severityRank 给出终端/Web 展示时的排序权重，数值越大越严重。
// 单独用一个 map 而不是靠字符串比较，是因为 "error" < "info" < "warning"
// 的字典序跟严重程度完全不相关。
var severityRank = map[Severity]int{
	SeverityInfo:    1,
	SeverityWarning: 2,
	SeverityError:   3,
}

// Rank 返回该严重程度的排序权重，未知值返回 0（排在最后）。
func (s Severity) Rank() int { return severityRank[s] }

// Valid 报告 s 是否是三个合法取值之一。
func (s Severity) Valid() bool {
	_, ok := severityRank[s]
	return ok
}

// AllSeverities 按从轻到重返回全部合法取值，供工具的 JSON Schema 枚举
// 和错误信息复用——避免在多处硬编码这三个字符串，改档位时只需改一处。
func AllSeverities() []Severity {
	return []Severity{SeverityInfo, SeverityWarning, SeverityError}
}

// Finding 是一条具体的审查意见。
//
// 一条 Finding 就是后续交互的最小单位：阶段四里用户针对某条 Finding 回复、
// 模型判断是否撤回，都是以 ID 为锚点的，所以 ID 必须在一次运行内稳定且唯一。
type Finding struct {
	// ID 在一次运行内唯一（f1/f2/...），由 NormalizeReport 统一分配，
	// 不信任模型自己填的值——模型很容易给出重复 ID 或者干脆不填。
	ID string `json:"id"`

	// File 是仓库相对路径，必须是本次 diff 涉及的文件之一。
	File string `json:"file"`

	// Anchor 是模型从 diff 里逐字复制的、这条意见所针对的代码片段（可多行）。
	// 模型只负责"引用它在评论哪段代码"，行号由 ResolveAnchors 用确定性算法
	// 贴回 diff 算出——模型不擅长数行号，但很擅长复制它正在看的内容。
	Anchor string `json:"anchor,omitempty"`

	// StartLine/EndLine 是 Anchor 定位出的 1-based 起止行号，由 ResolveAnchors 填写，
	// 不接受模型直接提供。两者同为 0 表示未能定位（anchor 为空或没匹配上），
	// 此时该意见按文件级处理。
	StartLine int `json:"start_line,omitempty"`
	EndLine   int `json:"end_line,omitempty"`

	Severity Severity `json:"severity"`

	// Summary 是一句话概括，终端列表里只显示这一行。
	Summary string `json:"summary"`

	// Detail 是详细说明与修改建议，可以为空。
	Detail string `json:"detail,omitempty"`

	// Kept 记录这条意见是否通过了复核。复核未通过的意见同样会落盘（Kept=false），
	// 只是不进入最终展示——保留下来是为了以后能回答"复核到底起了多大作用、
	// 砍掉的都是些什么"，这对调优提示词很有价值。
	//
	// 复核尚未执行时该字段为 false，此时不代表"被丢弃"，调用方应以
	// Report.Critiqued 判断复核是否已经跑过。
	Kept bool `json:"kept"`

	// CritiqueReason 是复核给出的保留/丢弃理由，复核未执行时为空。
	CritiqueReason string `json:"critique_reason,omitempty"`

	// Status 是这条意见在用户交互中的状态。零值（空串）等同于 StatusOpen——
	// 阶段四之前落盘的记录读出来就是空串，不该因为多了一个字段就全变成
	// "状态未知"。取值判断一律走 Status.IsWithdrawn / Report.ActiveFindings，
	// 不要直接跟 StatusOpen 做等值比较。
	Status FindingStatus `json:"status,omitempty"`

	// Discussion 是围绕这条意见的追加对话：用户的 reply 与模型的回应，
	// 按时间顺序追加。用 schema.Message 而不是自定义的 {角色,内容} 二元组，
	// 是因为模型在 reply 循环里同样可以调用只读工具去查证——自定义类型会把
	// ToolCalls 丢掉，那段"模型为了回答你的质疑去读了哪些代码"的记录就没了，
	// 而这恰恰是判断它的回应可不可信的依据。
	Discussion []schema.Message `json:"discussion,omitempty"`
}

// FindingStatus 记录一条意见在用户交互中的状态。
//
// 它与 Kept 是两套彼此独立的判据，刻意不合并：Kept 归复核管（模型自我批判的
// 结论），Status 归交互管（用户介入后的结论）。合成一个字段的话，用户撤回一条
// 意见和复核砍掉一条意见就再也分不开了，而这两件事对调优提示词的意义完全不同。
type FindingStatus string

const (
	// StatusOpen 是意见的初始状态：已提出、尚未被撤回。
	StatusOpen FindingStatus = "open"
	// StatusWithdrawn 表示用户提出异议后，模型认同其理由并撤回了这条意见。
	StatusWithdrawn FindingStatus = "withdrawn"
)

// IsWithdrawn 报告这条意见是否已被撤回。
//
// 用方法而不是 `s == StatusWithdrawn` 的直接比较，是为了把"空串等同于 open"
// 这条兼容约定收在一处：阶段四之前的记录、以及任何没显式设过 Status 的
// Finding，读出来都是空串。
func (s FindingStatus) IsWithdrawn() bool { return s == StatusWithdrawn }

// Report 是一次审查的完整结构化结果。
type Report struct {
	// Findings 包含本次审查产出的全部意见。复核跑过之后，这里既有 Kept=true
	// 的也有 Kept=false 的；要拿最终结果用 KeptFindings。
	Findings []Finding `json:"findings"`

	// Summary 是对本次改动的整体评价。即使 Findings 为空也应该有内容
	// （比如"改动很小且直接，没有发现问题"），否则用户会怀疑是不是跑挂了。
	Summary string `json:"summary"`

	// Critiqued 标记复核阶段是否已经执行过，用于区分"复核后被丢弃"和
	// "复核还没跑，Kept 只是零值"这两种含义完全不同的 Kept=false。
	Critiqued bool `json:"critiqued"`
}

// KeptFindings 返回通过复核的意见。复核尚未执行（Critiqued 为 false）时
// 返回全部意见——此时没有任何依据可以丢弃它们。
//
// 返回的一律是新切片，调用方可以安全地就地排序而不影响这份 Report。
func (r Report) KeptFindings() []Finding {
	if !r.Critiqued {
		return append([]Finding(nil), r.Findings...)
	}
	kept := make([]Finding, 0, len(r.Findings))
	for _, f := range r.Findings {
		if f.Kept {
			kept = append(kept, f)
		}
	}
	return kept
}

// ActiveFindings 返回最终应当呈现给用户的意见：既通过了复核，又没有被用户
// 的异议说服撤回。这是展示层唯一该用的入口（终端渲染、Web 的"保留"区块）。
//
// 单开一个方法而不是让各处调用方自己拼 `Kept && !Status.IsWithdrawn()`：
// 判据一旦散落在多处，日后再加一种状态就必然漏改其中一处，而漏改的表现是
// "某条意见在终端不见了但网页上还在"这类极难察觉的不一致。
//
// 与 KeptFindings 的关系：KeptFindings 只回答"复核放行了哪些"，是复核阶段的
// 中间结论，Web 详情页靠它把意见分成保留/丢弃两组；ActiveFindings 是叠加了
// 用户交互之后的最终结论。
//
// 返回的一律是新切片，调用方可以安全地就地排序。
func (r Report) ActiveFindings() []Finding {
	kept := r.KeptFindings()
	active := make([]Finding, 0, len(kept))
	for _, f := range kept {
		if !f.Status.IsWithdrawn() {
			active = append(active, f)
		}
	}
	return active
}

// NormalizeReport 校验并规范化模型提交的 Report：逐条检查必填字段、
// 重新分配 ID、清理首尾空白。
//
// 校验失败时返回的 error 会原样作为工具调用的错误结果回喂给模型，所以错误信息
// 必须同时说明"错在哪"和"该怎么改"（沿用 internal/tool 里的既有约定）——
// 含糊的错误信息会让模型多试错一轮。
//
// ID 一律由这里重新分配而不是沿用模型填的值：模型在一次提交里给出重复 ID
// 是常见现象，而 ID 一旦重复，阶段四的 reply 就会锚定到错误的意见上。
func NormalizeReport(r Report) (Report, error) {
	out := Report{
		Summary:  strings.TrimSpace(r.Summary),
		Findings: make([]Finding, 0, len(r.Findings)),
	}
	if out.Summary == "" {
		return Report{}, fmt.Errorf("summary 为空；请用一段话给出对这批改动的整体评价，即使一条意见都没有也要写")
	}

	for i, f := range r.Findings {
		normalized, err := normalizeFinding(f, i)
		if err != nil {
			return Report{}, err
		}
		out.Findings = append(out.Findings, normalized)
	}
	return out, nil
}

// normalizeFinding 校验并规范化单条意见，index 是它在提交列表里的下标，
// 仅用于生成错误信息里的定位提示（模型看到 "findings[2]" 就知道改哪一条）。
func normalizeFinding(f Finding, index int) (Finding, error) {
	file := strings.TrimSpace(f.File)
	if file == "" {
		return Finding{}, fmt.Errorf("findings[%d].file 为空；请填上这条意见所针对文件的仓库相对路径（必须是本次暂存区改动中的某个文件）", index)
	}
	summary := strings.TrimSpace(f.Summary)
	if summary == "" {
		return Finding{}, fmt.Errorf("findings[%d].summary 为空；请用一句话说明问题，更长的解释放到 detail 里", index)
	}
	if !f.Severity.Valid() {
		return Finding{}, fmt.Errorf("findings[%d].severity 是 %q；请使用 %s 之一", index, f.Severity, severityList())
	}
	return Finding{
		ID:       findingID(index),
		File:     file,
		Anchor:   strings.Trim(f.Anchor, "\n"),
		Severity: f.Severity,
		Summary:  summary,
		Detail:   strings.TrimSpace(f.Detail),
	}, nil
}

// findingID 按下标生成 f1/f2/... 形式的 ID。
func findingID(index int) string {
	return "f" + strconv.Itoa(index+1)
}

// severityList 把合法档位渲染成 `"info", "warning", "error"` 供错误信息使用。
func severityList() string {
	quoted := make([]string, 0, len(AllSeverities()))
	for _, s := range AllSeverities() {
		quoted = append(quoted, strconv.Quote(string(s)))
	}
	return strings.Join(quoted, ", ")
}

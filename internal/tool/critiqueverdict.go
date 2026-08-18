package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

// CritiqueVerdictName 是复核裁决工具的名字。
const CritiqueVerdictName = "submit_verdict"

// Verdict 是复核者对一条审查意见的裁决。
type Verdict struct {
	// Keep 为 true 表示这条意见有依据、应该呈现给用户。
	Keep bool `json:"keep"`
	// Reason 是做出该裁决的理由，保留和丢弃都要写——丢弃的理由会随
	// Finding 一起落盘，用于回答"复核到底砍掉了什么、为什么"。
	Reason string `json:"reason"`
}

// CritiqueVerdictTool 让复核者以结构化形式给出保留/丢弃的裁决。
//
// 和 submit_review 一样，它的存在是为了约束输出格式：复核的结论必须是一个
// 明确的布尔值加一段理由，而不是一段需要程序去猜测语气的自由文本。
//
// 复核只做减法不做加法——这个工具没有"修改意见内容"或"新增意见"的入口，
// 是刻意的：复核阶段引入初审没看到的新臆测，比漏掉一条意见更糟。
//
// 无状态，裁决通过 Result.CritiqueVerdict 返回。
type CritiqueVerdictTool struct{}

func (CritiqueVerdictTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name: CritiqueVerdictName,
		Description: "Submit your verdict on the review comment under examination. Call this exactly once, when you have finished checking whether the comment holds up. " +
			"Keep the comment only if it identifies a real problem that the author should act on; drop it if it is speculative, already handled elsewhere in the code, or not worth the author's attention.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"keep": map[string]interface{}{
					"type":        "boolean",
					"description": "true to keep the comment, false to drop it.",
				},
				"reason": map[string]interface{}{
					"type":        "string",
					"description": "Why you reached this verdict, in one or two sentences. Required for both keeping and dropping.",
				},
			},
			"required": []interface{}{"keep", "reason"},
		},
	}
}

func (CritiqueVerdictTool) Execute(_ context.Context, _ string, args json.RawMessage) Result {
	var v Verdict
	if err := json.Unmarshal(args, &v); err != nil {
		return Result{
			Output: fmt.Sprintf("%s 的参数不是合法的 JSON（%v）。请用类似 "+
				`{"keep": false, "reason": "..."}`+" 的对象重新调用。", CritiqueVerdictName, err),
			IsError: true,
		}
	}
	v.Reason = strings.TrimSpace(v.Reason)
	if v.Reason == "" {
		return Result{
			Output: fmt.Sprintf("%s：reason 为空；请用一两句话说明你为什么%s这条意见。",
				CritiqueVerdictName, keepOrDropWord(v.Keep)),
			IsError: true,
		}
	}

	return Result{
		Output:          fmt.Sprintf("裁决已记录：%s该条意见。", keepOrDropWord(v.Keep)),
		CritiqueVerdict: &v,
	}
}

func keepOrDropWord(keep bool) string {
	if keep {
		return "保留"
	}
	return "丢弃"
}

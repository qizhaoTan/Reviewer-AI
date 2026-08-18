package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

// WithdrawFindingName 是撤回审查意见的工具名。
const WithdrawFindingName = "withdraw_finding"

// Withdrawal 是模型认同用户的异议、决定撤回一条审查意见的记录。
type Withdrawal struct {
	// Reason 是撤回的理由：用户的哪一点说服了它。写下来是为了事后能回答
	// "这条意见当初为什么被撤掉"——尤其是当同一个问题后来真的出事的时候。
	Reason string `json:"reason"`
}

// WithdrawFindingTool 让模型在被用户说服后正式撤回一条审查意见。
//
// 为什么撤回要走工具而不是识别自由文本：模型说"你说得有道理"跟它真的认为
// 这条意见不成立，是两回事——它在措辞上极容易顺着用户走。要求它调用一个
// 工具，等于要求它做一个明确的、二值的决定，而不是给一段可以两头解释的回话。
//
// 反过来，坚持意见**不需要**调用任何工具：模型只要正常把理由说出来即可。
// 这个不对称是刻意的——把"撤回"设成需要额外动作的那一边，模型就不会因为
// 想尽快结束对话而顺手撤掉一条本来站得住的意见。
//
// 工具不接收 finding id：一次 reply 对话自始至终只围绕一条意见，是哪一条由
// 调用方（engine.Reply）决定并记录。让模型填 id 只会多出一种它填错的可能，
// 而填错的后果是撤销了另一条意见——一个静默且严重的错误。
//
// 无状态，撤回决定通过 Result.Withdrawal 返回。
type WithdrawFindingTool struct{}

func (WithdrawFindingTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name: WithdrawFindingName,
		Description: "Withdraw the review comment under discussion, because the author's objection has convinced you it does not hold up. " +
			"Call this only when you genuinely agree the comment was wrong or not worth raising. " +
			"If you still believe the comment is valid, do not call this tool — just explain why you disagree, in plain text.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"reason": map[string]interface{}{
					"type":        "string",
					"description": "Which part of the author's objection convinced you, in one or two sentences.",
				},
			},
			"required": []interface{}{"reason"},
		},
	}
}

func (WithdrawFindingTool) Execute(_ context.Context, _ string, args json.RawMessage) Result {
	var w Withdrawal
	if err := json.Unmarshal(args, &w); err != nil {
		return Result{
			Output: fmt.Sprintf("%s 的参数不是合法的 JSON（%v）。请用类似 "+
				`{"reason": "..."}`+" 的对象重新调用。", WithdrawFindingName, err),
			IsError: true,
		}
	}
	w.Reason = strings.TrimSpace(w.Reason)
	if w.Reason == "" {
		return Result{
			Output: fmt.Sprintf("%s：reason 为空；请用一两句话说明作者异议中的哪一点说服了你。"+
				"如果并没有任何一点说服你，就根本不该调用这个工具——直接用纯文本说明你为什么不认同。",
				WithdrawFindingName),
			IsError: true,
		}
	}

	return Result{
		Output:     "该条意见已撤回。",
		Withdrawal: &w,
	}
}

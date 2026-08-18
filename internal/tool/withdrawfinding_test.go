package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestWithdrawFindingExecute(t *testing.T) {
	tests := []struct {
		name string
		args string
		// wantErrContains 非空时期望调用失败，且 Output 含这些片段。
		wantErrContains []string
		wantReason      string
	}{
		{
			name:       "records a withdrawal",
			args:       `{"reason":"the caller already guarantees non-nil"}`,
			wantReason: "the caller already guarantees non-nil",
		},
		{
			name:       "trims whitespace around the reason",
			args:       `{"reason":"  spaced out  "}`,
			wantReason: "spaced out",
		},
		{
			// 空理由要挡住，并且错误信息必须提醒模型"不认同就别调这个工具"——
			// 否则它会随手填一句废话把工具调完，把一次"我坚持"扭曲成"我撤回"。
			name:            "rejects an empty reason and points at the alternative",
			args:            `{"reason":"   "}`,
			wantErrContains: []string{"reason 为空", "就根本不该调用这个工具"},
		},
		{
			name:            "rejects a missing reason",
			args:            `{}`,
			wantErrContains: []string{"reason 为空"},
		},
		{
			name:            "rejects malformed json",
			args:            `{"reason": "unterminated`,
			wantErrContains: []string{"不是合法的 JSON", `"reason"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WithdrawFindingTool{}.Execute(context.Background(), "/repo", json.RawMessage(tt.args))

			if len(tt.wantErrContains) > 0 {
				if !got.IsError {
					t.Fatalf("Execute() IsError = false, want an error; Output = %q", got.Output)
				}
				for _, want := range tt.wantErrContains {
					if !strings.Contains(got.Output, want) {
						t.Errorf("Output %q does not contain %q", got.Output, want)
					}
				}
				// 一次被拒绝的调用绝不能返回撤回决定：调用方靠 Withdrawal != nil
				// 判断意见是否撤回，这里漏一个非 nil 就是静默撤销一条有效意见。
				if got.Withdrawal != nil {
					t.Error("a rejected withdrawal must not be returned")
				}
				return
			}

			if got.IsError {
				t.Fatalf("Execute() IsError = true, want success; Output = %q", got.Output)
			}
			if got.Withdrawal == nil {
				t.Fatal("Withdrawal is nil, want the withdrawal to be returned")
			}
			if got.Withdrawal.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", got.Withdrawal.Reason, tt.wantReason)
			}
		})
	}
}

// TestWithdrawFindingDefinition 固定住工具描述里几条不能丢的措辞。
//
// 这些句子是模型判断"该不该调这个工具"的唯一依据：少了"坚持时不要调用"
// 这条，模型会把每一次 reply 都当成必须调工具收尾（submit_review /
// submit_verdict 都是那个模式），于是任何异议都能撤掉一条意见。
func TestWithdrawFindingDefinition(t *testing.T) {
	tests := []struct {
		name         string
		wantContains []string
	}{
		{
			name: "description tells the model not to call it when disagreeing",
			wantContains: []string{
				"do not call this tool",
				"only when you genuinely agree",
			},
		},
	}

	def := WithdrawFindingTool{}.Definition()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if def.Name != WithdrawFindingName {
				t.Errorf("Definition().Name = %q, want %q", def.Name, WithdrawFindingName)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(def.Description, want) {
					t.Errorf("Description does not contain %q; got:\n%s", want, def.Description)
				}
			}
		})
	}
}

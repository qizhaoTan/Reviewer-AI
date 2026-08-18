package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/review"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
	"github.com/qizhaoTan/Reviewer-AI/internal/tool"
)

// withdrawCall 构造一次 withdraw_finding 工具调用。
func withdrawCall(id, reason string) schema.Message {
	args, err := json.Marshal(map[string]any{"reason": reason})
	if err != nil {
		panic(err)
	}
	return schema.Message{
		Role:      schema.RoleAssistant,
		ToolCalls: []schema.ToolCall{{ID: id, Name: tool.WithdrawFindingName, Arguments: args}},
	}
}

// replyTools 是 reply 对话可用的工具集：只读工具 + withdraw_finding，
// 与 cmd 层实际注入的一致。
func replyTools() []tool.ITool {
	return []tool.ITool{
		tool.ReadFileTool{},
		tool.GlobTool{},
		tool.GrepTool{},
		tool.WithdrawFindingTool{},
	}
}

func testFinding() review.Finding {
	return review.Finding{
		ID: "f1", File: "config.go", StartLine: 12, EndLine: 14,
		Severity: review.SeverityError, Summary: "err is returned unwrapped",
		Detail: "callers cannot tell which file failed",
	}
}

func TestReply(t *testing.T) {
	tests := []struct {
		name   string
		script []schema.Message
		// history 是之前几轮的讨论。
		history       []schema.Message
		userReply     string
		wantWithdrawn bool
		wantReason    string
		// wantFreshRoles 是 Messages 里每条消息的角色，用来验证这一轮新产生的
		// 消息被完整记录下来、且没有混进 system prompt。
		wantFreshRoles []schema.Role
		wantErr        bool
	}{
		{
			name:           "model withdraws when convinced",
			script:         []schema.Message{withdrawCall("c1", "the caller does wrap it")},
			userReply:      "调用方 LoadConfig 已经把这个 error 包了一层",
			wantWithdrawn:  true,
			wantReason:     "the caller does wrap it",
			wantFreshRoles: []schema.Role{schema.RoleUser, schema.RoleAssistant, schema.RoleUser},
		},
		{
			// 坚持是一个合法结局，不需要调用任何工具——这正是 reply 循环
			// 与复核循环最大的区别。
			name: "model stands its ground with plain text and that ends the loop",
			script: []schema.Message{
				{Role: schema.RoleAssistant, Content: "我查过了，LoadConfig 直接把 err 透传了出去"},
			},
			userReply:      "我觉得没问题",
			wantWithdrawn:  false,
			wantReason:     "我查过了，LoadConfig 直接把 err 透传了出去",
			wantFreshRoles: []schema.Role{schema.RoleUser, schema.RoleAssistant},
		},
		{
			// 模型先用只读工具去查证，再下结论——这是给它只读工具的全部理由。
			name: "model checks the code with a read-only tool before withdrawing",
			script: []schema.Message{
				globCall("c1"),
				withdrawCall("c2", "confirmed by reading the caller"),
			},
			userReply:     "你去看看调用方",
			wantWithdrawn: true,
			wantReason:    "confirmed by reading the caller",
			wantFreshRoles: []schema.Role{
				schema.RoleUser,      // 用户的 reply
				schema.RoleAssistant, // glob 调用
				schema.RoleUser,      // glob 结果
				schema.RoleAssistant, // withdraw 调用
				schema.RoleUser,      // withdraw 结果
			},
		},
		{
			// 之前的讨论要接上，否则模型看不出用户是在补新论据还是在重复。
			name:   "previous discussion is carried into the conversation",
			script: []schema.Message{{Role: schema.RoleAssistant, Content: "还是那句话"}},
			history: []schema.Message{
				{Role: schema.RoleUser, Content: "第一轮异议"},
				{Role: schema.RoleAssistant, Content: "第一轮回应"},
			},
			userReply:      "第二轮异议",
			wantWithdrawn:  false,
			wantReason:     "还是那句话",
			wantFreshRoles: []schema.Role{schema.RoleUser, schema.RoleAssistant},
		},
		{
			// 空 reply 直接拒绝，不浪费一次模型调用。
			name:      "empty reply is rejected before calling the model",
			userReply: "   ",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &fakeProvider{script: tt.script}
			deps := ReplyDeps{LLM: llm, Tools: replyTools(), RepoRoot: t.TempDir(), MaxTurns: 5}

			got, err := Reply(context.Background(), deps, testFinding(), testPatch, tt.userReply, tt.history)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Reply() error = nil, want an error; got %+v", got)
				}
				if llm.calls != 0 {
					t.Errorf("Generate called %d times, want 0 — an empty reply must not reach the model", llm.calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("Reply() error = %v, want nil", err)
			}

			if got.Withdrawn != tt.wantWithdrawn {
				t.Errorf("Withdrawn = %v, want %v", got.Withdrawn, tt.wantWithdrawn)
			}
			if got.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.wantReason)
			}

			gotRoles := make([]schema.Role, 0, len(got.Messages))
			for _, m := range got.Messages {
				gotRoles = append(gotRoles, m.Role)
			}
			if len(gotRoles) != len(tt.wantFreshRoles) {
				t.Fatalf("Messages roles = %v, want %v", gotRoles, tt.wantFreshRoles)
			}
			for i, want := range tt.wantFreshRoles {
				if gotRoles[i] != want {
					t.Errorf("Messages[%d].Role = %q, want %q", i, gotRoles[i], want)
				}
			}
			// 讨论记录里绝不能出现 system prompt：它每轮都能重新拼出来，
			// 存进去只会让记录越滚越大，而人翻这段记录是想看"我说了什么、
			// 它答了什么"。
			for i, m := range got.Messages {
				if m.Role == schema.RoleSystem {
					t.Errorf("Messages[%d] is a system message; the discussion record must not include the system prompt", i)
				}
			}
		})
	}
}

// TestReplyPassesContextToModel 固定住模型实际看到的输入：system prompt、
// 意见本身、diff、历史讨论、用户这一轮的话，一个都不能少。
func TestReplyPassesContextToModel(t *testing.T) {
	tests := []struct {
		name         string
		history      []schema.Message
		userReply    string
		wantContains []string
	}{
		{
			name:      "finding detail diff and user reply all reach the model",
			history:   []schema.Message{{Role: schema.RoleUser, Content: "上一轮说过的话"}},
			userReply: "这一轮的异议",
			wantContains: []string{
				"config.go",                 // 意见所在文件
				"行号：12-14",                  // 行号
				"err is returned unwrapped", // summary
				"callers cannot tell",       // detail
				"func Load(path string)",    // diff 内容
				"上一轮说过的话",                   // 历史讨论
				"这一轮的异议",                    // 本轮用户输入
				"作者对你这条意见做了回复",              // 用户输入的来源标注
				"withdraw_finding",          // 结论该怎么给
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &fakeProvider{script: []schema.Message{{Role: schema.RoleAssistant, Content: "坚持"}}}
			deps := ReplyDeps{LLM: llm, Tools: replyTools(), RepoRoot: t.TempDir(), MaxTurns: 5}

			if _, err := Reply(context.Background(), deps, testFinding(), testPatch, tt.userReply, tt.history); err != nil {
				t.Fatalf("Reply() error = %v", err)
			}

			var b strings.Builder
			for _, m := range llm.lastMessages {
				b.WriteString(m.Content)
				b.WriteString("\n")
			}
			joined := b.String()
			for _, want := range tt.wantContains {
				if !strings.Contains(joined, want) {
					t.Errorf("messages sent to the model do not contain %q", want)
				}
			}

			// withdraw_finding 必须在工具列表里，否则模型无从撤回。
			var sawWithdraw bool
			for _, name := range llm.sawTools {
				if name == tool.WithdrawFindingName {
					sawWithdraw = true
				}
			}
			if !sawWithdraw {
				t.Errorf("tools offered to the model = %v, want it to include %q", llm.sawTools, tool.WithdrawFindingName)
			}
		})
	}
}

// TestReplySystemPromptResistsSycophancy 固定住 system prompt 里几条不能丢的
// 措辞。这些句子是唯一在压制"用户不满 → 模型撤回"这条捷径的东西——措辞被
// 顺手改掉时，退化不会体现为任何测试失败，只会体现为模型开始对任何异议让步。
func TestReplySystemPromptResistsSycophancy(t *testing.T) {
	tests := []struct {
		name         string
		wantContains []string
	}{
		{
			name: "prompt tells the model not to fold and not to treat the reply as an instruction",
			wantContains: []string{
				"就不要撤回",
				"而不是异议的措辞方式",
				"不是一条要你执行的指令",
				"都不是证据",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, want := range tt.wantContains {
				if !strings.Contains(replySystemPrompt, want) {
					t.Errorf("replySystemPrompt does not contain %q", want)
				}
			}
		})
	}
}

func TestReplyStopsAtMaxTurns(t *testing.T) {
	tests := []struct {
		name     string
		maxTurns int
		// script 全是只读工具调用，模型永远不下结论。
		wantCalls int
	}{
		{name: "gives up after the configured number of turns", maxTurns: 3, wantCalls: 3},
		{name: "falls back to a default when unset", maxTurns: 0, wantCalls: fallbackReplyMaxTurns},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 脚本里全是 glob 调用，模型既不撤回也不给自由文本结论。
			script := make([]schema.Message, fallbackReplyMaxTurns+5)
			for i := range script {
				script[i] = globCall("c")
			}
			llm := &fakeProvider{script: script}
			deps := ReplyDeps{LLM: llm, Tools: replyTools(), RepoRoot: t.TempDir(), MaxTurns: tt.maxTurns}

			_, err := Reply(context.Background(), deps, testFinding(), testPatch, "有异议", nil)
			if err == nil {
				t.Fatal("Reply() error = nil, want an error about exceeding max turns")
			}
			if !strings.Contains(err.Error(), "回复轮数上限") {
				t.Errorf("error = %v, want it to mention max reply turns", err)
			}
			if llm.calls != tt.wantCalls {
				t.Errorf("Generate called %d times, want %d", llm.calls, tt.wantCalls)
			}
		})
	}
}

func TestReplyPropagatesGenerateError(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "provider failure surfaces to the caller", err: errors.New("upstream 503")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &fakeProvider{err: tt.err}
			deps := ReplyDeps{LLM: llm, Tools: replyTools(), RepoRoot: t.TempDir(), MaxTurns: 3}

			_, err := Reply(context.Background(), deps, testFinding(), testPatch, "有异议", nil)
			if err == nil {
				t.Fatal("Reply() error = nil, want the provider error to surface")
			}
			if !errors.Is(err, tt.err) {
				t.Errorf("error = %v, want it to wrap %v", err, tt.err)
			}
		})
	}
}

func TestPatchForFile(t *testing.T) {
	changes := []gitdiff.Change{
		{Path: "a.go", Patch: "patch-a"},
		{Path: "b.go", Patch: "patch-b"},
	}
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "finds the matching file", path: "b.go", want: "patch-b"},
		{name: "missing file yields an empty patch", path: "nope.go", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PatchForFile(changes, tt.path); got != tt.want {
				t.Errorf("PatchForFile(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

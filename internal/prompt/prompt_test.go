package prompt

import (
	"strings"
	"testing"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

// TestSystemPromptRequiresSubmitReview 固定住 system prompt 里几条不能丢的指令。
// 这些措辞看着像"文案"，但每一条丢掉都会让流程实际跑不通：模型会退回自由文本
// 作答（engine 收不到结果，白跑一轮）、或者自己去猜行号（anchor 机制失效）、
// 或者在没有问题时干脆不作声（engine 分不清"审查通过"和"模型跑飞了"）。
func TestSystemPromptRequiresSubmitReview(t *testing.T) {
	tests := []struct {
		name     string
		contains string
		why      string
	}{
		{
			name:     "names the submit_review tool",
			contains: "submit_review",
			why:      "模型需要知道该调用哪个工具收尾",
		},
		{
			name:     "forbids answering with plain text",
			contains: "不要把结论写成纯文本",
			why:      "否则模型会用 Markdown 作答，engine 拿不到结构化结果",
		},
		{
			name:     "requires an explicit call even when the changeset is fine",
			contains: "findings 传空数组",
			why:      "空审查必须是一次显式提交，否则无法与'模型跑飞了'区分",
		},
		{
			name:     "tells the model not to compute line numbers",
			contains: "不要自己去推算行号",
			why:      "行号由 anchor 反推，模型自己数会数错且错得看不出来",
		},
		{
			name:     "tells the model to copy exact source lines as the anchor",
			contains: "anchor 字段",
			why:      "anchor 是定位的唯一输入",
		},
		{
			name:     "tells the model not to repeat a search it already ran",
			contains: "已经跑过的搜索绝不要重复跑",
			why:      "实测中同一个符号被 grep 了 6 次，历史结果明明还在上下文里",
		},
		{
			name:     "tells the model to weigh each call against the verdict",
			contains: "有可能改变我的结论吗",
			why:      "给'还要不要再查一次'一个判断锚点，否则模型只会一路往下查",
		},
		{
			name:     "frames finding nothing as a successful review",
			contains: "什么问题都没找到同样是一次成功的审查",
			why:      "meticulous reviewer 的人设让交白卷像失职，得显式许可它收工",
		},
		{
			name:     "forbids purely forward-looking suggestions",
			contains: "只是建议将来重构",
			why:      "这类噪音意见让作者无事可做，从源头掐掉比事后让复核砍更省一轮",
		},
	}

	lower := strings.ToLower(systemPrompt)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(lower, strings.ToLower(tt.contains)) {
				t.Errorf("system prompt is missing %q (%s):\n%s", tt.contains, tt.why, systemPrompt)
			}
		})
	}
}

func TestBuildInitial(t *testing.T) {
	tests := []struct {
		name           string
		changes        []gitdiff.Change
		languagePrompt string
		systemSuffix   string // 非空时断言 system prompt 以此结尾
		userContains   []string
		userExcludes   []string
	}{
		{
			name:         "no changes",
			changes:      nil,
			userContains: []string{"暂存区没有任何变更。"},
		},
		{
			name:           "language prompt is appended to the end of the system prompt",
			changes:        []gitdiff.Change{{Status: "M", Path: "a.go", Patch: "-x\n+y\n"}},
			languagePrompt: "- Your response needs to be in Chinese!!!",
			systemSuffix:   "\n\n- Your response needs to be in Chinese!!!",
			userContains:   []string{"a.go"},
		},
		{
			name:           "empty language prompt leaves the system prompt untouched",
			changes:        []gitdiff.Change{{Status: "M", Path: "a.go", Patch: "-x\n+y\n"}},
			languagePrompt: "",
			// 断言"原样传出"而不是写死末行内容：这条用例要守的是"不加语言约束时
			// 不动 system prompt"，跟末行恰好是哪句话无关。写死末行会让每次调整
			// 提示词措辞都误报一次失败，而那正是这个包设计上鼓励反复迭代的部分。
			systemSuffix: systemPrompt,
			userContains: []string{"a.go"},
		},
		{
			name: "single change",
			changes: []gitdiff.Change{
				{Status: "M", Path: "internal/foo.go", Patch: "@@ -1,1 +1,1 @@\n-old\n+new\n"},
			},
			userContains: []string{"internal/foo.go", "（状态：M）", "-old", "+new"},
		},
		{
			name: "multiple changes with different statuses",
			changes: []gitdiff.Change{
				{Status: "A", Path: "new.go", Patch: "+const added = true\n"},
				{Status: "D", Path: "old.go", Patch: "-const removed = true\n"},
				{Status: "M", Path: "changed.go", Patch: "-before\n+after\n"},
			},
			userContains: []string{
				"new.go", "（状态：A）", "+const added = true",
				"old.go", "（状态：D）", "-const removed = true",
				"changed.go", "（状态：M）", "-before", "+after",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := BuildInitial(tt.changes, tt.languagePrompt)

			if len(msgs) != 2 {
				t.Fatalf("len(msgs) = %d, want 2", len(msgs))
			}
			if msgs[0].Role != schema.RoleSystem {
				t.Errorf("msgs[0].Role = %q, want %q", msgs[0].Role, schema.RoleSystem)
			}
			if msgs[0].Content == "" {
				t.Error("msgs[0].Content is empty, want non-empty system prompt")
			}
			if tt.systemSuffix != "" && !strings.HasSuffix(msgs[0].Content, tt.systemSuffix) {
				t.Errorf("msgs[0].Content does not end with %q; got tail %q", tt.systemSuffix, tail(msgs[0].Content))
			}
			if msgs[1].Role != schema.RoleUser {
				t.Errorf("msgs[1].Role = %q, want %q", msgs[1].Role, schema.RoleUser)
			}

			for _, want := range tt.userContains {
				if !strings.Contains(msgs[1].Content, want) {
					t.Errorf("user message does not contain %q:\n%s", want, msgs[1].Content)
				}
			}
			for _, unwanted := range tt.userExcludes {
				if strings.Contains(msgs[1].Content, unwanted) {
					t.Errorf("user message contains excluded text %q:\n%s", unwanted, msgs[1].Content)
				}
			}
		})
	}
}

// tail 截取字符串末尾一小段，用于让断言失败时的输出可读——system prompt 很长，
// 整段打出来会淹没失败信息。
func tail(s string) string {
	const n = 80
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func TestWithLanguage(t *testing.T) {
	tests := []struct {
		name           string
		systemPrompt   string
		languagePrompt string
		want           string
	}{
		{
			name:           "appended after a blank line",
			systemPrompt:   "base",
			languagePrompt: "- in Chinese",
			want:           "base\n\n- in Chinese",
		},
		{
			name:           "trailing newlines on the base prompt are collapsed",
			systemPrompt:   "base\n\n\n",
			languagePrompt: "- in Chinese",
			want:           "base\n\n- in Chinese",
		},
		{
			name:           "empty language prompt returns the base unchanged",
			systemPrompt:   "base\n",
			languagePrompt: "",
			want:           "base\n",
		},
		{
			name:           "whitespace-only language prompt counts as empty",
			systemPrompt:   "base\n",
			languagePrompt: "   \n\t ",
			want:           "base\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WithLanguage(tt.systemPrompt, tt.languagePrompt); got != tt.want {
				t.Fatalf("WithLanguage() = %q, want %q", got, tt.want)
			}
		})
	}
}

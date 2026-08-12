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
			contains: "do not write your findings as plain text",
			why:      "否则模型会用 Markdown 作答，engine 拿不到结构化结果",
		},
		{
			name:     "requires an explicit call even when the changeset is fine",
			contains: "empty findings array",
			why:      "空审查必须是一次显式提交，否则无法与'模型跑飞了'区分",
		},
		{
			name:     "tells the model not to compute line numbers",
			contains: "do not try to work out line numbers yourself",
			why:      "行号由 anchor 反推，模型自己数会数错且错得看不出来",
		},
		{
			name:     "tells the model to copy exact source lines as the anchor",
			contains: "anchor field",
			why:      "anchor 是定位的唯一输入",
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
		name         string
		changes      []gitdiff.Change
		userContains []string
		userExcludes []string
	}{
		{
			name:         "no changes",
			changes:      nil,
			userContains: []string{"No staged changes."},
		},
		{
			name: "single change",
			changes: []gitdiff.Change{
				{Status: "M", Path: "internal/foo.go", Patch: "@@ -1,1 +1,1 @@\n-old\n+new\n"},
			},
			userContains: []string{"internal/foo.go", "status: M", "-old", "+new"},
		},
		{
			name: "multiple changes with different statuses",
			changes: []gitdiff.Change{
				{Status: "A", Path: "new.go", Patch: "+const added = true\n"},
				{Status: "D", Path: "old.go", Patch: "-const removed = true\n"},
				{Status: "M", Path: "changed.go", Patch: "-before\n+after\n"},
			},
			userContains: []string{
				"new.go", "status: A", "+const added = true",
				"old.go", "status: D", "-const removed = true",
				"changed.go", "status: M", "-before", "+after",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := BuildInitial(tt.changes)

			if len(msgs) != 2 {
				t.Fatalf("len(msgs) = %d, want 2", len(msgs))
			}
			if msgs[0].Role != schema.RoleSystem {
				t.Errorf("msgs[0].Role = %q, want %q", msgs[0].Role, schema.RoleSystem)
			}
			if msgs[0].Content == "" {
				t.Error("msgs[0].Content is empty, want non-empty system prompt")
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

package prompt

import (
	"strings"
	"testing"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

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

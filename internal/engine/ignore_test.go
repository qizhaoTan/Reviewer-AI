package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/prompt"
	"github.com/qizhaoTan/Reviewer-AI/internal/store"
)

func TestResumeIgnorePolicy(t *testing.T) {
	tests := []struct {
		name         string
		old, current []string
		reuse        bool
	}{
		{"same", []string{"*.log"}, []string{"*.log"}, true},
		{"added", nil, []string{"*.log"}, false},
		{"removed", []string{"*.log"}, nil, false},
		{"changed", []string{"*.log"}, []string{"*.tmp"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, err := store.New(filepath.Join(t.TempDir(), "runs.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()
			snapshot := []gitdiff.Change{{Status: "M", Path: "keep.go", Patch: "patch"}}
			old, err := resumeOrStartRun(ctx, db, "repo#main", "", snapshot, "", false, tt.old...)
			if err != nil {
				t.Fatal(err)
			}
			old.Status = store.StatusCompleted
			if err := db.SaveRun(ctx, *old); err != nil {
				t.Fatal(err)
			}
			current, err := resumeOrStartRun(ctx, db, "repo#main", "", snapshot, "", false, tt.current...)
			if err != nil {
				t.Fatal(err)
			}
			if (current.ID == old.ID) != tt.reuse {
				t.Fatalf("reuse=%v want %v", current.ID == old.ID, tt.reuse)
			}
			if !prompt.MatchesIgnorePolicy(current.Messages, tt.current) {
				t.Fatal("new policy missing")
			}
			if !tt.reuse {
				inter := Interactive{Deps: Deps{Store: db, Ignore: tt.current}}
				if _, err := inter.Reply(ctx, old.ID, "f1", "check"); err == nil || !strings.Contains(err.Error(), "ignore rules have changed") {
					t.Fatalf("unexpected reply error: %v", err)
				}
			}
		})
	}
}

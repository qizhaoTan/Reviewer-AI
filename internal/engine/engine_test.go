package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
	"github.com/qizhaoTan/Reviewer-AI/internal/store"
)

func TestBuildRunKey(t *testing.T) {
	tests := []struct {
		name            string
		repoAbs, branch string
		want            string
	}{
		{name: "joins repo path and branch with a separator", repoAbs: "/repo/a", branch: "main", want: "/repo/a#main"},
		{name: "different branches produce different keys", repoAbs: "/repo/a", branch: "feature-x", want: "/repo/a#feature-x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildRunKey(tt.repoAbs, tt.branch); got != tt.want {
				t.Errorf("buildRunKey(%q, %q) = %q, want %q", tt.repoAbs, tt.branch, got, tt.want)
			}
		})
	}
}

func TestResumeOrStartRun(t *testing.T) {
	snapshotA := []gitdiff.Change{{Status: "M", Path: "main.go", Patch: "@@ a @@"}}
	snapshotB := []gitdiff.Change{{Status: "M", Path: "main.go", Patch: "@@ b @@"}}

	tests := []struct {
		name string
		// seed 在测试开始前预置的记录；nil 表示不预置任何记录。
		seed *store.Run
		// changes 是本次"新采集到"的暂存区快照。
		changes []gitdiff.Change
		// wantSeedReused 为 true 时期望拿到 seed 本身（同一个 ID）；
		// 为 false 时期望拿到一条全新的、Messages 由 prompt.BuildInitial 生成的记录。
		wantSeedReused bool
		// wantStatus 是期望返回记录的状态。
		wantStatus store.RunStatus
	}{
		{
			name:           "no prior run starts fresh",
			seed:           nil,
			changes:        snapshotA,
			wantSeedReused: false,
			wantStatus:     store.StatusInProgress,
		},
		{
			name: "in_progress run with matching snapshot resumes",
			seed: &store.Run{
				ID:       "seed-run",
				Status:   store.StatusInProgress,
				Snapshot: snapshotA,
				Messages: []schema.Message{
					{Role: schema.RoleSystem, Content: "system prompt"},
					{Role: schema.RoleUser, Content: "diff for main.go"},
					{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "tc1", Name: "grep"}}},
					{Role: schema.RoleUser, Content: "grep: no matches", ToolCallID: "tc1"},
				},
			},
			changes:        snapshotA,
			wantSeedReused: true,
			wantStatus:     store.StatusInProgress,
		},
		{
			name: "in_progress run with different snapshot starts fresh",
			seed: &store.Run{
				ID:       "seed-run",
				Status:   store.StatusInProgress,
				Snapshot: snapshotA,
			},
			changes:        snapshotB,
			wantSeedReused: false,
			wantStatus:     store.StatusInProgress,
		},
		{
			name: "completed run with matching snapshot is reused as-is",
			seed: &store.Run{
				ID:       "seed-run",
				Status:   store.StatusCompleted,
				Snapshot: snapshotA,
				Messages: []schema.Message{
					{Role: schema.RoleAssistant, Content: "final review report"},
				},
			},
			changes:        snapshotA,
			wantSeedReused: true,
			wantStatus:     store.StatusCompleted,
		},
		{
			name: "failed run with matching snapshot is not reused",
			seed: &store.Run{
				ID:       "seed-run",
				Status:   store.StatusFailed,
				Snapshot: snapshotA,
			},
			changes:        snapshotA,
			wantSeedReused: false,
			wantStatus:     store.StatusInProgress,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dsn := "file:" + filepath.Join(t.TempDir(), "test.db")
			db, err := store.New(dsn)
			if err != nil {
				t.Fatalf("store.New: %v", err)
			}
			defer db.Close()

			ctx := context.Background()
			const runKey = "/repo/a#main"

			if tt.seed != nil {
				seed := *tt.seed
				seed.RepoPath = runKey
				if err := db.SaveRun(ctx, seed); err != nil {
					t.Fatalf("seed SaveRun: %v", err)
				}
			}

			got, err := resumeOrStartRun(ctx, db, runKey, tt.changes, "")
			if err != nil {
				t.Fatalf("resumeOrStartRun: %v", err)
			}

			if tt.wantSeedReused {
				if got.ID != tt.seed.ID {
					t.Errorf("ID = %q, want reused seed run ID %q", got.ID, tt.seed.ID)
				}
			} else {
				if tt.seed != nil && got.ID == tt.seed.ID {
					t.Errorf("ID = %q, want a fresh run, not the stale seed run", got.ID)
				}
				if len(got.Messages) == 0 {
					t.Errorf("fresh run has no messages, want prompt.BuildInitial output")
				}
			}
			if got.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tt.wantStatus)
			}
		})
	}
}

// TestResumeOrStartRun_StashRoundTrip 模拟 Tan 提出的场景：同一分支上先审查内容 A
// （跑完 completed），git stash 后改成内容 B，再 stash pop 回内容 A。第二次面对内容 A
// 时，即使 A 已经不是"最新一条记录"（B 才是），也应该命中 A 的旧结果，而不是拿 B
// 的记录去比较，也不是重新发起一次全新审查。
func TestResumeOrStartRun_StashRoundTrip(t *testing.T) {
	snapshotA := []gitdiff.Change{{Status: "M", Path: "main.go", Patch: "@@ change A @@"}}
	snapshotB := []gitdiff.Change{{Status: "M", Path: "main.go", Patch: "@@ change B @@"}}

	dsn := "file:" + filepath.Join(t.TempDir(), "test.db")
	db, err := store.New(dsn)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	const runKey = "/repo/a#main"

	// 1. 审查 A，跑完。
	runA, err := resumeOrStartRun(ctx, db, runKey, snapshotA, "")
	if err != nil {
		t.Fatalf("resumeOrStartRun(A): %v", err)
	}
	runA.Status = store.StatusCompleted
	runA.Messages = []schema.Message{{Role: schema.RoleAssistant, Content: "review of A"}}
	if err := db.SaveRun(ctx, *runA); err != nil {
		t.Fatalf("complete run A: %v", err)
	}

	// 2. git stash：暂存区变成 B，审查 B（成为"最新"记录）。
	runB, err := resumeOrStartRun(ctx, db, runKey, snapshotB, "")
	if err != nil {
		t.Fatalf("resumeOrStartRun(B): %v", err)
	}
	if runB.ID == runA.ID {
		t.Fatalf("run B got the same ID as run A, want a fresh run")
	}

	// 3. git stash pop：暂存区变回 A。即使 B 现在是"最新"记录，也应该找到 A 的旧结果。
	gotA, err := resumeOrStartRun(ctx, db, runKey, snapshotA, "")
	if err != nil {
		t.Fatalf("resumeOrStartRun(A again): %v", err)
	}
	if gotA.ID != runA.ID {
		t.Errorf("resuming content A got run %q, want the original run %q", gotA.ID, runA.ID)
	}
	if gotA.Status != store.StatusCompleted {
		t.Errorf("resuming content A got status %q, want %q (reused, not re-reviewed)", gotA.Status, store.StatusCompleted)
	}
}

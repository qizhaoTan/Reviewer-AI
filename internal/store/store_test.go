package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db")
	s, err := New(dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestNewMigratesLegacyTableWithoutSnapshotHash 模拟打开一个在 snapshot_hash 列
// 引入之前创建的旧数据库：先手工建一张没有该列的 runs 表，再调用 New 验证它能
// 补上这一列并且后续的 SaveRun/LoadRunByHash 正常工作，而不是报 "no such column"。
func TestNewMigratesLegacyTableWithoutSnapshotHash(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "legacy.db")

	legacyDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE runs (
			id         TEXT PRIMARY KEY,
			repo_path  TEXT    NOT NULL,
			status     TEXT    NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			snapshot   TEXT    NOT NULL,
			messages   TEXT    NOT NULL
		)
	`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := New(dsn)
	if err != nil {
		t.Fatalf("New on legacy db: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	changes := []gitdiff.Change{{Status: "M", Path: "main.go", Patch: "@@ a @@"}}
	run := Run{ID: "run-1", RepoPath: "/repo/a", Status: StatusCompleted, Snapshot: changes}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun after migration: %v", err)
	}

	got, err := s.LoadRunByHash(ctx, "/repo/a", gitdiff.SnapshotHash(changes))
	if err != nil {
		t.Fatalf("LoadRunByHash after migration: %v", err)
	}
	if got == nil || got.ID != "run-1" {
		t.Errorf("LoadRunByHash after migration = %+v, want run-1", got)
	}
}

func TestSaveAndLoadRun(t *testing.T) {
	tests := []struct {
		name string
		run  Run
	}{
		{
			name: "run with snapshot and messages",
			run: Run{
				ID:       "run-1",
				RepoPath: "/repo/a",
				Status:   StatusInProgress,
				Snapshot: []gitdiff.Change{{Status: "M", Path: "main.go", Patch: "@@ ... @@"}},
				Messages: []schema.Message{
					{Role: schema.RoleSystem, Content: "system prompt"},
					{Role: schema.RoleUser, Content: "diff here"},
				},
			},
		},
		{
			name: "run with empty snapshot and messages",
			run: Run{
				ID:       "run-2",
				RepoPath: "/repo/b",
				Status:   StatusPending,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()

			if err := s.SaveRun(ctx, tt.run); err != nil {
				t.Fatalf("SaveRun: %v", err)
			}

			got, err := s.LoadRun(ctx, tt.run.ID)
			if err != nil {
				t.Fatalf("LoadRun: %v", err)
			}
			if got == nil {
				t.Fatalf("LoadRun(%s) = nil, want a record", tt.run.ID)
			}
			if got.ID != tt.run.ID || got.RepoPath != tt.run.RepoPath || got.Status != tt.run.Status {
				t.Errorf("LoadRun(%s) = %+v, want ID/RepoPath/Status to match %+v", tt.run.ID, got, tt.run)
			}
			if len(got.Snapshot) != len(tt.run.Snapshot) {
				t.Errorf("LoadRun(%s).Snapshot has %d entries, want %d", tt.run.ID, len(got.Snapshot), len(tt.run.Snapshot))
			}
			if len(got.Messages) != len(tt.run.Messages) {
				t.Errorf("LoadRun(%s).Messages has %d entries, want %d", tt.run.ID, len(got.Messages), len(tt.run.Messages))
			}
			if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
				t.Errorf("LoadRun(%s) has zero timestamps: %+v", tt.run.ID, got)
			}
		})
	}
}

func TestSaveRunUpsert(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	run := Run{ID: "run-1", RepoPath: "/repo/a", Status: StatusInProgress, Messages: []schema.Message{{Role: schema.RoleUser, Content: "first"}}}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatalf("first SaveRun: %v", err)
	}

	run.Status = StatusCompleted
	run.Messages = append(run.Messages, schema.Message{Role: schema.RoleAssistant, Content: "final answer"})
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatalf("second SaveRun: %v", err)
	}

	got, err := s.LoadRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.Status != StatusCompleted {
		t.Errorf("Status = %q, want %q (upsert should overwrite, not duplicate)", got.Status, StatusCompleted)
	}
	if len(got.Messages) != 2 {
		t.Errorf("Messages has %d entries, want 2 after upsert", len(got.Messages))
	}
}

// TestSaveRunPreservesToolResultsWhileCompactionDisabled 锁定当前行为：
// compactionEnabled=false 时 SaveRun 不改写任何消息内容。如果重新启用压缩，
// 这条测试需要跟着改回"占位符替换"的断言（TestCompactToolResults 独立覆盖了
// 压缩函数本身的逻辑，压缩逻辑正确性不受这里的开关影响）。
func TestSaveRunPreservesToolResultsWhileCompactionDisabled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	run := Run{
		ID:       "run-1",
		RepoPath: "/repo/a",
		Status:   StatusInProgress,
		Messages: []schema.Message{
			{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "tc1", Name: "read_file"}}},
			{Role: schema.RoleUser, Content: "the full file content...", ToolCallID: "tc1"},
			{Role: schema.RoleUser, Content: "plain user turn"},
		},
	}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	got, err := s.LoadRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.Messages[1].Content != "the full file content..." {
		t.Errorf("tool result content = %q, want original content preserved (compaction disabled)", got.Messages[1].Content)
	}
	if got.Messages[2].Content != "plain user turn" {
		t.Errorf("plain user message content was altered: got %q", got.Messages[2].Content)
	}
}

func TestCompactToolResults(t *testing.T) {
	tests := []struct {
		name string
		in   []schema.Message
		want []schema.Message
	}{
		{
			name: "replaces tool result content, leaves other messages untouched",
			in: []schema.Message{
				{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "tc1", Name: "read_file"}}},
				{Role: schema.RoleUser, Content: "the full file content...", ToolCallID: "tc1"},
				{Role: schema.RoleUser, Content: "plain user turn"},
			},
			want: []schema.Message{
				{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "tc1", Name: "read_file"}}},
				{Role: schema.RoleUser, Content: toolResultPlaceholder, ToolCallID: "tc1"},
				{Role: schema.RoleUser, Content: "plain user turn"},
			},
		},
		{
			name: "empty input",
			in:   nil,
			want: []schema.Message{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceToolResultContents(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d messages, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i].Content != tt.want[i].Content || got[i].ToolCallID != tt.want[i].ToolCallID {
					t.Errorf("message[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestLoadRunNotFound(t *testing.T) {
	s := newTestStore(t)
	got, err := s.LoadRun(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got != nil {
		t.Errorf("LoadRun(missing) = %+v, want nil", got)
	}
}

func TestLoadLatestRun(t *testing.T) {
	tests := []struct {
		name     string
		repoPath string
		wantID   string
		wantNil  bool
	}{
		{name: "returns most recently updated run for repo", repoPath: "/repo/a", wantID: "run-a2"},
		{name: "no runs for repo returns nil", repoPath: "/repo/none", wantNil: true},
	}

	s := newTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	seed := []Run{
		{ID: "run-a1", RepoPath: "/repo/a", Status: StatusCompleted, CreatedAt: base},
		{ID: "run-a2", RepoPath: "/repo/a", Status: StatusInProgress, CreatedAt: base.Add(time.Minute)},
		{ID: "run-b1", RepoPath: "/repo/b", Status: StatusCompleted, CreatedAt: base},
	}
	// updated_at 是纳秒精度，SaveRun 内部各调一次 time.Now() 就足以保证严格递增，
	// 不需要真实 sleep 来错开时间戳。
	for _, r := range seed {
		if err := s.SaveRun(ctx, r); err != nil {
			t.Fatalf("seed SaveRun(%s): %v", r.ID, err)
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.LoadLatestRun(ctx, tt.repoPath)
			if err != nil {
				t.Fatalf("LoadLatestRun: %v", err)
			}
			if tt.wantNil {
				if got != nil {
					t.Errorf("LoadLatestRun(%s) = %+v, want nil", tt.repoPath, got)
				}
				return
			}
			if got == nil || got.ID != tt.wantID {
				t.Errorf("LoadLatestRun(%s) = %+v, want ID %q", tt.repoPath, got, tt.wantID)
			}
		})
	}
}

func TestLoadRunByHash(t *testing.T) {
	snapshotA := []gitdiff.Change{{Status: "M", Path: "main.go", Patch: "@@ a @@"}}
	snapshotB := []gitdiff.Change{{Status: "M", Path: "main.go", Patch: "@@ b @@"}}
	hashA := gitdiff.SnapshotHash(snapshotA)
	hashB := gitdiff.SnapshotHash(snapshotB)

	tests := []struct {
		name   string
		hash   string
		wantID string
	}{
		{name: "finds a completed run by hash even though it is not the latest", hash: hashA, wantID: "run-a-old"},
		{name: "finds the in_progress run for a different hash", hash: hashB, wantID: "run-b-new"},
		{name: "unknown hash returns nil", hash: "does-not-exist", wantID: ""},
	}

	s := newTestStore(t)
	ctx := context.Background()
	// run-a-old 用快照 A 跑完，之后 run-b-new 用快照 B 成为"最新"记录——
	// LoadLatestRun 现在只会看到 run-b-new，但 LoadRunByHash(hashA) 必须仍然能
	// 找到 run-a-old，这正是它和 LoadLatestRun 的关键区别：不局限于最新一条。
	seed := []Run{
		{ID: "run-a-old", RepoPath: "/repo/a", Status: StatusCompleted, Snapshot: snapshotA},
		{ID: "run-b-new", RepoPath: "/repo/a", Status: StatusInProgress, Snapshot: snapshotB},
	}
	for _, r := range seed {
		if err := s.SaveRun(ctx, r); err != nil {
			t.Fatalf("seed SaveRun(%s): %v", r.ID, err)
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.LoadRunByHash(ctx, "/repo/a", tt.hash)
			if err != nil {
				t.Fatalf("LoadRunByHash: %v", err)
			}
			if tt.wantID == "" {
				if got != nil {
					t.Errorf("LoadRunByHash(%s) = %+v, want nil", tt.hash, got)
				}
				return
			}
			if got == nil || got.ID != tt.wantID {
				t.Errorf("LoadRunByHash(%s) = %+v, want ID %q", tt.hash, got, tt.wantID)
			}
		})
	}
}

func TestListRuns(t *testing.T) {
	tests := []struct {
		name    string
		limit   int
		wantIDs []string
	}{
		{name: "no limit returns all in descending update order", limit: 0, wantIDs: []string{"run-3", "run-2", "run-1"}},
		{name: "limit caps result count to most recent", limit: 2, wantIDs: []string{"run-3", "run-2"}},
	}

	s := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"run-1", "run-2", "run-3"} {
		if err := s.SaveRun(ctx, Run{ID: id, RepoPath: "/repo/a", Status: StatusCompleted}); err != nil {
			t.Fatalf("seed SaveRun(%s): %v", id, err)
		}
	}
	if err := s.SaveRun(ctx, Run{ID: "other-repo", RepoPath: "/repo/other", Status: StatusCompleted}); err != nil {
		t.Fatalf("seed SaveRun(other-repo): %v", err)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runs, err := s.ListRuns(ctx, "/repo/a", tt.limit)
			if err != nil {
				t.Fatalf("ListRuns: %v", err)
			}
			if len(runs) != len(tt.wantIDs) {
				t.Fatalf("ListRuns returned %d runs, want %d", len(runs), len(tt.wantIDs))
			}
			for i, id := range tt.wantIDs {
				if runs[i].ID != id {
					t.Errorf("runs[%d].ID = %q, want %q", i, runs[i].ID, id)
				}
			}
		})
	}
}

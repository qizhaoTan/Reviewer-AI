package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/review"
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
		t.Fatalf("LoadRunByHash after migration = %+v, want run-1", got)
	}

	// 旧表同样缺少 findings/summary/critiqued 三列，迁移后它们应该有可用的默认值：
	// 空意见列表、空 summary、未复核——这正是那些在结构化输出之前跑的运行的真实状态。
	if len(got.Findings) != 0 {
		t.Errorf("Findings = %+v, want empty for a migrated legacy row", got.Findings)
	}
	if got.Summary != "" {
		t.Errorf("Summary = %q, want empty for a migrated legacy row", got.Summary)
	}
	if got.Critiqued {
		t.Error("Critiqued = true, want false for a migrated legacy row")
	}

	// 迁移后写入带 Findings 的记录也必须正常工作。
	withFindings := Run{
		ID: "run-2", RepoPath: "/repo/a", Status: StatusCompleted, Snapshot: changes,
		Findings:  []review.Finding{{ID: "f1", File: "main.go", Severity: review.SeverityError, Summary: "boom", Kept: true}},
		Summary:   "one problem",
		Critiqued: true,
	}
	if err := s.SaveRun(ctx, withFindings); err != nil {
		t.Fatalf("SaveRun with findings after migration: %v", err)
	}
	reloaded, err := s.LoadRun(ctx, "run-2")
	if err != nil {
		t.Fatalf("LoadRun after migration: %v", err)
	}
	if len(reloaded.Findings) != 1 || reloaded.Findings[0].ID != "f1" {
		t.Errorf("Findings = %+v, want the saved finding", reloaded.Findings)
	}
}

func TestSaveAndLoadRunFindings(t *testing.T) {
	findings := []review.Finding{
		{ID: "f1", File: "a.go", Anchor: "return err", StartLine: 12, EndLine: 14,
			Severity: review.SeverityError, Summary: "swallowed", Detail: "wrap it",
			Kept: true, CritiqueReason: "verified against the code"},
		{ID: "f2", File: "b.go", Severity: review.SeverityInfo, Summary: "nit",
			Kept: false, CritiqueReason: "speculative"},
	}

	tests := []struct {
		name string
		run  Run
		// wantFindings 是期望读回的意见条数。
		wantFindings  int
		wantSummary   string
		wantCritiqued bool
	}{
		{
			name: "round trips findings summary and critiqued",
			run: Run{
				ID: "r1", RepoPath: "/repo/a", Status: StatusCompleted,
				Findings: findings, Summary: "two comments", Critiqued: true,
			},
			wantFindings: 2, wantSummary: "two comments", wantCritiqued: true,
		},
		{
			name: "an initial review persists before critique runs",
			run: Run{
				ID: "r2", RepoPath: "/repo/a", Status: StatusInProgress,
				Findings: findings[:1], Summary: "one comment", Critiqued: false,
			},
			wantFindings: 1, wantSummary: "one comment", wantCritiqued: false,
		},
		{
			name:         "a run with no findings reads back as an empty slice, not null",
			run:          Run{ID: "r3", RepoPath: "/repo/a", Status: StatusCompleted, Summary: "all clear", Critiqued: true},
			wantFindings: 0, wantSummary: "all clear", wantCritiqued: true,
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
				t.Fatal("LoadRun returned nil, want the saved run")
			}

			if len(got.Findings) != tt.wantFindings {
				t.Fatalf("len(Findings) = %d, want %d", len(got.Findings), tt.wantFindings)
			}
			if got.Summary != tt.wantSummary {
				t.Errorf("Summary = %q, want %q", got.Summary, tt.wantSummary)
			}
			if got.Critiqued != tt.wantCritiqued {
				t.Errorf("Critiqued = %v, want %v", got.Critiqued, tt.wantCritiqued)
			}
			// 每个字段都要原样读回——尤其是 Kept 和 CritiqueReason，被丢弃的意见
			// 之所以落盘就是为了保住这两个字段。
			for i := range got.Findings {
				if !reflect.DeepEqual(got.Findings[i], tt.run.Findings[i]) {
					t.Errorf("Findings[%d] = %+v, want %+v", i, got.Findings[i], tt.run.Findings[i])
				}
			}
		})
	}
}

func TestRunReportRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		report review.Report
	}{
		{
			name: "a critiqued report survives the round trip",
			report: review.Report{
				Findings: []review.Finding{
					{ID: "f1", File: "a.go", Severity: review.SeverityError, Summary: "boom", Kept: true, CritiqueReason: "real"},
					{ID: "f2", File: "b.go", Severity: review.SeverityInfo, Summary: "nit", Kept: false, CritiqueReason: "noise"},
				},
				Summary: "one real problem", Critiqued: true,
			},
		},
		{
			name:   "an empty report survives the round trip",
			report: review.Report{Summary: "nothing to flag", Critiqued: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var run Run
			run.SetReport(tt.report)

			got := run.Report()
			if got.Summary != tt.report.Summary || got.Critiqued != tt.report.Critiqued {
				t.Errorf("Report() = %+v, want %+v", got, tt.report)
			}
			if len(got.Findings) != len(tt.report.Findings) {
				t.Fatalf("len(Findings) = %d, want %d", len(got.Findings), len(tt.report.Findings))
			}
			for i := range got.Findings {
				if !reflect.DeepEqual(got.Findings[i], tt.report.Findings[i]) {
					t.Errorf("Findings[%d] = %+v, want %+v", i, got.Findings[i], tt.report.Findings[i])
				}
			}
		})
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

func TestListAllRuns(t *testing.T) {
	tests := []struct {
		name    string
		limit   int
		wantIDs []string
	}{
		{
			name:    "no limit returns every repo in descending update order",
			limit:   0,
			wantIDs: []string{"b-2", "a-2", "b-1", "a-1"},
		},
		{
			name:    "limit caps result count across repos",
			limit:   2,
			wantIDs: []string{"b-2", "a-2"},
		},
	}

	s := newTestStore(t)
	ctx := context.Background()
	// 交替写入两个 repoPath，这样"跨仓库"和"按更新时间倒序"必须同时成立
	// 才能得到期望顺序——只要实现漏了任一条，用例就会失败。
	for _, id := range []string{"a-1", "b-1", "a-2", "b-2"} {
		repo := "/repo/a#main"
		if strings.HasPrefix(id, "b-") {
			repo = "/repo/b#feature"
		}
		if err := s.SaveRun(ctx, Run{ID: id, RepoPath: repo, Status: StatusCompleted}); err != nil {
			t.Fatalf("seed SaveRun(%s): %v", id, err)
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runs, err := s.ListAllRuns(ctx, tt.limit)
			if err != nil {
				t.Fatalf("ListAllRuns: %v", err)
			}
			if len(runs) != len(tt.wantIDs) {
				t.Fatalf("ListAllRuns returned %d runs, want %d", len(runs), len(tt.wantIDs))
			}
			for i, id := range tt.wantIDs {
				if runs[i].ID != id {
					t.Errorf("runs[%d].ID = %q, want %q", i, runs[i].ID, id)
				}
			}
		})
	}

	// ListAllRuns 存在的理由：ListRuns 的空 repoPath 不是通配符。这条断言
	// 固定住该语义，防止有人"顺手"把 ListRuns 改成空值即全量，从而让两个
	// 方法悄悄重叠。
	t.Run("empty repoPath is not a wildcard for ListRuns", func(t *testing.T) {
		runs, err := s.ListRuns(ctx, "", 0)
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(runs) != 0 {
			t.Fatalf("ListRuns with empty repoPath returned %d runs, want 0", len(runs))
		}
	})
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

func TestSaveFindingsRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		initial  []review.Finding
		updated  []review.Finding
		wantIDs  []string
		wantStat []review.FindingStatus
	}{
		{
			name: "status and discussion survive a round trip",
			initial: []review.Finding{
				{ID: "f1", File: "a.go", Kept: true},
				{ID: "f2", File: "b.go", Kept: true},
			},
			updated: []review.Finding{
				{ID: "f1", File: "a.go", Kept: true},
				{ID: "f2", File: "b.go", Kept: true, Status: review.StatusWithdrawn,
					Discussion: []schema.Message{
						{Role: schema.RoleUser, Content: "调用方已经处理了"},
						{Role: schema.RoleAssistant, Content: "确实，撤回"},
					}},
			},
			wantIDs:  []string{"f1", "f2"},
			wantStat: []review.FindingStatus{"", review.StatusWithdrawn},
		},
		{
			name:     "an empty list is stored as an empty array not null",
			initial:  []review.Finding{{ID: "f1", File: "a.go"}},
			updated:  nil,
			wantIDs:  []string{},
			wantStat: []review.FindingStatus{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			run := Run{ID: "r1", RepoPath: "/repo#main", Status: StatusCompleted, Findings: tt.initial}
			if err := s.SaveRun(ctx, run); err != nil {
				t.Fatalf("SaveRun: %v", err)
			}

			if err := s.SaveFindings(ctx, "r1", tt.updated); err != nil {
				t.Fatalf("SaveFindings: %v", err)
			}

			got, err := s.LoadRun(ctx, "r1")
			if err != nil {
				t.Fatalf("LoadRun: %v", err)
			}
			if got == nil {
				t.Fatal("LoadRun returned nil")
			}
			if len(got.Findings) != len(tt.wantIDs) {
				t.Fatalf("len(Findings) = %d, want %d", len(got.Findings), len(tt.wantIDs))
			}
			for i := range tt.wantIDs {
				if got.Findings[i].ID != tt.wantIDs[i] {
					t.Errorf("Findings[%d].ID = %q, want %q", i, got.Findings[i].ID, tt.wantIDs[i])
				}
				if got.Findings[i].Status != tt.wantStat[i] {
					t.Errorf("Findings[%d].Status = %q, want %q", i, got.Findings[i].Status, tt.wantStat[i])
				}
			}
			// 讨论记录必须完整读回——它是判断撤回可不可信的依据。
			for _, f := range got.Findings {
				for _, want := range tt.updated {
					if f.ID == want.ID && !reflect.DeepEqual(f.Discussion, want.Discussion) {
						t.Errorf("Findings[%s].Discussion = %+v, want %+v", f.ID, f.Discussion, want.Discussion)
					}
				}
			}
		})
	}
}

// TestSaveFindingsLeavesOtherColumnsAlone 固定住 SaveFindings 的核心约定：
// 它只碰 findings 一列。写成 SaveRun 那样的整行 upsert 时，Web 侧一次撤回
// 就会用读旧了的快照覆盖掉命令行正在推进的 messages。
func TestSaveFindingsLeavesOtherColumnsAlone(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "messages status and snapshot are untouched"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			run := Run{
				ID: "r1", RepoPath: "/repo#main", Status: StatusInProgress,
				Snapshot: []gitdiff.Change{{Status: "M", Path: "a.go", Patch: "p1"}},
				Messages: []schema.Message{{Role: schema.RoleSystem, Content: "sys"}},
				Findings: []review.Finding{{ID: "f1", File: "a.go"}},
				Summary:  "original summary",
			}
			if err := s.SaveRun(ctx, run); err != nil {
				t.Fatalf("SaveRun: %v", err)
			}

			if err := s.SaveFindings(ctx, "r1", []review.Finding{
				{ID: "f1", File: "a.go", Status: review.StatusWithdrawn},
			}); err != nil {
				t.Fatalf("SaveFindings: %v", err)
			}

			got, err := s.LoadRun(ctx, "r1")
			if err != nil {
				t.Fatalf("LoadRun: %v", err)
			}
			if got.Status != StatusInProgress {
				t.Errorf("Status = %q, want it untouched (%q)", got.Status, StatusInProgress)
			}
			if len(got.Messages) != 1 || got.Messages[0].Content != "sys" {
				t.Errorf("Messages = %+v, want them untouched", got.Messages)
			}
			if !reflect.DeepEqual(got.Snapshot, run.Snapshot) {
				t.Errorf("Snapshot = %+v, want it untouched", got.Snapshot)
			}
			if got.Summary != "original summary" {
				t.Errorf("Summary = %q, want it untouched", got.Summary)
			}
			if got.Findings[0].Status != review.StatusWithdrawn {
				t.Errorf("Findings[0].Status = %q, want the update to have landed", got.Findings[0].Status)
			}
		})
	}
}

func TestSaveFindingsRejectsUnknownRun(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		// 静默成功会让调用方以为撤回已经落盘，而实际上什么都没发生。
		{name: "a missing run is an error not a silent no-op", id: "does-not-exist"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			err := s.SaveFindings(context.Background(), tt.id, []review.Finding{{ID: "f1"}})
			if err == nil {
				t.Fatal("SaveFindings() error = nil, want an error about the run not existing")
			}
			if !strings.Contains(err.Error(), "does not exist") {
				t.Errorf("error = %v, want it to say the run does not exist", err)
			}
		})
	}
}

func TestParentRunIDRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		parent string
	}{
		{name: "a re-review links back to its baseline", parent: "run-parent"},
		{name: "a first review has no parent", parent: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			run := Run{ID: "r1", RepoPath: "/repo#main", Status: StatusCompleted, ParentRunID: tt.parent}
			if err := s.SaveRun(ctx, run); err != nil {
				t.Fatalf("SaveRun: %v", err)
			}
			got, err := s.LoadRun(ctx, "r1")
			if err != nil {
				t.Fatalf("LoadRun: %v", err)
			}
			if got.ParentRunID != tt.parent {
				t.Errorf("ParentRunID = %q, want %q", got.ParentRunID, tt.parent)
			}
		})
	}
}

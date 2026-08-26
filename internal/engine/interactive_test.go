package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/review"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
	"github.com/qizhaoTan/Reviewer-AI/internal/store"
)

// TestSplitRunKeyInvertsBuildRunKey 固定住两个函数互为逆操作。
// 它们分别写在 engine.go 和 interactive.go 里，改了一个忘了另一个的话，
// 表现是 reply 拿着一个错的仓库路径去执行只读工具——工具会报"文件不存在"，
// 而真正的原因在完全不相干的地方。
func TestSplitRunKeyInvertsBuildRunKey(t *testing.T) {
	tests := []struct {
		name   string
		repo   string
		branch string
	}{
		{name: "plain path and branch", repo: "/home/tan/proj", branch: "main"},
		{name: "branch containing a slash", repo: "/home/tan/proj", branch: "feature/x"},
		{name: "repo path containing a hash", repo: "/home/tan/pr#42", branch: "main"},
		{name: "detached head short hash", repo: "/home/tan/proj", branch: "a1b2c3d"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRepo, gotBranch := splitRunKey(buildRunKey(tt.repo, tt.branch))
			if gotRepo != tt.repo || gotBranch != tt.branch {
				t.Errorf("splitRunKey(buildRunKey(%q, %q)) = (%q, %q), want (%q, %q)",
					tt.repo, tt.branch, gotRepo, gotBranch, tt.repo, tt.branch)
			}
		})
	}
}

func TestRenumber(t *testing.T) {
	tests := []struct {
		name    string
		in      []review.Finding
		wantIDs []string
	}{
		{
			// 带过来的和新审出来的各自从 f1 开始，直接拼起来就是重复 ID，
			// 而 reply 靠 ID 定位意见——重复意味着回复打在另一条上。
			name:    "duplicate ids from merging two sources are resolved",
			in:      []review.Finding{{ID: "f1"}, {ID: "f2"}, {ID: "f1"}, {ID: "f2"}},
			wantIDs: []string{"f1", "f2", "f3", "f4"},
		},
		{name: "empty input", in: nil, wantIDs: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renumber(tt.in)
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.wantIDs))
			}
			for i, want := range tt.wantIDs {
				if got[i].ID != want {
					t.Errorf("[%d].ID = %q, want %q", i, got[i].ID, want)
				}
			}
		})
	}
}

func TestIndexOfFinding(t *testing.T) {
	findings := []review.Finding{{ID: "f1"}, {ID: "f2"}}
	tests := []struct {
		name string
		id   string
		want int
	}{
		{name: "found", id: "f2", want: 1},
		{name: "missing", id: "f9", want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := indexOfFinding(findings, tt.id); got != tt.want {
				t.Errorf("indexOfFinding(%q) = %d, want %d", tt.id, got, tt.want)
			}
		})
	}
}

// newInteractive 装配一个用假 provider 驱动的 Interactive，并预置一条运行记录。
func newInteractive(t *testing.T, llm schema.IProvider, base store.Run) *Interactive {
	t.Helper()
	deps, db := newTestDeps(t, llm, base.Snapshot)
	if err := db.SaveRun(context.Background(), base); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	return &Interactive{Deps: deps, ReplyTools: replyTools(), ReplyMaxTurns: 5}
}

func baseRun(repoAbs string) store.Run {
	return store.Run{
		ID: "run-base", RepoPath: buildRunKey(repoAbs, "main"), Status: store.StatusCompleted,
		Snapshot:  []gitdiff.Change{{Status: "M", Path: "config.go", Patch: testPatch}},
		Critiqued: true,
		Findings: []review.Finding{
			{ID: "f1", File: "config.go", Severity: review.SeverityError, Summary: "err swallowed", Kept: true},
			{ID: "f2", File: "config.go", Severity: review.SeverityInfo, Summary: "nit", Kept: true},
		},
	}
}

func TestInteractiveReply(t *testing.T) {
	tests := []struct {
		name       string
		script     []schema.Message
		findingID  string
		wantStatus review.FindingStatus
		wantErr    string
	}{
		{
			name:       "a withdrawal marks the finding and records the discussion",
			script:     []schema.Message{withdrawCall("c1", "the caller wraps it")},
			findingID:  "f1",
			wantStatus: review.StatusWithdrawn,
		},
		{
			// 模型坚持时状态不变，但讨论记录照样要留下——用户下次翻这条
			// 意见时该看到完整往复，而不只是"它还在这儿"。
			name:       "standing firm leaves the status open but still records the discussion",
			script:     []schema.Message{{Role: schema.RoleAssistant, Content: "我坚持"}},
			findingID:  "f2",
			wantStatus: "",
		},
		{
			name:      "an unknown finding id is an error",
			script:    []schema.Message{withdrawCall("c1", "x")},
			findingID: "f99",
			wantErr:   "no finding f99",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			i := newInteractive(t, &fakeProvider{script: tt.script}, baseRun(repo))

			got, err := i.Reply(context.Background(), "run-base", tt.findingID, "我觉得不对")

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Reply() error = nil, want one containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Reply() error = %v", err)
			}

			idx := indexOfFinding(got.Findings, tt.findingID)
			if got.Findings[idx].Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", got.Findings[idx].Status, tt.wantStatus)
			}
			if len(got.Findings[idx].Discussion) == 0 {
				t.Error("Discussion is empty; the exchange must be recorded whatever the outcome")
			}

			// 结论必须真的落盘，不能只改内存里那份。
			reloaded, err := i.Deps.Store.LoadRun(context.Background(), "run-base")
			if err != nil {
				t.Fatalf("LoadRun: %v", err)
			}
			ridx := indexOfFinding(reloaded.Findings, tt.findingID)
			if reloaded.Findings[ridx].Status != tt.wantStatus {
				t.Errorf("persisted Status = %q, want %q", reloaded.Findings[ridx].Status, tt.wantStatus)
			}
			if len(reloaded.Findings[ridx].Discussion) == 0 {
				t.Error("persisted Discussion is empty")
			}
			// 其他意见不该被这次 reply 波及。
			other := indexOfFinding(reloaded.Findings, "f1")
			if tt.findingID != "f1" && reloaded.Findings[other].Status != "" {
				t.Errorf("an unrelated finding was modified: %+v", reloaded.Findings[other])
			}
		})
	}
}

func TestInteractiveReplyRejectsUnknownRun(t *testing.T) {
	tests := []struct {
		name  string
		runID string
	}{
		{name: "missing run", runID: "nope"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := newInteractive(t, &fakeProvider{}, baseRun(t.TempDir()))
			_, err := i.Reply(context.Background(), tt.runID, "f1", "异议")
			if err == nil || !strings.Contains(err.Error(), "does not exist") {
				t.Fatalf("Reply() error = %v, want it to say the run does not exist", err)
			}
		})
	}
}

func TestInteractiveRereview(t *testing.T) {
	// 新快照：config.go 改过了（patch 不同），untouched.go 没动。
	editedPatch := testPatch + "\n// fixed\n"

	tests := []struct {
		name string
		// staged 是重审时采集到的当前暂存区。
		staged []gitdiff.Change
		// baseExtra 是基线里除 config.go 之外的额外文件与意见。
		baseExtra []gitdiff.Change
		baseMore  []review.Finding
		script    []schema.Message
		wantIDs   []string
		wantErr   string
	}{
		{
			// 主场景：改过的文件重审，没改的原样带过来。
			name:      "unchanged files keep their findings and only changed files are re-reviewed",
			staged:    []gitdiff.Change{{Status: "M", Path: "config.go", Patch: editedPatch}, {Status: "M", Path: "untouched.go", Patch: "p-untouched"}},
			baseExtra: []gitdiff.Change{{Status: "M", Path: "untouched.go", Patch: "p-untouched"}},
			// untouched.go 在基线里是 f3。重审只送 config.go，模型新产出的
			// 意见从 f1 重新编号（NormalizeReport 一律重排），于是"带过来的 f3"
			// 与"新审出来的 f1/f2"拼在一起——本例里 f3 与新产出的第二条 f2
			// 之外还会撞上，具体见下面的重复 ID 断言。
			baseMore: []review.Finding{{ID: "f3", File: "untouched.go", Severity: review.SeverityWarning, Summary: "carried over", Kept: true}},
			script: []schema.Message{
				// 产出 3 条：NormalizeReport 会把它们编号成 f1/f2/f3，
				// 其中 f3 与带过来的那条撞车——不重新编号就是两条 f3。
				submitCall("c1", "重审结果",
					map[string]any{"file": "config.go", "severity": "error", "summary": "还有问题 1"},
					map[string]any{"file": "config.go", "severity": "warning", "summary": "还有问题 2"},
					map[string]any{"file": "config.go", "severity": "info", "summary": "还有问题 3"},
				),
			},
			// f1/f2 属于改动过的 config.go，不带；f3 属于未变化文件，带过来
			// 并重新编号为 f1；新审出来的接在后面。
			wantIDs: []string{"f1", "f2"},
		},
		{
			name:    "nothing staged is an error",
			staged:  nil,
			wantErr: "nothing is staged",
		},
		{
			// 什么都没改就重审是无意义的，明确报错比默默跑一遍花钱好。
			name:    "no changed file means there is nothing to re-review",
			staged:  []gitdiff.Change{{Status: "M", Path: "config.go", Patch: testPatch}},
			wantErr: "nothing to re-review",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			base := baseRun(repo)
			base.Snapshot = append(base.Snapshot, tt.baseExtra...)
			base.Findings = append(base.Findings, tt.baseMore...)

			llm := &reviewThenCritiqueProvider{fakeProvider: fakeProvider{script: tt.script}}
			i := newInteractive(t, llm, base)
			i.LoadStaged = func(context.Context, string) ([]gitdiff.Change, error) {
				return tt.staged, nil
			}

			got, err := i.Rereview(context.Background(), "run-base")

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Rereview() error = nil, want one containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Rereview() error = %v", err)
			}

			if got.ParentRunID != "run-base" {
				t.Errorf("ParentRunID = %q, want it to link back to the baseline", got.ParentRunID)
			}
			if got.ID == "run-base" {
				t.Error("Rereview overwrote the baseline; it must create a new run")
			}
			// 快照必须是**完整**的当前暂存区，不是只有改动子集——下次重审
			// 拿它当基线，只存子集会让未变化的文件被误判成新增。
			if len(got.Snapshot) != len(tt.staged) {
				t.Errorf("Snapshot has %d files, want the full staged set (%d)", len(got.Snapshot), len(tt.staged))
			}

			var ids []string
			for _, f := range got.Findings {
				ids = append(ids, f.ID)
			}
			// ID 必须无重复：带过来的和新审出来的各自从 f1 编号。
			seen := map[string]bool{}
			for _, id := range ids {
				if seen[id] {
					t.Errorf("duplicate finding id %q in the merged run: %v", id, ids)
				}
				seen[id] = true
			}
			// 未变化文件的意见要在，改动过文件的旧意见不该在。
			var sawCarried, sawStale bool
			for _, f := range got.Findings {
				if f.Summary == "carried over" {
					sawCarried = true
				}
				if f.Summary == "err swallowed" {
					sawStale = true
				}
			}
			if !sawCarried {
				t.Error("the finding on the unchanged file was not carried over")
			}
			if sawStale {
				t.Error("a finding on a re-reviewed file was carried over; it must be replaced by the fresh review")
			}
		})
	}
}

func TestInteractiveRereviewPropagatesLoadError(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "git failure surfaces", err: errors.New("not a git repository")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := newInteractive(t, &fakeProvider{}, baseRun(t.TempDir()))
			i.LoadStaged = func(context.Context, string) ([]gitdiff.Change, error) { return nil, tt.err }

			_, err := i.Rereview(context.Background(), "run-base")
			if err == nil || !errors.Is(err, tt.err) {
				t.Fatalf("Rereview() error = %v, want it to wrap %v", err, tt.err)
			}
		})
	}
}

func TestInteractiveRereviewAutoStage(t *testing.T) {
	tests := []struct {
		name string
		// autoStage 对应配置里的 auto_stage。
		autoStage bool
		// stageErr 是注入的 StageAll 失败。
		stageErr error
		// wantStaged 是期望 StageAll 被调用的次数。
		wantStaged int
		wantErr    string
	}{
		{
			name:       "disabled leaves the index untouched",
			autoStage:  false,
			wantStaged: 0,
		},
		{
			name:       "enabled stages before snapshotting",
			autoStage:  true,
			wantStaged: 1,
		},
		{
			// stage 失败必须中断：接着跑只会拿到一份用户以为已经更新、
			// 实际是旧的暂存区，静默审错东西比报错更糟。
			name:       "stage failure aborts the re-review",
			autoStage:  true,
			stageErr:   errors.New("not a git repository"),
			wantStaged: 1,
			wantErr:    "auto-stage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			base := baseRun(repo)

			llm := &reviewThenCritiqueProvider{fakeProvider: fakeProvider{script: []schema.Message{
				submitCall("c1", "重审结果", map[string]any{"file": "config.go", "severity": "error", "summary": "还有问题"}),
			}}}
			i := newInteractive(t, llm, base)
			i.AutoStage = tt.autoStage

			var staged int
			var stagedBeforeLoad bool
			i.StageAll = func(context.Context, string) error {
				staged++
				return tt.stageErr
			}
			i.LoadStaged = func(context.Context, string) ([]gitdiff.Change, error) {
				stagedBeforeLoad = staged > 0
				return []gitdiff.Change{{Status: "M", Path: "config.go", Patch: testPatch + "\n// fixed\n"}}, nil
			}

			_, err := i.Rereview(context.Background(), "run-base")

			if staged != tt.wantStaged {
				t.Errorf("StageAll called %d times, want %d", staged, tt.wantStaged)
			}
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Rereview() error = nil, want one containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				if tt.stageErr != nil && !errors.Is(err, tt.stageErr) {
					t.Errorf("error = %v, want it to wrap %v", err, tt.stageErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Rereview() error = %v", err)
			}
			// stage 必须发生在采集快照之前，否则等于没 stage。
			if tt.autoStage && !stagedBeforeLoad {
				t.Error("StageAll ran after LoadStaged; it must stage before the snapshot is taken")
			}
		})
	}
}

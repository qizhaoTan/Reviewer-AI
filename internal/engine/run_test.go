package engine

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/review"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
	"github.com/qizhaoTan/Reviewer-AI/internal/store"
	"github.com/qizhaoTan/Reviewer-AI/internal/tool"
)

// testPatch 是一份真实形状的 diff，新文件侧行号：
//
//	10 func Load(path string) (*File, error) {   （上下文）
//	11 	data, err := os.ReadFile(path)          （上下文）
//	12 	if err != nil {                         （新增）
//	13 		return nil, err                     （新增）
//	14 	}                                       （新增）
//	15 	var f File                              （上下文）
const testPatch = `diff --git a/config.go b/config.go
--- a/config.go
+++ b/config.go
@@ -10,4 +10,7 @@ package config
 func Load(path string) (*File, error) {
 	data, err := os.ReadFile(path)
+	if err != nil {
+		return nil, err
+	}
 	var f File
`

// fakeProvider 按预设脚本逐轮返回响应，用来在不调用真实模型的前提下
// 驱动整个 tool loop。
type fakeProvider struct {
	// script 的每一项对应一轮 Generate 的返回值。
	script []schema.Message
	// err 非 nil 时，第 errAt 轮（0-based）返回该错误。
	err   error
	errAt int

	// calls 记录实际发生的轮数，以及每轮收到的消息条数，供断言使用。
	calls int
	// lastMessages 是最后一次 Generate 收到的完整消息历史。
	lastMessages []schema.Message
	// sawTools 是最后一次收到的工具定义名字。
	sawTools []string
}

func (f *fakeProvider) Generate(_ context.Context, msgs []schema.Message, tools []schema.ToolDefinition) (*schema.Message, error) {
	f.lastMessages = msgs
	f.sawTools = nil
	for _, t := range tools {
		f.sawTools = append(f.sawTools, t.Name)
	}
	idx := f.calls
	f.calls++

	if f.err != nil && idx == f.errAt {
		return nil, f.err
	}
	if idx >= len(f.script) {
		// 脚本用尽后一直返回"没有工具调用的自由文本"，用来模拟模型迟迟不肯
		// 调用 submit_review 的情形。
		return &schema.Message{Role: schema.RoleAssistant, Content: "still thinking out loud"}, nil
	}
	resp := f.script[idx]
	return &resp, nil
}

// submitCall 构造一次 submit_review 工具调用。
func submitCall(id, summary string, findings ...map[string]any) schema.Message {
	if findings == nil {
		findings = []map[string]any{}
	}
	args, err := json.Marshal(map[string]any{"summary": summary, "findings": findings})
	if err != nil {
		panic(err)
	}
	return schema.Message{
		Role:      schema.RoleAssistant,
		ToolCalls: []schema.ToolCall{{ID: id, Name: tool.SubmitReviewName, Arguments: args}},
	}
}

// globCall 构造一次普通的只读工具调用，用来占据"还没结束"的轮次。
func globCall(id string) schema.Message {
	return schema.Message{
		Role:      schema.RoleAssistant,
		ToolCalls: []schema.ToolCall{{ID: id, Name: "glob", Arguments: json.RawMessage(`{"pattern":"*.go"}`)}},
	}
}

func newTestDeps(t *testing.T, llm schema.IProvider, changes []gitdiff.Change) (Deps, *store.Store) {
	t.Helper()
	db, err := store.New("file:" + filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return Deps{
		LLM:   llm,
		Store: db,
		Tools: []tool.ITool{
			tool.ReadFileTool{},
			tool.GlobTool{},
			tool.GrepTool{},
			tool.SubmitReviewTool{Changes: changes},
		},
	}, db
}

func TestRun(t *testing.T) {
	changes := []gitdiff.Change{{Status: "M", Path: "config.go", Patch: testPatch}}

	tests := []struct {
		name   string
		script []schema.Message
		// wantErrContains 非空时期望 Run 返回错误。
		wantErrContains string
		// wantCalls 是期望的 Generate 轮数。
		wantCalls int
		// wantStatus 是运行结束后落盘记录应有的状态。
		wantStatus store.RunStatus
		// check 在期望成功时对返回的 Report 做断言。
		check func(*testing.T, *store.Run, review.Report)
	}{
		{
			name:       "submitting on the first turn ends the loop immediately",
			script:     []schema.Message{submitCall("tc1", "looks fine", map[string]any{"file": "config.go", "anchor": "return nil, err", "severity": "error", "summary": "swallowed error"})},
			wantCalls:  1,
			wantStatus: store.StatusCompleted,
			check: func(t *testing.T, run *store.Run, r review.Report) {
				if len(r.Findings) != 1 {
					t.Fatalf("len(Findings) = %d, want 1", len(r.Findings))
				}
				if r.Findings[0].StartLine != 13 {
					t.Errorf("StartLine = %d, want 13 (anchor must be resolved)", r.Findings[0].StartLine)
				}
			},
		},
		{
			name: "investigates with read-only tools before submitting",
			script: []schema.Message{
				globCall("tc1"),
				globCall("tc2"),
				submitCall("tc3", "checked the surrounding code, all good"),
			},
			wantCalls:  3,
			wantStatus: store.StatusCompleted,
			check: func(t *testing.T, run *store.Run, r review.Report) {
				if len(r.Findings) != 0 {
					t.Errorf("len(Findings) = %d, want 0", len(r.Findings))
				}
				if r.Summary == "" {
					t.Error("Summary is empty, want the model's all-clear explanation")
				}
			},
		},
		{
			name: "plain text answer is rejected and the model is nudged to submit",
			script: []schema.Message{
				{Role: schema.RoleAssistant, Content: "## Review\n\nLooks good to me!"},
				submitCall("tc1", "fine after all"),
			},
			wantCalls:  2,
			wantStatus: store.StatusCompleted,
			check: func(t *testing.T, run *store.Run, r review.Report) {
				// 那段自由文本必须被一条"提醒去调 submit_review"的用户消息跟上，
				// 否则模型不知道自己漏了什么。
				var nudged bool
				for _, m := range run.Messages {
					if m.Role == schema.RoleUser && strings.Contains(m.Content, tool.SubmitReviewName) &&
						strings.Contains(m.Content, "还没有被记录") {
						nudged = true
					}
				}
				if !nudged {
					t.Error("no nudge message found; the model must be told its plain-text answer was not recorded")
				}
			},
		},
		{
			name: "invalid submission is retried after the tool error",
			script: []schema.Message{
				// severity 非法，工具会拒绝并回喂错误。
				submitCall("tc1", "ok", map[string]any{"file": "config.go", "severity": "blocker", "summary": "boom"}),
				submitCall("tc2", "ok", map[string]any{"file": "config.go", "severity": "warning", "summary": "boom"}),
			},
			wantCalls:  2,
			wantStatus: store.StatusCompleted,
			check: func(t *testing.T, run *store.Run, r review.Report) {
				if len(r.Findings) != 1 || r.Findings[0].Severity != "warning" {
					t.Errorf("Findings = %+v, want the corrected submission to win", r.Findings)
				}
			},
		},
		{
			name:            "never submitting exhausts the loop and fails the run",
			script:          nil, // 脚本为空 → 每轮都返回自由文本，永远不提交
			wantErrContains: tool.SubmitReviewName,
			wantCalls:       maxToolLoopIterations,
			wantStatus:      store.StatusFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &fakeProvider{script: tt.script}
			deps, db := newTestDeps(t, llm, changes)
			repoAbs := t.TempDir()

			run, err := Run(context.Background(), deps, repoAbs, "main", "", changes, time.Minute)

			if tt.wantErrContains != "" {
				if err == nil {
					t.Fatalf("Run() error = nil, want error containing %q", tt.wantErrContains)
				}
				if !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Errorf("error %q does not contain %q", err, tt.wantErrContains)
				}
			} else if err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			if llm.calls != tt.wantCalls {
				t.Errorf("Generate called %d times, want %d", llm.calls, tt.wantCalls)
			}

			// 无论成败，落盘记录的状态都要正确——失败时也必须留下 failed 记录。
			persisted := latestRun(t, db, repoAbs, "main")
			if persisted.Status != tt.wantStatus {
				t.Errorf("persisted Status = %q, want %q", persisted.Status, tt.wantStatus)
			}

			if tt.check != nil {
				tt.check(t, run, run.Report())
			}
		})
	}
}

func TestRunExposesSubmitReviewToTheModel(t *testing.T) {
	changes := []gitdiff.Change{{Status: "M", Path: "config.go", Patch: testPatch}}

	tests := []struct {
		name      string
		wantTools []string
	}{
		{name: "all four tools are offered", wantTools: []string{"read_file", "glob", "grep", tool.SubmitReviewName}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &fakeProvider{script: []schema.Message{submitCall("tc1", "fine")}}
			deps, _ := newTestDeps(t, llm, changes)

			if _, err := Run(context.Background(), deps, t.TempDir(), "main", "", changes, time.Minute); err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			for _, want := range tt.wantTools {
				found := false
				for _, got := range llm.sawTools {
					if got == want {
						found = true
					}
				}
				if !found {
					t.Errorf("tools offered to the model %v do not include %q", llm.sawTools, want)
				}
			}
		})
	}
}

// TestRunGenerateFailureStaysResumable 锁住"Generate 失败可恢复"这条语义：
// 网络抖动跟已经攒下的消息历史无关，运行必须留在 in_progress，这样下次重跑
// 才能沿用这些历史接着问，而不是从头审一遍、白白丢掉已经付过费的工具调用。
func TestRunGenerateFailureStaysResumable(t *testing.T) {
	changes := []gitdiff.Change{{Status: "M", Path: "config.go", Patch: testPatch}}

	tests := []struct {
		name  string
		errAt int
		// wantMessages 是失败落盘后消息历史至少应有的条数。初始的
		// [system, user] 两条一定在；errAt=1 时还多攒了一轮工具往返。
		wantMinMessages int
	}{
		{name: "failing on the first turn", errAt: 0, wantMinMessages: 2},
		{name: "failing after some progress", errAt: 1, wantMinMessages: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &fakeProvider{
				script: []schema.Message{globCall("tc1"), globCall("tc2")},
				err:    errors.New("upstream exploded"),
				errAt:  tt.errAt,
			}
			deps, db := newTestDeps(t, llm, changes)
			repoAbs := t.TempDir()

			_, err := Run(context.Background(), deps, repoAbs, "main", "", changes, time.Minute)
			if err == nil {
				t.Fatal("Run() error = nil, want the provider failure to surface")
			}
			if !strings.Contains(err.Error(), "upstream exploded") {
				t.Errorf("error %q does not mention the underlying cause", err)
			}

			got := latestRun(t, db, repoAbs, "main")
			if got.Status != store.StatusInProgress {
				t.Errorf("persisted Status = %q, want %q: a provider failure must leave the run resumable",
					got.Status, store.StatusInProgress)
			}
			if len(got.Messages) < tt.wantMinMessages {
				t.Errorf("persisted %d messages, want at least %d: the history so far must survive the failure",
					len(got.Messages), tt.wantMinMessages)
			}
		})
	}
}

// TestRunResumesAfterGenerateFailure 走完整的两趟：第一趟中途 Generate 失败，
// 第二趟必须接着同一条记录跑完，而不是新建一条从头审。
func TestRunResumesAfterGenerateFailure(t *testing.T) {
	changes := []gitdiff.Change{{Status: "M", Path: "config.go", Patch: testPatch}}

	tests := []struct {
		name string
	}{
		{name: "the second attempt continues the same run"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoAbs := t.TempDir()

			// 第一趟：先成功跑一轮工具调用，第二轮 Generate 炸掉。
			failing := &fakeProvider{
				script: []schema.Message{globCall("tc1"), globCall("tc2")},
				err:    errors.New("upstream exploded"),
				errAt:  1,
			}
			deps, db := newTestDeps(t, failing, changes)
			if _, err := Run(context.Background(), deps, repoAbs, "main", "", changes, time.Minute); err == nil {
				t.Fatal("first Run() error = nil, want the provider failure to surface")
			}
			first := latestRun(t, db, repoAbs, "main")
			resumedFrom := len(first.Messages)

			// 第二趟：换一个正常的 provider，复用同一个 store。
			deps.LLM = &fakeProvider{script: []schema.Message{submitCall("tc3", "done",
				map[string]any{"file": "config.go", "anchor": "return nil, err", "severity": "error", "summary": "swallowed error"})}}
			run, err := Run(context.Background(), deps, repoAbs, "main", "", changes, time.Minute)
			if err != nil {
				t.Fatalf("second Run() error = %v", err)
			}

			if run.ID != first.ID {
				t.Errorf("second attempt used run %s, want it to resume %s instead of starting over",
					run.ID, first.ID)
			}
			if run.Status != store.StatusCompleted {
				t.Errorf("Status = %q, want %q", run.Status, store.StatusCompleted)
			}
			if len(run.Messages) <= resumedFrom {
				t.Errorf("len(Messages) = %d, want more than the %d carried over: the resumed run must build on the old history",
					len(run.Messages), resumedFrom)
			}
			if got := len(run.Report().Findings); got != 1 {
				t.Errorf("len(Findings) = %d, want 1", got)
			}

			runs, err := db.ListRuns(context.Background(), buildRunKey(repoAbs, "main", ""), 10)
			if err != nil {
				t.Fatalf("ListRuns: %v", err)
			}
			if len(runs) != 1 {
				t.Errorf("persisted %d runs, want 1: resuming must not leave an orphaned record behind", len(runs))
			}
		})
	}
}

// TestRunFreshIgnoresResumableRun 锁住 -fresh 的逃生舱语义：哪怕存在一条
// 内容相同、可续跑的记录，也要另起一条从头审。
func TestRunFreshIgnoresResumableRun(t *testing.T) {
	changes := []gitdiff.Change{{Status: "M", Path: "config.go", Patch: testPatch}}

	tests := []struct {
		name       string
		seedStatus store.RunStatus
	}{
		{name: "ignores a resumable in-progress run", seedStatus: store.StatusInProgress},
		{name: "ignores a completed run instead of reusing its result", seedStatus: store.StatusCompleted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoAbs := t.TempDir()

			// 先种一条内容相同的记录：不带 fresh 时它会被复用。
			seeding := &fakeProvider{
				script: []schema.Message{globCall("tc1"), globCall("tc2")},
				err:    errors.New("upstream exploded"),
				errAt:  1,
			}
			deps, db := newTestDeps(t, seeding, changes)
			if _, err := Run(context.Background(), deps, repoAbs, "main", "", changes, time.Minute); err == nil {
				t.Fatal("seeding Run() error = nil, want the provider failure to surface")
			}
			seeded := latestRun(t, db, repoAbs, "main")
			if tt.seedStatus == store.StatusCompleted {
				seeded.Status = store.StatusCompleted
				if err := db.SaveRun(context.Background(), seeded); err != nil {
					t.Fatalf("SaveRun: %v", err)
				}
			}

			deps.LLM = &fakeProvider{script: []schema.Message{submitCall("tc3", "done",
				map[string]any{"file": "config.go", "anchor": "return nil, err", "severity": "error", "summary": "swallowed error"})}}
			deps.Fresh = true
			run, err := Run(context.Background(), deps, repoAbs, "main", "", changes, time.Minute)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			if run.ID == seeded.ID {
				t.Errorf("Run reused run %s, want -fresh to start a new one", seeded.ID)
			}
			if run.Status != store.StatusCompleted {
				t.Errorf("Status = %q, want %q", run.Status, store.StatusCompleted)
			}

			runs, err := db.ListRuns(context.Background(), buildRunKey(repoAbs, "main", ""), 10)
			if err != nil {
				t.Fatalf("ListRuns: %v", err)
			}
			if len(runs) != 2 {
				t.Errorf("persisted %d runs, want 2: -fresh must add a record, not overwrite the old one", len(runs))
			}
		})
	}
}

// latestRun 取出 repoAbs/branch 对应的最近一条记录。
func latestRun(t *testing.T, db *store.Store, repoAbs, branch string) store.Run {
	t.Helper()
	runs, err := db.ListRuns(context.Background(), buildRunKey(repoAbs, branch, ""), 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) == 0 {
		t.Fatal("no runs persisted, want at least one")
	}
	return runs[0]
}

// critiqueDeps 在基础 Deps 上补齐复核工具，让 Run 会真的走复核阶段。
func withCritique(deps Deps, changes []gitdiff.Change) Deps {
	deps.CritiqueTools = []tool.ITool{
		tool.ReadFileTool{}, tool.GlobTool{}, tool.GrepTool{}, tool.CritiqueVerdictTool{},
	}
	deps.CritiqueConcurrency = 2
	deps.CritiqueMaxTurns = 5
	return deps
}

// reviewThenCritiqueProvider 先按初审脚本走，之后的请求（工具清单里含
// submit_verdict）一律当作复核请求处理。
type reviewThenCritiqueProvider struct {
	fakeProvider
	// keep 决定复核裁决；nil 表示一律保留。
	keep func(userMessage string) bool
	// critiqueCalls 记录复核阶段发生了多少次 Generate。复核是并发的，
	// 多个 goroutine 会同时命中这个计数器，必须用原子操作。
	critiqueCalls atomic.Int64

	// mu 保护 critiqueErr：它会在两趟 Run 之间被改写，而复核 goroutine 并发读它。
	mu sync.Mutex
	// critiqueErr 非 nil 时复核阶段的 Generate 一律以该错误告终，用来模拟
	// 基础设施抖动。
	critiqueErr error
}

func (p *reviewThenCritiqueProvider) Generate(ctx context.Context, msgs []schema.Message, tools []schema.ToolDefinition) (*schema.Message, error) {
	for _, t := range tools {
		if t.Name == tool.CritiqueVerdictName {
			p.critiqueCalls.Add(1)
			p.mu.Lock()
			critiqueErr := p.critiqueErr
			p.mu.Unlock()
			if critiqueErr != nil {
				return nil, critiqueErr
			}
			keep := p.keep == nil || p.keep(msgs[1].Content)
			args, _ := json.Marshal(tool.Verdict{Keep: keep, Reason: "verdict"})
			return &schema.Message{
				Role:      schema.RoleAssistant,
				ToolCalls: []schema.ToolCall{{ID: "v1", Name: tool.CritiqueVerdictName, Arguments: args}},
			}, nil
		}
	}
	return p.fakeProvider.Generate(ctx, msgs, tools)
}

// critiqueFailure 让 reviewThenCritiqueProvider 的复核阶段报错。设成字段而不是
// 构造参数，是为了能在两趟 Run 之间把它清掉，模拟"网络恢复了"。
func (p *reviewThenCritiqueProvider) failCritique(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.critiqueErr = err
}

// TestRunResumesAfterCritiqueFailure 走完整的两趟：第一趟初审成功、复核因为
// provider 报错中断，第二趟必须直接从复核接着跑——初审已经付过费了，不能重来。
func TestRunResumesAfterCritiqueFailure(t *testing.T) {
	changes := []gitdiff.Change{{Status: "M", Path: "config.go", Patch: testPatch}}

	tests := []struct {
		name string
	}{
		{name: "the retry re-runs only the critique"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &reviewThenCritiqueProvider{}
			llm.script = []schema.Message{submitCall("tc1", "two comments",
				map[string]any{"file": "config.go", "anchor": "return nil, err", "severity": "error", "summary": "real problem"},
				map[string]any{"file": "config.go", "severity": "info", "summary": "noise"},
			)}
			llm.failCritique(errors.New("critic is down"))

			base, db := newTestDeps(t, llm, changes)
			deps := withCritique(base, changes)
			repoAbs := t.TempDir()

			// 第一趟：初审跑完并落盘，复核炸掉。
			_, err := Run(context.Background(), deps, repoAbs, "main", "", changes, time.Minute)
			if err == nil {
				t.Fatal("first Run() error = nil, want the critique failure to surface")
			}
			if !strings.Contains(err.Error(), "critic is down") {
				t.Errorf("error %q does not mention the underlying cause", err)
			}

			first := latestRun(t, db, repoAbs, "main")
			if first.Status != store.StatusInProgress {
				t.Errorf("persisted Status = %q, want %q: the primary review is done and must not be thrown away",
					first.Status, store.StatusInProgress)
			}
			if len(first.Findings) != 2 {
				t.Fatalf("persisted Findings = %d, want 2: the primary review must survive the critique failure",
					len(first.Findings))
			}
			if first.Critiqued {
				t.Error("persisted Critiqued = true, want false: the critique never finished")
			}

			// 第二趟：复核恢复正常。初审脚本已经用尽，如果引擎错误地重跑初审，
			// fakeProvider 会因为脚本耗尽而报错，这个用例就会失败。
			llm.failCritique(nil)
			run, err := Run(context.Background(), deps, repoAbs, "main", "", changes, time.Minute)
			if err != nil {
				t.Fatalf("second Run() error = %v", err)
			}

			if run.ID != first.ID {
				t.Errorf("second attempt used run %s, want it to resume %s", run.ID, first.ID)
			}
			if run.Status != store.StatusCompleted {
				t.Errorf("Status = %q, want %q", run.Status, store.StatusCompleted)
			}
			if !run.Critiqued {
				t.Error("Critiqued = false, want true after the retry finished the critique")
			}
			if len(run.Findings) != 2 {
				t.Errorf("len(Findings) = %d, want 2", len(run.Findings))
			}
			for i, f := range run.Findings {
				if f.CritiqueReason != "verdict" {
					t.Errorf("Findings[%d].CritiqueReason = %q, want the real verdict rather than a failure note",
						i, f.CritiqueReason)
				}
			}
		})
	}
}

// TestRunPersistsFindings 验证审查结果确实写进了 Run 记录，而不只是随返回值传出。
func TestRunPersistsFindings(t *testing.T) {
	changes := []gitdiff.Change{{Status: "M", Path: "config.go", Patch: testPatch}}

	tests := []struct {
		name string
		keep func(string) bool
		// wantPersistedFindings 是落盘记录里应有的意见条数（含被丢弃的）。
		wantPersistedFindings int
		// wantKept 是复核后仍然保留的条数。
		wantKept int
	}{
		{
			name:                  "keeping both findings persists both",
			wantPersistedFindings: 2, wantKept: 2,
		},
		{
			name:                  "a dropped finding is still persisted",
			keep:                  func(msg string) bool { return !strings.Contains(msg, "noise") },
			wantPersistedFindings: 2, wantKept: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &reviewThenCritiqueProvider{keep: tt.keep}
			llm.script = []schema.Message{submitCall("tc1", "two comments",
				map[string]any{"file": "config.go", "anchor": "return nil, err", "severity": "error", "summary": "real problem"},
				map[string]any{"file": "config.go", "severity": "info", "summary": "noise"},
			)}
			base, db := newTestDeps(t, llm, changes)
			deps := withCritique(base, changes)
			repoAbs := t.TempDir()

			run, err := Run(context.Background(), deps, repoAbs, "main", "", changes, time.Minute)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			persisted := latestRun(t, db, repoAbs, "main")
			if len(persisted.Findings) != tt.wantPersistedFindings {
				t.Fatalf("persisted Findings = %d, want %d (dropped findings must be kept in the database)",
					len(persisted.Findings), tt.wantPersistedFindings)
			}
			if !persisted.Critiqued {
				t.Error("persisted Critiqued = false, want true after the critique ran")
			}
			if persisted.Summary == "" {
				t.Error("persisted Summary is empty, want the model's overall assessment")
			}
			if got := len(persisted.Report().KeptFindings()); got != tt.wantKept {
				t.Errorf("kept findings = %d, want %d", got, tt.wantKept)
			}
			// 返回的 run 和落盘的记录必须一致——调用方拿哪个都一样。
			if len(run.Findings) != len(persisted.Findings) {
				t.Errorf("returned run has %d findings, persisted has %d", len(run.Findings), len(persisted.Findings))
			}
		})
	}
}

// TestRunReusesPersistedFindingsOnCacheHit 验证命中历史 completed 记录时，
// 结果直接来自数据库，一次模型调用都不发生。
func TestRunReusesPersistedFindingsOnCacheHit(t *testing.T) {
	changes := []gitdiff.Change{{Status: "M", Path: "config.go", Patch: testPatch}}

	tests := []struct {
		name string
	}{
		{name: "the second run answers from the database"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &reviewThenCritiqueProvider{}
			llm.script = []schema.Message{submitCall("tc1", "one comment",
				map[string]any{"file": "config.go", "anchor": "return nil, err", "severity": "error", "summary": "real problem"},
			)}
			base, _ := newTestDeps(t, llm, changes)
			deps := withCritique(base, changes)
			repoAbs := t.TempDir()

			first, err := Run(context.Background(), deps, repoAbs, "main", "", changes, time.Minute)
			if err != nil {
				t.Fatalf("first Run() error = %v", err)
			}
			callsAfterFirst := llm.calls + int(llm.critiqueCalls.Load())

			// 同样的内容再跑一次：应该完全命中缓存。
			second, err := Run(context.Background(), deps, repoAbs, "main", "", changes, time.Minute)
			if err != nil {
				t.Fatalf("second Run() error = %v", err)
			}

			if got := llm.calls + int(llm.critiqueCalls.Load()); got != callsAfterFirst {
				t.Errorf("the second run made %d extra model calls, want 0", got-callsAfterFirst)
			}
			if second.ID != first.ID {
				t.Errorf("second run ID = %q, want the cached run %q", second.ID, first.ID)
			}
			report := second.Report()
			if len(report.Findings) != 1 || report.Findings[0].Summary != "real problem" {
				t.Errorf("cached report = %+v, want the findings stored by the first run", report)
			}
			if !report.Critiqued {
				t.Error("cached report Critiqued = false, want the stored value")
			}
			if report.Findings[0].StartLine != 13 {
				t.Errorf("cached StartLine = %d, want 13: resolved line numbers must survive the round trip",
					report.Findings[0].StartLine)
			}

		})
	}
}

// TestRunResumesFromCritique 验证一条"初审已完成、复核未完成"的记录被恢复时，
// 直接从复核阶段接着跑，不让模型把整个 changeset 重审一遍。
func TestRunResumesFromCritique(t *testing.T) {
	changes := []gitdiff.Change{{Status: "M", Path: "config.go", Patch: testPatch}}

	tests := []struct {
		name string
		// seedFindings 是预置的初审结果。
		seedFindings []review.Finding
		wantKept     int
	}{
		{
			name: "resumes with the stored findings instead of re-reviewing",
			seedFindings: []review.Finding{
				{ID: "f1", File: "config.go", Severity: review.SeverityError, Summary: "real problem"},
				{ID: "f2", File: "config.go", Severity: review.SeverityInfo, Summary: "noise"},
			},
			wantKept: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &reviewThenCritiqueProvider{
				keep: func(msg string) bool { return !strings.Contains(msg, "noise") },
			}
			// 初审脚本刻意留空：一旦走了主循环就会拿不到 submit_review 而失败，
			// 从而暴露"没有真正跳过初审"的问题。
			base, db := newTestDeps(t, llm, changes)
			deps := withCritique(base, changes)
			repoAbs := t.TempDir()

			// 预置一条初审已完成、复核未完成的记录。
			seed := store.Run{
				ID:       store.NewRunID(),
				RepoPath: buildRunKey(repoAbs, "main", ""),
				Status:   store.StatusInProgress,
				Snapshot: changes,
				Findings: tt.seedFindings,
				Summary:  "initial review done",
			}
			if err := db.SaveRun(context.Background(), seed); err != nil {
				t.Fatalf("seed SaveRun: %v", err)
			}

			run, err := Run(context.Background(), deps, repoAbs, "main", "", changes, time.Minute)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			if run.ID != seed.ID {
				t.Errorf("run ID = %q, want the seeded run %q (a new run means the initial review was thrown away)",
					run.ID, seed.ID)
			}
			if llm.calls != 0 {
				t.Errorf("the main review loop ran %d times, want 0: the stored initial review must be reused", llm.calls)
			}
			if got := int(llm.critiqueCalls.Load()); got != len(tt.seedFindings) {
				t.Errorf("critique made %d calls, want %d (one per finding)", got, len(tt.seedFindings))
			}
			if !run.Critiqued {
				t.Error("Critiqued = false, want true after resuming through the critique")
			}
			if got := len(run.Report().KeptFindings()); got != tt.wantKept {
				t.Errorf("kept findings = %d, want %d", got, tt.wantKept)
			}
		})
	}
}

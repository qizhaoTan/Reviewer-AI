package engine

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
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
		check func(*testing.T, *store.Run, *review.Report)
	}{
		{
			name:       "submitting on the first turn ends the loop immediately",
			script:     []schema.Message{submitCall("tc1", "looks fine", map[string]any{"file": "config.go", "anchor": "return nil, err", "severity": "error", "summary": "swallowed error"})},
			wantCalls:  1,
			wantStatus: store.StatusCompleted,
			check: func(t *testing.T, run *store.Run, r *review.Report) {
				if r == nil {
					t.Fatal("Report is nil, want the submitted review")
				}
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
			check: func(t *testing.T, run *store.Run, r *review.Report) {
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
			check: func(t *testing.T, run *store.Run, r *review.Report) {
				// 那段自由文本必须被一条"提醒去调 submit_review"的用户消息跟上，
				// 否则模型不知道自己漏了什么。
				var nudged bool
				for _, m := range run.Messages {
					if m.Role == schema.RoleUser && strings.Contains(m.Content, tool.SubmitReviewName) &&
						strings.Contains(m.Content, "has not been recorded") {
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
			check: func(t *testing.T, run *store.Run, r *review.Report) {
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

			run, report, err := Run(context.Background(), deps, repoAbs, "main", changes, time.Minute)

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
				tt.check(t, run, report)
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

			if _, _, err := Run(context.Background(), deps, t.TempDir(), "main", changes, time.Minute); err != nil {
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

func TestRunGenerateFailureMarksRunFailed(t *testing.T) {
	changes := []gitdiff.Change{{Status: "M", Path: "config.go", Patch: testPatch}}

	tests := []struct {
		name  string
		errAt int
	}{
		{name: "failing on the first turn", errAt: 0},
		{name: "failing after some progress", errAt: 1},
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

			_, _, err := Run(context.Background(), deps, repoAbs, "main", changes, time.Minute)
			if err == nil {
				t.Fatal("Run() error = nil, want the provider failure to surface")
			}
			if !strings.Contains(err.Error(), "upstream exploded") {
				t.Errorf("error %q does not mention the underlying cause", err)
			}
			if got := latestRun(t, db, repoAbs, "main").Status; got != store.StatusFailed {
				t.Errorf("persisted Status = %q, want %q", got, store.StatusFailed)
			}
		})
	}
}

// latestRun 取出 repoAbs/branch 对应的最近一条记录。
func latestRun(t *testing.T, db *store.Store, repoAbs, branch string) store.Run {
	t.Helper()
	runs, err := db.ListRuns(context.Background(), buildRunKey(repoAbs, branch), 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) == 0 {
		t.Fatal("no runs persisted, want at least one")
	}
	return runs[0]
}

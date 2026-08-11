package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/review"
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

func testChanges() []gitdiff.Change {
	return []gitdiff.Change{{Status: "M", Path: "config.go", Patch: testPatch}}
}

func TestSubmitReviewExecute(t *testing.T) {
	tests := []struct {
		name string
		// args 是模型提交的原始 JSON 参数。
		args string
		// changes 为 nil 时使用 testChanges()。
		changes []gitdiff.Change
		// wantErrContains 非空时期望调用失败，且 Output 含这些片段
		// （这些文字会回喂给模型，所以断言它确实说清了错在哪、怎么改）。
		wantErrContains []string
		// check 在期望成功时对收集到的 Report 做断言。
		check func(*testing.T, review.Report)
	}{
		{
			name: "accepts a well formed submission and resolves anchors to line numbers",
			args: `{"summary":"looks reasonable overall","findings":[
				{"file":"config.go","anchor":"return nil, err","severity":"error","summary":"error is swallowed","detail":"wrap it"}
			]}`,
			check: func(t *testing.T, r review.Report) {
				if len(r.Findings) != 1 {
					t.Fatalf("len(Findings) = %d, want 1", len(r.Findings))
				}
				f := r.Findings[0]
				if f.ID != "f1" {
					t.Errorf("ID = %q, want %q", f.ID, "f1")
				}
				if f.StartLine != 13 || f.EndLine != 13 {
					t.Errorf("lines = %d-%d, want 13-13 (anchor must be resolved during Execute)", f.StartLine, f.EndLine)
				}
				if f.Severity != review.SeverityError {
					t.Errorf("Severity = %q, want %q", f.Severity, review.SeverityError)
				}
			},
		},
		{
			name: "resolves a multi line anchor to a range",
			args: `{"summary":"ok","findings":[
				{"file":"config.go","anchor":"if err != nil {\n\treturn nil, err\n}","severity":"warning","summary":"early return"}
			]}`,
			check: func(t *testing.T, r review.Report) {
				if r.Findings[0].StartLine != 12 || r.Findings[0].EndLine != 14 {
					t.Errorf("lines = %d-%d, want 12-14", r.Findings[0].StartLine, r.Findings[0].EndLine)
				}
			},
		},
		{
			name: "accepts an empty findings list as an explicit all clear",
			args: `{"summary":"small and straightforward change, nothing to flag","findings":[]}`,
			check: func(t *testing.T, r review.Report) {
				if len(r.Findings) != 0 {
					t.Errorf("len(Findings) = %d, want 0", len(r.Findings))
				}
				if r.Summary == "" {
					t.Error("Summary is empty, want the model's all-clear explanation")
				}
			},
		},
		{
			name: "accepts a finding with no anchor as a file level note",
			args: `{"summary":"ok","findings":[
				{"file":"config.go","severity":"info","summary":"file needs a package comment"}
			]}`,
			check: func(t *testing.T, r review.Report) {
				if r.Findings[0].StartLine != 0 || r.Findings[0].EndLine != 0 {
					t.Errorf("lines = %d-%d, want 0-0 for a file level finding",
						r.Findings[0].StartLine, r.Findings[0].EndLine)
				}
			},
		},
		{
			name: "unresolvable anchor degrades to a file level finding instead of failing",
			args: `{"summary":"ok","findings":[
				{"file":"config.go","anchor":"code that appears nowhere in the diff","severity":"info","summary":"note"}
			]}`,
			check: func(t *testing.T, r review.Report) {
				if len(r.Findings) != 1 {
					t.Fatalf("len(Findings) = %d, want the finding to survive an unresolvable anchor", len(r.Findings))
				}
				if r.Findings[0].StartLine != 0 {
					t.Errorf("StartLine = %d, want 0", r.Findings[0].StartLine)
				}
			},
		},
		{
			name:            "rejects malformed json",
			args:            `{"summary": "unterminated`,
			wantErrContains: []string{"not valid JSON", `"findings"`},
		},
		{
			name:            "rejects a missing summary",
			args:            `{"findings":[]}`,
			wantErrContains: []string{"summary is empty", "even when there are no findings"},
		},
		{
			name: "rejects an unknown severity and lists the allowed values",
			args: `{"summary":"ok","findings":[
				{"file":"config.go","severity":"blocker","summary":"boom"}
			]}`,
			wantErrContains: []string{`severity is "blocker"`, `"info"`, `"warning"`, `"error"`},
		},
		{
			name: "rejects a finding with no summary",
			args: `{"summary":"ok","findings":[
				{"file":"config.go","severity":"info","summary":"  "}
			]}`,
			wantErrContains: []string{"findings[0].summary is empty", "one sentence"},
		},
		{
			name: "rejects a file outside the staged changeset and lists what is available",
			args: `{"summary":"ok","findings":[
				{"file":"some/other.go","severity":"error","summary":"boom"}
			]}`,
			wantErrContains: []string{"not part of this staged changeset", `"config.go"`},
		},
		{
			name: "reports the offending index when a later finding is invalid",
			args: `{"summary":"ok","findings":[
				{"file":"config.go","severity":"info","summary":"fine"},
				{"file":"config.go","severity":"nope","summary":"bad"}
			]}`,
			wantErrContains: []string{"findings[1].severity"},
		},
		{
			name:    "skips the changeset check when no snapshot is configured",
			args:    `{"summary":"ok","findings":[{"file":"anything.go","severity":"info","summary":"note"}]}`,
			changes: []gitdiff.Change{},
			check: func(t *testing.T, r review.Report) {
				if len(r.Findings) != 1 {
					t.Errorf("len(Findings) = %d, want 1", len(r.Findings))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changes := tt.changes
			if changes == nil {
				changes = testChanges()
			}
			tool := SubmitReviewTool{Changes: changes}

			got := tool.Execute(context.Background(), "/repo", json.RawMessage(tt.args))

			if len(tt.wantErrContains) > 0 {
				if !got.IsError {
					t.Fatalf("Execute() IsError = false, want an error; Output = %q", got.Output)
				}
				for _, want := range tt.wantErrContains {
					if !strings.Contains(got.Output, want) {
						t.Errorf("Output %q does not contain %q", got.Output, want)
					}
				}
				if got.ReviewResult != nil {
					t.Error("a rejected submission must not produce a ReviewResult")
				}
				return
			}

			if got.IsError {
				t.Fatalf("Execute() IsError = true, Output = %q", got.Output)
			}
			if got.ReviewResult == nil {
				t.Fatal("ReviewResult is nil, want the submitted report")
			}
			if got.ReviewResult.Critiqued {
				t.Error("Critiqued = true, want false: submitting is not critiquing")
			}
			if tt.check != nil {
				tt.check(t, *got.ReviewResult)
			}
		})
	}
}

// TestSubmitReviewLeavesOtherToolsResultsAlone 固定住"只有 submit_review 填
// ReviewResult"这条约定：其他工具一旦误填，engine 会把它们的调用误判为
// "审查已提交"而提前终止循环。
func TestSubmitReviewLeavesOtherToolsResultsAlone(t *testing.T) {
	tests := []struct {
		name string
		tool ITool
		args string
	}{
		{name: "read_file leaves ReviewResult nil", tool: ReadFileTool{}, args: `{"path":"go.mod"}`},
		{name: "glob leaves ReviewResult nil", tool: GlobTool{}, args: `{"pattern":"*.go"}`},
		{name: "grep leaves ReviewResult nil", tool: GrepTool{}, args: `{"pattern":"package"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.tool.Execute(context.Background(), t.TempDir(), json.RawMessage(tt.args))
			if got.ReviewResult != nil {
				t.Errorf("%s produced a ReviewResult; only %s may do that",
					tt.tool.Definition().Name, SubmitReviewName)
			}
		})
	}
}

func TestSubmitReviewDefinition(t *testing.T) {
	tests := []struct {
		name string
		// wantRequired 是顶层 required 里必须出现的字段。
		wantRequired []string
	}{
		{name: "requires summary and findings", wantRequired: []string{"summary", "findings"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := SubmitReviewTool{}.Definition()
			if def.Name != SubmitReviewName {
				t.Errorf("Name = %q, want %q", def.Name, SubmitReviewName)
			}

			schemaMap, ok := def.InputSchema.(map[string]interface{})
			if !ok {
				t.Fatalf("InputSchema is %T, want map[string]interface{}", def.InputSchema)
			}
			required, _ := schemaMap["required"].([]interface{})
			for _, want := range tt.wantRequired {
				found := false
				for _, r := range required {
					if r == want {
						found = true
					}
				}
				if !found {
					t.Errorf("required %v does not include %q", required, want)
				}
			}

			// severity 的 enum 必须与 review.AllSeverities 同源，否则模型会被告知
			// 一套取值、校验时却按另一套拒绝。
			props := schemaMap["properties"].(map[string]interface{})
			items := props["findings"].(map[string]interface{})["items"].(map[string]interface{})
			itemProps := items["properties"].(map[string]interface{})
			gotEnum, _ := itemProps["severity"].(map[string]interface{})["enum"].([]interface{})
			if len(gotEnum) != len(review.AllSeverities()) {
				t.Fatalf("severity enum = %v, want %d values matching review.AllSeverities()", gotEnum, len(review.AllSeverities()))
			}
			for i, s := range review.AllSeverities() {
				if gotEnum[i] != string(s) {
					t.Errorf("severity enum[%d] = %v, want %q", i, gotEnum[i], s)
				}
			}

			// anchor 的描述必须明确告诉模型不要自己报行号——这是整个
			// anchor 机制的前提，措辞丢了模型就会退回去猜行号。
			anchorDesc, _ := itemProps["anchor"].(map[string]interface{})["description"].(string)
			if !strings.Contains(anchorDesc, "Do NOT report line numbers") {
				t.Errorf("anchor description %q must tell the model not to report line numbers", anchorDesc)
			}
		})
	}
}

func TestSubmitReviewIsDiscoverableByName(t *testing.T) {
	tests := []struct {
		name     string
		lookup   string
		wantFind bool
	}{
		{name: "found via the exported constant", lookup: SubmitReviewName, wantFind: true},
		{name: "not found under a different name", lookup: "submit_findings", wantFind: false},
	}

	tools := []ITool{ReadFileTool{}, GlobTool{}, GrepTool{}, SubmitReviewTool{}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FindToolByName(tools, tt.lookup)
			if tt.wantFind {
				if err != nil {
					t.Fatalf("FindToolByName(%q) error = %v", tt.lookup, err)
				}
				if got.Definition().Name != tt.lookup {
					t.Errorf("found tool %q, want %q", got.Definition().Name, tt.lookup)
				}
				return
			}
			if err == nil {
				t.Errorf("FindToolByName(%q) found %q, want an error", tt.lookup, got.Definition().Name)
			}
		})
	}
}

// TestSubmitReviewSharedInstanceIsConcurrencySafe 验证单个工具实例可以被多个
// 并发的审查链路共享——这正是把结果放进返回值而不是实例字段换来的性质，
// 阶段五的并发子 Agent 依赖它。每个 goroutine 必须拿到自己那次提交的结果，
// 互不串味。需要配合 -race 运行才有完整意义。
func TestSubmitReviewSharedInstanceIsConcurrencySafe(t *testing.T) {
	tests := []struct {
		name       string
		goroutines int
	}{
		{name: "each caller gets back its own report", goroutines: 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 一个实例，被所有 goroutine 共享。
			shared := SubmitReviewTool{Changes: testChanges()}

			var wg sync.WaitGroup
			errs := make([]error, tt.goroutines)
			for i := range tt.goroutines {
				wg.Add(1)
				go func() {
					defer wg.Done()
					// 每个调用方提交自己独有的 summary，用来验证结果没有串味。
					want := fmt.Sprintf("summary from caller %d", i)
					args := fmt.Sprintf(`{"summary":%q,"findings":[
						{"file":"config.go","anchor":"return nil, err","severity":"info","summary":"note"}
					]}`, want)

					got := shared.Execute(context.Background(), "/repo", json.RawMessage(args))
					switch {
					case got.IsError:
						errs[i] = fmt.Errorf("caller %d: Execute failed: %s", i, got.Output)
					case got.ReviewResult == nil:
						errs[i] = fmt.Errorf("caller %d: ReviewResult is nil", i)
					case got.ReviewResult.Summary != want:
						errs[i] = fmt.Errorf("caller %d: Summary = %q, want %q (results leaked between callers)",
							i, got.ReviewResult.Summary, want)
					case got.ReviewResult.Findings[0].StartLine != 13:
						errs[i] = fmt.Errorf("caller %d: StartLine = %d, want 13",
							i, got.ReviewResult.Findings[0].StartLine)
					}
				}()
			}
			wg.Wait()

			for _, err := range errs {
				if err != nil {
					t.Error(err)
				}
			}
		})
	}
}

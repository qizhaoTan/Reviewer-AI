package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/review"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
	"github.com/qizhaoTan/Reviewer-AI/internal/store"
)

// fakeSource 是 RunSource 的测试替身：直接吐出预置数据或预置错误，
// 不碰 SQLite——这个包测的是 handler 行为，不是持久化。
type fakeSource struct {
	runs    []store.Run
	listErr error
	loadErr error

	// deleteErr 让测试模拟持久化层失败；deleted 记录 handler 实际请求删除的 ID，
	// 用来断言"按钮上的 ID 确实传到了 store"。用指针是因为 fakeSource 按值传递。
	deleteErr error
	deleted   *[]string

	// deleteAllErr 让测试模拟清空失败；deleteAllCalls 记录端点被调了几次。
	deleteAllErr   error
	deleteAllCalls *int
}

func (f fakeSource) ListAllRuns(_ context.Context, limit int) ([]store.Run, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if limit > 0 && limit < len(f.runs) {
		return f.runs[:limit], nil
	}
	return f.runs, nil
}

func (f fakeSource) LoadRun(_ context.Context, id string) (*store.Run, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	for i := range f.runs {
		if f.runs[i].ID == id {
			return &f.runs[i], nil
		}
	}
	return nil, nil
}

func (f fakeSource) DeleteRun(_ context.Context, id string) (bool, error) {
	if f.deleteErr != nil {
		return false, f.deleteErr
	}
	if f.deleted != nil {
		*f.deleted = append(*f.deleted, id)
	}
	for i := range f.runs {
		if f.runs[i].ID == id {
			return true, nil
		}
	}
	return false, nil
}

func (f fakeSource) DeleteAllRuns(_ context.Context) (int64, error) {
	if f.deleteAllCalls != nil {
		*f.deleteAllCalls++
	}
	if f.deleteAllErr != nil {
		return 0, f.deleteAllErr
	}
	return int64(len(f.runs)), nil
}

func get(t *testing.T, source RunSource, target string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(t, source, nil, httptest.NewRequest(http.MethodGet, target, nil))
}

// serve 把一个请求打进装配好的 handler。reviewer 为 nil 表示只读部署。
func serve(t *testing.T, source RunSource, reviewer Reviewer, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	NewServer(source, reviewer, ":0").Handler().ServeHTTP(rec, req)
	return rec
}

// postForm 构造一个表单 POST 请求。
func postForm(target string, values url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// fakeReviewer 是 Reviewer 的测试替身：记录收到的参数，按预置结果作答。
type fakeReviewer struct {
	replyErr    error
	rereviewErr error
	// newRunID 是 Rereview 成功时返回的新记录 ID。
	newRunID string

	// 收到的参数，供断言。
	gotRunID     string
	gotFindingID string
	gotReply     string
	replyCalls   int
	rereviewCall int
}

func (f *fakeReviewer) Reply(_ context.Context, runID, findingID, userReply string) (*store.Run, error) {
	f.replyCalls++
	f.gotRunID, f.gotFindingID, f.gotReply = runID, findingID, userReply
	if f.replyErr != nil {
		return nil, f.replyErr
	}
	return &store.Run{ID: runID}, nil
}

func (f *fakeReviewer) Rereview(_ context.Context, runID string) (*store.Run, error) {
	f.rereviewCall++
	f.gotRunID = runID
	if f.rereviewErr != nil {
		return nil, f.rereviewErr
	}
	return &store.Run{ID: f.newRunID}, nil
}

func TestHandleIndex(t *testing.T) {
	critiqued := store.Run{
		ID:        "run-critiqued",
		RepoPath:  "/repo/alpha#main",
		Status:    store.StatusCompleted,
		UpdatedAt: time.Date(2026, 8, 17, 10, 30, 0, 0, time.UTC),
		Snapshot:  []gitdiff.Change{{Path: "a.go"}, {Path: "b.go"}},
		Critiqued: true,
		Findings: []review.Finding{
			{ID: "f1", Kept: true},
			{ID: "f2", Kept: false},
			{ID: "f3", Kept: false},
		},
	}
	pending := store.Run{
		ID:        "run-pending",
		RepoPath:  "/repo/beta#feature/x",
		Status:    store.StatusInProgress,
		UpdatedAt: time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC),
		Critiqued: false,
		Findings:  []review.Finding{{ID: "f1"}, {ID: "f2"}},
	}

	tests := []struct {
		name        string
		source      fakeSource
		wantCode    int
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:     "renders repo, branch and kept/dropped counts",
			source:   fakeSource{runs: []store.Run{critiqued}},
			wantCode: http.StatusOK,
			wantContain: []string{
				"/repo/alpha", "main", "2026-08-17 10:30:00",
				"completed", `href="/run?id=run-critiqued"`,
			},
		},
		{
			name:     "un-critiqued run counts all findings as kept, not dropped",
			source:   fakeSource{runs: []store.Run{pending}},
			wantCode: http.StatusOK,
			// 复核没跑过时不能显示成"全被砍了"，丢弃列应是"未复核"占位。
			wantContain: []string{"未复核", "feature/x"},
		},
		{
			name:        "empty database shows placeholder instead of blank table",
			source:      fakeSource{},
			wantCode:    http.StatusOK,
			wantContain: []string{"还没有任何审查记录"},
			wantAbsent:  []string{"<tbody>"},
		},
		{
			name:        "source error surfaces as 500",
			source:      fakeSource{listErr: errors.New("db exploded")},
			wantCode:    http.StatusInternalServerError,
			wantContain: []string{"db exploded"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := get(t, tt.source, "/")
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.wantCode, rec.Body.String())
			}
			body := rec.Body.String()
			for _, want := range tt.wantContain {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q\nbody: %s", want, body)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(body, absent) {
					t.Errorf("body unexpectedly contains %q", absent)
				}
			}
		})
	}
}

func TestHandleIndexCountsFindings(t *testing.T) {
	tests := []struct {
		name        string
		run         store.Run
		wantKept    int
		wantDropped int
	}{
		{
			name: "critiqued splits by Kept",
			run: store.Run{Critiqued: true, Findings: []review.Finding{
				{Kept: true}, {Kept: true}, {Kept: false},
			}},
			wantKept: 2, wantDropped: 1,
		},
		{
			// 这是 countFindings 存在的理由：复核没跑时 Kept 全是零值，
			// 若按 Kept 分组会把所有意见误报成"被丢弃"。
			name:     "not critiqued treats every finding as kept",
			run:      store.Run{Critiqued: false, Findings: []review.Finding{{}, {}, {}}},
			wantKept: 3, wantDropped: 0,
		},
		{
			name:     "no findings",
			run:      store.Run{Critiqued: true},
			wantKept: 0, wantDropped: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kept, dropped := countFindings(tt.run)
			if kept != tt.wantKept || dropped != tt.wantDropped {
				t.Errorf("countFindings = (%d, %d), want (%d, %d)", kept, dropped, tt.wantKept, tt.wantDropped)
			}
		})
	}
}

func TestHandleIndexRejectsUnknownPath(t *testing.T) {
	tests := []struct {
		name   string
		target string
	}{
		{name: "unknown path 404s instead of showing the list", target: "/nope"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := get(t, fakeSource{}, tt.target)
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
		})
	}
}

func TestHandleRun(t *testing.T) {
	full := store.Run{
		ID:        "run-1",
		RepoPath:  "/repo/alpha#main",
		Status:    store.StatusCompleted,
		CreatedAt: time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 8, 17, 10, 30, 0, 0, time.UTC),
		Snapshot:  []gitdiff.Change{{Path: "a.go"}},
		Critiqued: true,
		Summary:   "整体还行",
		Findings: []review.Finding{
			{
				ID: "f1", File: "a.go", StartLine: 12, EndLine: 14,
				Severity: review.SeverityError, Summary: "空指针风险",
				Detail: "调用前没判空", Anchor: "if x != nil {",
				Kept: true, CritiqueReason: "确实成立",
			},
			{
				ID: "f2", File: "a.go", Severity: review.SeverityInfo,
				Summary: "过度解读的意见", Kept: false,
				CritiqueReason: "这是既有代码风格，不构成问题",
			},
		},
		Messages: []schema.Message{
			{Role: schema.RoleSystem, Content: "you are a reviewer"},
			{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{
				{ID: "tc-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.go"}`)},
			}},
			{Role: schema.RoleUser, ToolCallID: "tc-1", Content: "<tool result omitted>"},
		},
	}

	tests := []struct {
		name        string
		source      fakeSource
		target      string
		wantCode    int
		wantContain []string
	}{
		{
			name:     "renders metadata, both finding groups and message history",
			source:   fakeSource{runs: []store.Run{full}},
			target:   "/run?id=run-1",
			wantCode: http.StatusOK,
			wantContain: []string{
				"run-1", "/repo/alpha", "main", "整体还行",
				"a.go:12-14",       // 行号由 lineRange 渲染
				"ERROR",            // severity 徽标
				"空指针风险", "过度解读的意见", // 保留组与丢弃组都要出现
				"这是既有代码风格，不构成问题", // 丢弃理由必须可见，这是本页的主要用途
				"if x != nil {",     // anchor
				"read_file", "tc-1", // 工具调用往返
				`{&#34;path&#34;:&#34;a.go&#34;}`, // 参数经过 HTML 转义后仍完整
			},
		},
		{
			name:        "missing id is a 400",
			source:      fakeSource{runs: []store.Run{full}},
			target:      "/run",
			wantCode:    http.StatusBadRequest,
			wantContain: []string{"缺少运行记录 id"},
		},
		{
			name:        "unknown id is a 404",
			source:      fakeSource{runs: []store.Run{full}},
			target:      "/run?id=nope",
			wantCode:    http.StatusNotFound,
			wantContain: []string{"nope"},
		},
		{
			name:        "source error surfaces as 500",
			source:      fakeSource{loadErr: errors.New("db exploded")},
			target:      "/run?id=run-1",
			wantCode:    http.StatusInternalServerError,
			wantContain: []string{"db exploded"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := get(t, tt.source, tt.target)
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.wantCode, rec.Body.String())
			}
			body := rec.Body.String()
			for _, want := range tt.wantContain {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q\nbody: %s", want, body)
				}
			}
		})
	}
}

func TestHandleRunEscapesUntrustedContent(t *testing.T) {
	// Finding 的文本全部来自大模型，必须走 html/template 的自动转义。
	// 这条用例防止有人以后为了"渲染 Markdown"把它换成 template.HTML。
	tests := []struct {
		name       string
		summary    string
		wantEscape string
		wantAbsent string
	}{
		{
			name:       "script tag in model output is escaped",
			summary:    `<script>alert(1)</script>`,
			wantEscape: "&lt;script&gt;",
			wantAbsent: "<script>alert(1)</script>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := fakeSource{runs: []store.Run{{
				ID: "x", Critiqued: true,
				Findings: []review.Finding{{ID: "f1", File: "a.go", Kept: true, Summary: tt.summary}},
			}}}
			rec := get(t, src, "/run?id=x")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tt.wantEscape) {
				t.Errorf("body missing escaped form %q", tt.wantEscape)
			}
			if strings.Contains(body, tt.wantAbsent) {
				t.Errorf("body contains unescaped %q", tt.wantAbsent)
			}
		})
	}
}

func TestSplitRunKey(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		wantRepo   string
		wantBranch string
	}{
		{name: "path and branch", key: "/repo/alpha#main", wantRepo: "/repo/alpha", wantBranch: "main"},
		{name: "branch containing slash", key: "/repo/alpha#feature/x", wantRepo: "/repo/alpha", wantBranch: "feature/x"},
		{
			// 旧记录存的是裸路径，不该因为 key 格式变了就在页面上显示不出来。
			name: "legacy key without separator", key: "/repo/alpha", wantRepo: "/repo/alpha", wantBranch: "",
		},
		{
			// 路径里本身带 # 时，以最后一个分隔符为准。
			name: "path containing hash", key: "/repo/a#b#main", wantRepo: "/repo/a#b", wantBranch: "main",
		},
		{name: "empty key", key: "", wantRepo: "", wantBranch: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, branch := splitRunKey(tt.key)
			if repo != tt.wantRepo || branch != tt.wantBranch {
				t.Errorf("splitRunKey(%q) = (%q, %q), want (%q, %q)", tt.key, repo, branch, tt.wantRepo, tt.wantBranch)
			}
		})
	}
}

func TestHandleRunCollapseState(t *testing.T) {
	run := store.Run{
		ID: "run-1", Critiqued: true,
		Findings: []review.Finding{
			{ID: "f1", File: "a.go", Severity: review.SeverityError, Summary: "保留的", Kept: true, Detail: "细节"},
			{ID: "f2", File: "a.go", Severity: review.SeverityInfo, Summary: "丢弃的", Kept: false, CritiqueReason: "站不住"},
		},
		Messages: []schema.Message{{Role: schema.RoleSystem, Content: "you are a reviewer"}},
	}
	body := get(t, fakeSource{runs: []store.Run{run}}, "/run?id=run-1").Body.String()

	// 分级折叠：只有"保留的意见"带 open,另外两个区块默认收起。
	// 断言 open 的出现次数,而不只是"包含 open"——后者在三个区块全都
	// 展开时同样成立,测不出任何东西。
	tests := []struct {
		name      string
		substr    string
		wantCount int
	}{
		{name: "exactly one section is open by default", substr: `<details class="section" open>`, wantCount: 1},
		{name: "the other two sections are collapsed", substr: `<details class="section">`, wantCount: 2},
		{name: "finding details are collapsed", substr: `<details class="fdetails">`, wantCount: 2},
		{name: "each message is individually collapsed", substr: `<details class="msg">`, wantCount: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := strings.Count(body, tt.substr); got != tt.wantCount {
				t.Errorf("count(%q) = %d, want %d\nbody: %s", tt.substr, got, tt.wantCount, body)
			}
		})
	}

	t.Run("kept findings section is the open one", func(t *testing.T) {
		openIdx := strings.Index(body, `<details class="section" open>`)
		keptIdx := strings.Index(body, "保留的意见")
		droppedIdx := strings.Index(body, "被复核丢弃的意见")
		if !(openIdx < keptIdx && keptIdx < droppedIdx) {
			t.Errorf("展开的区块应是「保留的意见」，位置: open=%d kept=%d dropped=%d", openIdx, keptIdx, droppedIdx)
		}
	})

	t.Run("collapsed content is still present in the HTML", func(t *testing.T) {
		// <details> 只是视觉折叠，内容仍在 DOM 里——浏览器的页内搜索能找到，
		// 也不需要任何请求就能展开。这条防止有人把折叠改成"点开再异步拉取"。
		for _, want := range []string{"丢弃的", "站不住", "you are a reviewer"} {
			if !strings.Contains(body, want) {
				t.Errorf("折叠区块的内容 %q 应当仍在 HTML 中", want)
			}
		}
	})
}

func TestFindingWithoutDetailsRendersNoToggle(t *testing.T) {
	// detail/anchor/复核理由 三者皆空时不该渲染折叠框，否则点开是空的。
	tests := []struct {
		name       string
		finding    review.Finding
		wantToggle bool
	}{
		{
			name:       "bare finding has no toggle",
			finding:    review.Finding{ID: "f1", File: "a.go", Summary: "就一句话", Kept: true},
			wantToggle: false,
		},
		{
			name:       "finding with detail has a toggle",
			finding:    review.Finding{ID: "f1", File: "a.go", Summary: "有细节", Detail: "展开看", Kept: true},
			wantToggle: true,
		},
		{
			name:       "finding with only a critique reason has a toggle",
			finding:    review.Finding{ID: "f1", File: "a.go", Summary: "有理由", CritiqueReason: "成立", Kept: true},
			wantToggle: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := fakeSource{runs: []store.Run{{ID: "x", Critiqued: true, Findings: []review.Finding{tt.finding}}}}
			body := get(t, src, "/run?id=x").Body.String()
			if got := strings.Contains(body, `<details class="fdetails">`); got != tt.wantToggle {
				t.Errorf("存在细节折叠框 = %v, want %v", got, tt.wantToggle)
			}
			// 摘要无论如何都要常显——这是"分级折叠"的前提。
			if !strings.Contains(body, tt.finding.Summary) {
				t.Errorf("摘要 %q 应当常显", tt.finding.Summary)
			}
		})
	}
}

func TestPeek(t *testing.T) {
	peek := tmplFuncs["peek"].(func(string) string)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "short content passes through", in: "hello", want: "hello"},
		{name: "newlines collapse to spaces so the summary stays one line", in: "a\nb\n\nc", want: "a b c"},
		{name: "surrounding whitespace trimmed", in: "  hi  ", want: "hi"},
		{name: "empty stays empty", in: "", want: ""},
		{
			// 按 rune 截断：按 byte 切会把汉字劈成半个，输出乱码。
			name: "multibyte truncation does not split a character",
			in:   strings.Repeat("中", 100),
			want: strings.Repeat("中", peekMaxRunes) + "…",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := peek(tt.in); got != tt.want {
				t.Errorf("peek(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestToStringHandlesNamedStringTypes(t *testing.T) {
	// store.RunStatus / review.Severity 都是具名 string 类型，模板函数拿到的是
	// any。用 `case string` 断言会匹配失败并静默返回空串——徽标于是既没文字
	// 也没颜色，页面不报错，只是"看起来没样式"。这组用例盯住这个静默失败。
	tests := []struct {
		name string
		in   any
		want string
	}{
		{name: "plain string", in: "info", want: "info"},
		{name: "named severity type", in: review.SeverityError, want: "error"},
		{name: "named run status type", in: store.StatusInProgress, want: "in_progress"},
		{name: "non-string yields empty", in: 42, want: ""},
		{name: "nil yields empty", in: nil, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toString(tt.in); got != tt.want {
				t.Errorf("toString(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestLineRange(t *testing.T) {
	lineRange := tmplFuncs["lineRange"].(func(int, int) string)
	tests := []struct {
		name       string
		start, end int
		want       string
	}{
		{name: "range", start: 12, end: 14, want: ":12-14"},
		{name: "single line when end equals start", start: 12, end: 12, want: ":12"},
		{name: "single line when end is zero", start: 12, end: 0, want: ":12"},
		{name: "unresolved anchor renders nothing", start: 0, end: 0, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lineRange(tt.start, tt.end); got != tt.want {
				t.Errorf("lineRange(%d, %d) = %q, want %q", tt.start, tt.end, got, tt.want)
			}
		})
	}
}

func TestHandleReply(t *testing.T) {
	run := store.Run{
		ID: "run-1", RepoPath: "/repo#main", Status: store.StatusCompleted, Critiqued: true,
		Findings: []review.Finding{{ID: "f1", File: "a.go", Severity: review.SeverityError, Summary: "boom", Kept: true}},
	}
	source := fakeSource{runs: []store.Run{run}}

	tests := []struct {
		name     string
		form     url.Values
		replyErr error
		wantCode int
		// wantLocation 非空时断言重定向目标包含这些片段。
		wantLocation []string
		wantCalls    int
	}{
		{
			name:         "a valid reply reaches the reviewer and redirects back",
			form:         url.Values{"run": {"run-1"}, "finding": {"f1"}, "reply": {"我认为不成立"}},
			wantCode:     http.StatusSeeOther,
			wantLocation: []string{"/run?", "id=run-1", "notice="},
			wantCalls:    1,
		},
		{
			// 失败要带着原因回到详情页，而不是甩一个白屏 500——用户刚写完
			// 一段话，至少得知道它为什么没被接受。
			name:         "a reviewer failure comes back as a message not a 500",
			form:         url.Values{"run": {"run-1"}, "finding": {"f1"}, "reply": {"异议"}},
			replyErr:     errors.New("upstream 503"),
			wantCode:     http.StatusSeeOther,
			wantLocation: []string{"error=", "upstream+503"},
			wantCalls:    1,
		},
		{
			// 空回复不该浪费一次模型调用。
			name:         "an empty reply never reaches the reviewer",
			form:         url.Values{"run": {"run-1"}, "finding": {"f1"}, "reply": {"   "}},
			wantCode:     http.StatusSeeOther,
			wantLocation: []string{"error="},
			wantCalls:    0,
		},
		{
			name:      "a missing run id is a bad request",
			form:      url.Values{"finding": {"f1"}, "reply": {"异议"}},
			wantCode:  http.StatusBadRequest,
			wantCalls: 0,
		},
		{
			name:      "a missing finding id is a bad request",
			form:      url.Values{"run": {"run-1"}, "reply": {"异议"}},
			wantCode:  http.StatusBadRequest,
			wantCalls: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reviewer := &fakeReviewer{replyErr: tt.replyErr}
			rec := serve(t, source, reviewer, postForm("/reply", tt.form))

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if reviewer.replyCalls != tt.wantCalls {
				t.Errorf("Reply called %d times, want %d", reviewer.replyCalls, tt.wantCalls)
			}
			for _, want := range tt.wantLocation {
				if got := rec.Header().Get("Location"); !strings.Contains(got, want) {
					t.Errorf("Location %q does not contain %q", got, want)
				}
			}
			if tt.wantCalls > 0 && reviewer.gotFindingID != "f1" {
				t.Errorf("finding id passed through = %q, want %q", reviewer.gotFindingID, "f1")
			}
		})
	}
}

// TestHandleReplyRejectsGet 固定住"这些端点只接受 POST"。
// GET 能触发的话，一个预取链接的浏览器插件就能自作主张地花掉几次模型调用。
func TestHandleReplyRejectsGet(t *testing.T) {
	tests := []struct {
		name   string
		target string
	}{
		{name: "reply", target: "/reply?run=run-1&finding=f1&reply=x"},
		{name: "rereview", target: "/rereview?run=run-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reviewer := &fakeReviewer{}
			rec := serve(t, fakeSource{}, reviewer, httptest.NewRequest(http.MethodGet, tt.target, nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
			if reviewer.replyCalls+reviewer.rereviewCall != 0 {
				t.Error("a GET reached the reviewer; these endpoints must be POST-only")
			}
		})
	}
}

// TestInteractiveEndpointsWithoutReviewer 固定住只读部署的行为：交互端点
// 返回 503，而浏览页面照常可用——配置里少个 API key 不该让整个查看服务瘫掉。
func TestInteractiveEndpointsWithoutReviewer(t *testing.T) {
	run := store.Run{ID: "run-1", RepoPath: "/repo#main", Status: store.StatusCompleted}
	source := fakeSource{runs: []store.Run{run}}

	tests := []struct {
		name     string
		target   string
		form     url.Values
		wantCode int
	}{
		{name: "reply is unavailable", target: "/reply", form: url.Values{"run": {"run-1"}, "finding": {"f1"}, "reply": {"x"}}, wantCode: http.StatusServiceUnavailable},
		{name: "rereview is unavailable", target: "/rereview", form: url.Values{"run": {"run-1"}}, wantCode: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, source, nil, postForm(tt.target, tt.form))
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
		})
	}

	t.Run("the detail page still renders and hides the forms", func(t *testing.T) {
		rec := get(t, source, "/run?id=run-1")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if strings.Contains(rec.Body.String(), `action="/reply"`) {
			t.Error("the reply form was rendered without a reviewer; clicking it would always fail")
		}
		if strings.Contains(rec.Body.String(), `action="/rereview"`) {
			t.Error("the rereview button was rendered without a reviewer")
		}
	})
}

func TestHandleRereview(t *testing.T) {
	run := store.Run{ID: "run-1", RepoPath: "/repo#main", Status: store.StatusCompleted}
	source := fakeSource{runs: []store.Run{run}}

	tests := []struct {
		name         string
		rereviewErr  error
		newRunID     string
		wantLocation []string
	}{
		{
			// 成功后要跳到**新**记录：用户要看的是重审之后的结果。
			name:         "success redirects to the newly created run",
			newRunID:     "run-2",
			wantLocation: []string{"id=run-2", "notice="},
		},
		{
			// 失败留在原记录并说明原因。
			name:         "failure redirects back to the original run with the reason",
			rereviewErr:  errors.New("nothing is staged"),
			wantLocation: []string{"id=run-1", "error=", "nothing+is+staged"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reviewer := &fakeReviewer{rereviewErr: tt.rereviewErr, newRunID: tt.newRunID}
			rec := serve(t, source, reviewer, postForm("/rereview", url.Values{"run": {"run-1"}}))

			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusSeeOther, rec.Body.String())
			}
			if reviewer.rereviewCall != 1 {
				t.Errorf("Rereview called %d times, want 1", reviewer.rereviewCall)
			}
			for _, want := range tt.wantLocation {
				if got := rec.Header().Get("Location"); !strings.Contains(got, want) {
					t.Errorf("Location %q does not contain %q", got, want)
				}
			}
		})
	}
}

func TestRunPageRendersInteractiveElements(t *testing.T) {
	withdrawn := review.Finding{
		ID: "f2", File: "b.go", Severity: review.SeverityWarning, Summary: "作者已说服撤回",
		Kept: true, Status: review.StatusWithdrawn,
		Discussion: []schema.Message{
			{Role: schema.RoleUser, Content: "调用方已经处理了"},
			{Role: schema.RoleAssistant, Content: "确实如此，撤回"},
		},
	}
	run := store.Run{
		ID: "run-1", RepoPath: "/repo#main", Status: store.StatusCompleted, Critiqued: true,
		ParentRunID: "run-0",
		Findings: []review.Finding{
			{ID: "f1", File: "a.go", Severity: review.SeverityError, Summary: "还站得住", Kept: true},
			withdrawn,
			{ID: "f3", File: "c.go", Severity: review.SeverityInfo, Summary: "复核砍掉的", Kept: false, CritiqueReason: "过度解读"},
		},
	}

	tests := []struct {
		name         string
		wantContains []string
		wantExcludes []string
		// wantCount 断言片段出现的确切次数。
		substr    string
		wantCount int
	}{
		{
			name: "withdrawn finding is marked and its discussion is shown",
			wantContains: []string{
				"已撤回", "finding withdrawn",
				"调用方已经处理了", "确实如此，撤回",
				"讨论记录",
			},
		},
		{
			name:         "the parent run is linked so the re-review chain is navigable",
			wantContains: []string{`href="/run?id=run-0"`},
		},
		{
			// 回复框只给"未撤回且通过复核"的那一条：撤回过的再争论没意义，
			// 复核砍掉的那组根本不该出现表单。
			name:      "a reply form is rendered only for the one open kept finding",
			substr:    `action="/reply"`,
			wantCount: 1,
		},
		{
			name:         "the rereview button is rendered",
			wantContains: []string{`action="/rereview"`},
		},
	}

	source := fakeSource{runs: []store.Run{run}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, source, &fakeReviewer{}, httptest.NewRequest(http.MethodGet, "/run?id=run-1", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			body := rec.Body.String()

			for _, want := range tt.wantContains {
				if !strings.Contains(body, want) {
					t.Errorf("body does not contain %q", want)
				}
			}
			for _, unwanted := range tt.wantExcludes {
				if strings.Contains(body, unwanted) {
					t.Errorf("body unexpectedly contains %q", unwanted)
				}
			}
			if tt.substr != "" {
				if got := strings.Count(body, tt.substr); got != tt.wantCount {
					t.Errorf("body contains %q %d times, want %d", tt.substr, got, tt.wantCount)
				}
			}
		})
	}
}

// TestReplyBannerIsEscaped 固定住提示语走的是 html/template 的自动转义。
// notice/error 是从查询参数原样带回来的，任何人都能构造一个链接。
func TestReplyBannerIsEscaped(t *testing.T) {
	run := store.Run{ID: "run-1", RepoPath: "/repo#main", Status: store.StatusCompleted}
	source := fakeSource{runs: []store.Run{run}}

	tests := []struct {
		name   string
		target string
	}{
		{name: "notice", target: "/run?id=run-1&notice=%3Cscript%3Ealert(1)%3C/script%3E"},
		{name: "error", target: "/run?id=run-1&error=%3Cimg+src%3Dx+onerror%3Dalert(1)%3E"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := get(t, source, tt.target)
			body := rec.Body.String()
			if strings.Contains(body, "<script>alert(1)</script>") || strings.Contains(body, "<img src=x onerror=") {
				t.Error("query-parameter content reached the page unescaped")
			}
		})
	}
}

// TestHandleDelete 覆盖删除端点：正常删除、记录不存在、持久化失败、
// 缺 run 参数、以及 GET 不该生效。
func TestHandleDelete(t *testing.T) {
	run := store.Run{ID: "run-1", RepoPath: "/repo#main", Status: store.StatusCompleted}

	tests := []struct {
		name      string
		method    string
		form      url.Values
		deleteErr error
		wantCode  int
		// wantLocation 非空时断言重定向目标包含这些片段。
		wantLocation []string
		wantDeleted  []string // 实际传到 store 的 ID
	}{
		{
			name:         "a valid delete removes the run and returns to the index",
			method:       http.MethodPost,
			form:         url.Values{"run": {"run-1"}},
			wantCode:     http.StatusSeeOther,
			wantLocation: []string{"/?", "notice="},
			wantDeleted:  []string{"run-1"},
		},
		{
			// 两个标签页各点一次删除时会走到这里。记录已经不在，用户想要的
			// 结果已经达成，所以不是错误状态码，只是提示措辞要说实话。
			name:         "deleting a missing run reports it without failing",
			method:       http.MethodPost,
			form:         url.Values{"run": {"nope"}},
			wantCode:     http.StatusSeeOther,
			wantLocation: []string{"/?", "error="},
			wantDeleted:  []string{"nope"},
		},
		{
			name:         "a store failure comes back as a message not a 500",
			method:       http.MethodPost,
			form:         url.Values{"run": {"run-1"}},
			deleteErr:    errors.New("database is locked"),
			wantCode:     http.StatusSeeOther,
			wantLocation: []string{"error=", "database+is+locked"},
		},
		{
			name:     "a missing run id is a bad request",
			method:   http.MethodPost,
			form:     url.Values{},
			wantCode: http.StatusBadRequest,
		},
		{
			// 删除是破坏性的，绝不能被一个预取链接的浏览器插件触发。
			name:     "a GET never deletes anything",
			method:   http.MethodGet,
			form:     url.Values{"run": {"run-1"}},
			wantCode: http.StatusMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var deleted []string
			source := fakeSource{runs: []store.Run{run}, deleteErr: tt.deleteErr, deleted: &deleted}

			var req *http.Request
			if tt.method == http.MethodPost {
				req = postForm("/delete", tt.form)
			} else {
				req = httptest.NewRequest(tt.method, "/delete?"+tt.form.Encode(), nil)
			}
			rec := serve(t, source, nil, req)

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.wantCode, rec.Body.String())
			}
			for _, want := range tt.wantLocation {
				if got := rec.Header().Get("Location"); !strings.Contains(got, want) {
					t.Errorf("Location %q does not contain %q", got, want)
				}
			}
			if strings.Join(deleted, ",") != strings.Join(tt.wantDeleted, ",") {
				t.Errorf("deleted ids = %v, want %v", deleted, tt.wantDeleted)
			}
		})
	}
}

// TestHandleDeleteWorksWithoutReviewer 固定住"删除不依赖模型"这条：
// 配置里没有 API key 时 reviewer 为 nil，而清理一条跑坏的记录恰恰是
// 这种时候最想做的事，不能跟着 503。
func TestHandleDeleteWorksWithoutReviewer(t *testing.T) {
	tests := []struct{ name string }{{name: "read-only deployment can still delete"}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var deleted []string
			source := fakeSource{
				runs:    []store.Run{{ID: "run-1", RepoPath: "/repo#main", Status: store.StatusCompleted}},
				deleted: &deleted,
			}
			rec := serve(t, source, nil, postForm("/delete", url.Values{"run": {"run-1"}}))

			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
			}
			if len(deleted) != 1 || deleted[0] != "run-1" {
				t.Errorf("deleted = %v, want [run-1]", deleted)
			}
		})
	}
}

// TestIndexRendersDeleteControls 固定住列表页上删除入口的存在与形态：
// 必须是 POST 表单（GET 会被预取）、必须带上这一行的 run id、必须有二次确认。
func TestIndexRendersDeleteControls(t *testing.T) {
	tests := []struct {
		name     string
		runs     []store.Run
		wantHTML []string
		skipHTML []string
	}{
		{
			name: "each row carries a confirming delete form",
			runs: []store.Run{{ID: "run-1", RepoPath: "/repo#main", Status: store.StatusCompleted}},
			wantHTML: []string{
				`method="post"`,
				`action="/delete"`,
				`name="run" value="run-1"`,
				"confirm(",
			},
		},
		{
			// 一条记录都没有时不该渲染出任何删除表单。
			name:     "an empty index has no delete form",
			runs:     nil,
			skipHTML: []string{`action="/delete"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := get(t, fakeSource{runs: tt.runs}, "/")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			for _, want := range tt.wantHTML {
				if !strings.Contains(body, want) {
					t.Errorf("index page missing %q", want)
				}
			}
			for _, skip := range tt.skipHTML {
				if strings.Contains(body, skip) {
					t.Errorf("index page unexpectedly contains %q", skip)
				}
			}
		})
	}
}

// TestIndexBannerIsEscaped 固定住列表页提示的转义：notice/error 来自查询
// 参数，任何人都能构造，直接插进 HTML 就是一个反射型 XSS。
func TestIndexBannerIsEscaped(t *testing.T) {
	tests := []struct {
		name   string
		target string
	}{
		{name: "notice", target: "/?notice=%3Cscript%3Ealert(1)%3C%2Fscript%3E"},
		{name: "error", target: "/?error=%3Cscript%3Ealert(1)%3C%2Fscript%3E"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := get(t, fakeSource{}, tt.target)
			body := rec.Body.String()
			if strings.Contains(body, "<script>alert(1)</script>") {
				t.Error("banner rendered raw HTML from a query parameter")
			}
			if !strings.Contains(body, "&lt;script&gt;") {
				t.Errorf("escaped banner text not found in body: %s", body)
			}
		})
	}
}

// TestHandleDeleteAll 覆盖清空端点：正常清空、库本来就空、持久化失败、
// 以及 GET 不该生效。
func TestHandleDeleteAll(t *testing.T) {
	tests := []struct {
		name         string
		method       string
		runs         []store.Run
		deleteAllErr error
		wantCode     int
		wantLocation []string
		wantCalls    int
	}{
		{
			name:   "clearing a populated store reports how many went",
			method: http.MethodPost,
			runs: []store.Run{
				{ID: "run-1", RepoPath: "/repo#main"},
				{ID: "run-2", RepoPath: "/repo#dev"},
			},
			wantCode: http.StatusSeeOther,
			// 条数要出现在提示里：用户点完得知道实际删掉了多少。
			wantLocation: []string{"/?", "notice=", "2"},
			wantCalls:    1,
		},
		{
			// 库本来就是空的不算错误——用户想要的"清空"状态已经达成。
			name:         "clearing an empty store is not an error",
			method:       http.MethodPost,
			runs:         nil,
			wantCode:     http.StatusSeeOther,
			wantLocation: []string{"/?", "notice="},
			wantCalls:    1,
		},
		{
			name:         "a store failure comes back as a message not a 500",
			method:       http.MethodPost,
			runs:         []store.Run{{ID: "run-1", RepoPath: "/repo#main"}},
			deleteAllErr: errors.New("database is locked"),
			wantCode:     http.StatusSeeOther,
			wantLocation: []string{"error=", "database+is+locked"},
			wantCalls:    1,
		},
		{
			// 清空是这个服务里破坏力最大的操作，绝不能被一个预取链接触发。
			name:      "a GET never clears anything",
			method:    http.MethodGet,
			runs:      []store.Run{{ID: "run-1", RepoPath: "/repo#main"}},
			wantCode:  http.StatusMethodNotAllowed,
			wantCalls: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			source := fakeSource{runs: tt.runs, deleteAllErr: tt.deleteAllErr, deleteAllCalls: &calls}

			var req *http.Request
			if tt.method == http.MethodPost {
				req = postForm("/delete-all", url.Values{})
			} else {
				req = httptest.NewRequest(tt.method, "/delete-all", nil)
			}
			rec := serve(t, source, nil, req)

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if calls != tt.wantCalls {
				t.Errorf("DeleteAllRuns called %d times, want %d", calls, tt.wantCalls)
			}
			for _, want := range tt.wantLocation {
				if got := rec.Header().Get("Location"); !strings.Contains(got, want) {
					t.Errorf("Location %q does not contain %q", got, want)
				}
			}
		})
	}
}

// TestDeleteEndpointsAreDistinct 固定住"单条删除"和"清空全部"是两个端点：
// 让 /delete 靠"run 参数为空"表示全删的话，一个漏传参数的表单就会从删一条
// 静默变成清空整个库。这里断言 /delete 缺参数是 400，且没有碰到清空逻辑。
func TestDeleteEndpointsAreDistinct(t *testing.T) {
	tests := []struct{ name string }{{name: "a delete without a run id never clears the store"}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			var deleted []string
			source := fakeSource{
				runs:           []store.Run{{ID: "run-1", RepoPath: "/repo#main"}},
				deleted:        &deleted,
				deleteAllCalls: &calls,
			}
			rec := serve(t, source, nil, postForm("/delete", url.Values{}))

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if calls != 0 {
				t.Error("/delete without a run id reached DeleteAllRuns")
			}
			if len(deleted) != 0 {
				t.Errorf("/delete without a run id deleted %v", deleted)
			}
		})
	}
}

// TestIndexRendersDeleteAllControl 固定住清空入口的形态：POST 表单、
// 确认文案里带上条数。
func TestIndexRendersDeleteAllControl(t *testing.T) {
	tests := []struct {
		name     string
		runs     []store.Run
		wantHTML []string
		skipHTML []string
	}{
		{
			name: "a populated index offers a confirming clear-all form",
			runs: []store.Run{
				{ID: "run-1", RepoPath: "/repo#main"},
				{ID: "run-2", RepoPath: "/repo#dev"},
			},
			wantHTML: []string{`action="/delete-all"`, `method="post"`, "confirm(", "2 条记录"},
		},
		{
			// 一条记录都没有时不该渲染出一个点了什么都不会发生的清空按钮。
			name:     "an empty index has no clear-all form",
			runs:     nil,
			skipHTML: []string{`action="/delete-all"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := get(t, fakeSource{runs: tt.runs}, "/")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			for _, want := range tt.wantHTML {
				if !strings.Contains(body, want) {
					t.Errorf("index page missing %q", want)
				}
			}
			for _, skip := range tt.skipHTML {
				if strings.Contains(body, skip) {
					t.Errorf("index page unexpectedly contains %q", skip)
				}
			}
		})
	}
}

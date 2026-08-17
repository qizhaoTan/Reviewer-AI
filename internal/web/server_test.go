package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

func get(t *testing.T, source RunSource, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	NewServer(source, ":0").Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
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

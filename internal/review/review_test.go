package review

import (
	"strings"
	"testing"
)

func TestSeverityValid(t *testing.T) {
	tests := []struct {
		name     string
		severity Severity
		want     bool
	}{
		{name: "info is valid", severity: SeverityInfo, want: true},
		{name: "warning is valid", severity: SeverityWarning, want: true},
		{name: "error is valid", severity: SeverityError, want: true},
		{name: "empty is invalid", severity: "", want: false},
		{name: "unknown value is invalid", severity: "critical", want: false},
		{name: "wrong case is invalid", severity: "Error", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.severity.Valid(); got != tt.want {
				t.Errorf("Severity(%q).Valid() = %v, want %v", tt.severity, got, tt.want)
			}
		})
	}
}

func TestSeverityRank(t *testing.T) {
	tests := []struct {
		name     string
		less     Severity
		greater  Severity
		wantLess bool
	}{
		{name: "info ranks below warning", less: SeverityInfo, greater: SeverityWarning, wantLess: true},
		{name: "warning ranks below error", less: SeverityWarning, greater: SeverityError, wantLess: true},
		{name: "info ranks below error", less: SeverityInfo, greater: SeverityError, wantLess: true},
		{name: "unknown ranks below info", less: "bogus", greater: SeverityInfo, wantLess: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.less.Rank() < tt.greater.Rank(); got != tt.wantLess {
				t.Errorf("Severity(%q).Rank()=%d < Severity(%q).Rank()=%d = %v, want %v",
					tt.less, tt.less.Rank(), tt.greater, tt.greater.Rank(), got, tt.wantLess)
			}
		})
	}
}

func TestNormalizeReport(t *testing.T) {
	tests := []struct {
		name string
		in   Report
		// wantErrContains 非空时期望返回错误，且错误信息包含这些片段——
		// 这些信息会直接回喂给模型，所以断言它确实指出了"错在哪"和"怎么改"。
		wantErrContains []string
		// wantIDs 是期望分配到的 ID 序列（仅在期望成功时检查）。
		wantIDs []string
		// check 是针对具体用例的额外断言，可为 nil。
		check func(*testing.T, Report)
	}{
		{
			name: "assigns sequential ids ignoring model supplied ones",
			in: Report{
				Summary: "looks mostly fine",
				Findings: []Finding{
					{ID: "dup", File: "a.go", Severity: SeverityError, Summary: "nil deref"},
					{ID: "dup", File: "b.go", Severity: SeverityInfo, Summary: "naming"},
					{File: "c.go", Severity: SeverityWarning, Summary: "unchecked error"},
				},
			},
			wantIDs: []string{"f1", "f2", "f3"},
		},
		{
			name:    "empty findings list is accepted",
			in:      Report{Summary: "no problems found"},
			wantIDs: []string{},
		},
		{
			name: "trims whitespace on all text fields",
			in: Report{
				Summary: "  overall fine  ",
				Findings: []Finding{
					{File: "  a.go\n", Severity: SeverityInfo, Summary: "  short  ", Detail: "\n long \n"},
				},
			},
			wantIDs: []string{"f1"},
			check: func(t *testing.T, got Report) {
				if got.Summary != "overall fine" {
					t.Errorf("Summary = %q, want %q", got.Summary, "overall fine")
				}
				f := got.Findings[0]
				if f.File != "a.go" || f.Summary != "short" || f.Detail != "long" {
					t.Errorf("finding = %+v, want trimmed File/Summary/Detail", f)
				}
			},
		},
		{
			name: "keeps anchor verbatim except for surrounding blank lines",
			in: Report{
				Summary: "ok",
				Findings: []Finding{
					{File: "a.go", Anchor: "\n\tif err != nil {\n\t\treturn err\n\t}\n", Severity: SeverityError, Summary: "boom"},
					{File: "b.go", Severity: SeverityInfo, Summary: "file level note"},
				},
			},
			wantIDs: []string{"f1", "f2"},
			check: func(t *testing.T, got Report) {
				// 缩进必须原样保留：anchor 的归一化是 ResolveAnchors 的职责，
				// NormalizeReport 只剥掉首尾换行，不能提前把缩进吃掉。
				want := "\tif err != nil {\n\t\treturn err\n\t}"
				if got.Findings[0].Anchor != want {
					t.Errorf("Findings[0].Anchor = %q, want %q", got.Findings[0].Anchor, want)
				}
				if got.Findings[1].Anchor != "" {
					t.Errorf("Findings[1].Anchor = %q, want empty", got.Findings[1].Anchor)
				}
			},
		},
		{
			name: "never trusts model supplied line numbers",
			in: Report{
				Summary: "ok",
				Findings: []Finding{
					{File: "a.go", Severity: SeverityError, Summary: "boom", StartLine: 999, EndLine: 1200},
				},
			},
			wantIDs: []string{"f1"},
			check: func(t *testing.T, got Report) {
				// 行号只能由 ResolveAnchors 算出来。模型即便硬塞了值也必须被丢弃，
				// 否则就退回了"相信模型数行号"这个本来要解决的问题。
				if got.Findings[0].StartLine != 0 || got.Findings[0].EndLine != 0 {
					t.Errorf("StartLine/EndLine = %d/%d, want 0/0: model supplied line numbers must be discarded",
						got.Findings[0].StartLine, got.Findings[0].EndLine)
				}
			},
		},
		{
			name:            "rejects empty summary",
			in:              Report{Findings: []Finding{{File: "a.go", Severity: SeverityInfo, Summary: "x"}}},
			wantErrContains: []string{"summary is empty", "even when there are no findings"},
		},
		{
			name:            "rejects whitespace only summary",
			in:              Report{Summary: "   \n  "},
			wantErrContains: []string{"summary is empty"},
		},
		{
			name: "rejects empty file with index and remedy",
			in: Report{
				Summary: "ok",
				Findings: []Finding{
					{File: "a.go", Severity: SeverityInfo, Summary: "fine"},
					{File: "", Severity: SeverityInfo, Summary: "orphan"},
				},
			},
			wantErrContains: []string{"findings[1].file is empty", "repo-relative path"},
		},
		{
			name: "rejects empty finding summary",
			in: Report{
				Summary:  "ok",
				Findings: []Finding{{File: "a.go", Severity: SeverityInfo, Summary: "  "}},
			},
			wantErrContains: []string{"findings[0].summary is empty", "one sentence"},
		},
		{
			name: "rejects unknown severity and lists the allowed values",
			in: Report{
				Summary:  "ok",
				Findings: []Finding{{File: "a.go", Severity: "critical", Summary: "boom"}},
			},
			wantErrContains: []string{`findings[0].severity is "critical"`, `"info"`, `"warning"`, `"error"`},
		},
		{
			name: "accepts a finding without an anchor as a file level note",
			in: Report{
				Summary:  "ok",
				Findings: []Finding{{File: "a.go", Severity: SeverityInfo, Summary: "this file needs a package comment"}},
			},
			wantIDs: []string{"f1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeReport(tt.in)

			if len(tt.wantErrContains) > 0 {
				if err == nil {
					t.Fatalf("NormalizeReport() error = nil, want error containing %q", tt.wantErrContains)
				}
				for _, want := range tt.wantErrContains {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err.Error(), want)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("NormalizeReport() error = %v", err)
			}
			if len(got.Findings) != len(tt.wantIDs) {
				t.Fatalf("len(Findings) = %d, want %d", len(got.Findings), len(tt.wantIDs))
			}
			for i, wantID := range tt.wantIDs {
				if got.Findings[i].ID != wantID {
					t.Errorf("Findings[%d].ID = %q, want %q", i, got.Findings[i].ID, wantID)
				}
			}
			if got.Critiqued {
				t.Error("Critiqued = true, want false: NormalizeReport must not claim the critique ran")
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func TestReportKeptFindings(t *testing.T) {
	tests := []struct {
		name    string
		report  Report
		wantIDs []string
	}{
		{
			name: "before critique all findings are returned despite Kept being false",
			report: Report{
				Critiqued: false,
				Findings: []Finding{
					{ID: "f1", Kept: false},
					{ID: "f2", Kept: false},
				},
			},
			wantIDs: []string{"f1", "f2"},
		},
		{
			name: "after critique only kept findings are returned",
			report: Report{
				Critiqued: true,
				Findings: []Finding{
					{ID: "f1", Kept: true},
					{ID: "f2", Kept: false, CritiqueReason: "speculative"},
					{ID: "f3", Kept: true},
				},
			},
			wantIDs: []string{"f1", "f3"},
		},
		{
			name:    "after critique dropping everything returns empty",
			report:  Report{Critiqued: true, Findings: []Finding{{ID: "f1", Kept: false}}},
			wantIDs: []string{},
		},
		{
			name:    "empty report returns empty",
			report:  Report{Critiqued: true},
			wantIDs: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.report.KeptFindings()
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("len(KeptFindings()) = %d, want %d", len(got), len(tt.wantIDs))
			}
			for i, wantID := range tt.wantIDs {
				if got[i].ID != wantID {
					t.Errorf("KeptFindings()[%d].ID = %q, want %q", i, got[i].ID, wantID)
				}
			}
		})
	}
}

// TestKeptFindingsReturnsACopy 固定住"返回的一律是新切片"这条约定。
// 未复核时曾经直接返回 r.Findings 本身，导致调用方（Render 的排序）
// 会打乱调用方持有的 Report。
func TestKeptFindingsReturnsACopy(t *testing.T) {
	tests := []struct {
		name   string
		report Report
	}{
		{
			name: "before critique",
			report: Report{Critiqued: false, Findings: []Finding{
				{ID: "f1"}, {ID: "f2"},
			}},
		},
		{
			name: "after critique",
			report: Report{Critiqued: true, Findings: []Finding{
				{ID: "f1", Kept: true}, {ID: "f2", Kept: true},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.report.KeptFindings()
			if len(got) < 2 {
				t.Fatalf("len(KeptFindings()) = %d, want at least 2 for this test", len(got))
			}
			got[0].ID = "mutated"
			if tt.report.Findings[0].ID == "mutated" {
				t.Error("writing to KeptFindings() result modified the Report; want an independent copy")
			}
		})
	}
}

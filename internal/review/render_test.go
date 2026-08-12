package review

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	tests := []struct {
		name string
		in   Report
		// wantContains / wantExcludes 断言输出里出现/不出现的片段。
		wantContains []string
		wantExcludes []string
		// wantOrder 里的片段必须按给定顺序出现，用来验证排序规则。
		wantOrder []string
	}{
		{
			name: "renders a finding with severity path line range and id",
			in: Report{
				Summary: "one issue worth fixing",
				Findings: []Finding{
					{ID: "f1", File: "internal/store/store.go", StartLine: 88, EndLine: 92,
						Severity: SeverityError, Summary: "ctx cancellation ignored", Detail: "wrap the call"},
				},
			},
			wantContains: []string{
				"[ERROR]", "internal/store/store.go:88-92", "(f1)",
				"ctx cancellation ignored", "wrap the call",
				"1 finding:", "one issue worth fixing",
			},
		},
		{
			name: "single line finding omits the range",
			in: Report{
				Summary:  "s",
				Findings: []Finding{{ID: "f1", File: "a.go", StartLine: 42, EndLine: 42, Severity: SeverityInfo, Summary: "note"}},
			},
			wantContains: []string{"a.go:42"},
			wantExcludes: []string{"a.go:42-42"},
		},
		{
			name: "file level finding omits line numbers entirely",
			in: Report{
				Summary:  "s",
				Findings: []Finding{{ID: "f1", File: "a.go", Severity: SeverityInfo, Summary: "needs a package comment"}},
			},
			wantContains: []string{"a.go  (f1)"},
			wantExcludes: []string{"a.go:"},
		},
		{
			name: "sorts by severity first then file then line",
			in: Report{
				Summary: "s",
				Findings: []Finding{
					{ID: "f1", File: "b.go", StartLine: 10, Severity: SeverityInfo, Summary: "least important"},
					{ID: "f2", File: "z.go", StartLine: 5, Severity: SeverityError, Summary: "most important"},
					{ID: "f3", File: "a.go", StartLine: 99, Severity: SeverityWarning, Summary: "middle"},
					{ID: "f4", File: "a.go", StartLine: 3, Severity: SeverityWarning, Summary: "middle earlier line"},
				},
			},
			wantOrder: []string{"most important", "middle earlier line", "middle", "least important"},
		},
		{
			name:         "empty findings says so instead of printing nothing",
			in:           Report{Summary: "small change, nothing to flag"},
			wantContains: []string{"No issues found.", "small change, nothing to flag"},
		},
		{
			name: "after critique only kept findings are shown",
			in: Report{
				Critiqued: true,
				Summary:   "s",
				Findings: []Finding{
					{ID: "f1", File: "a.go", Severity: SeverityError, Summary: "genuine problem", Kept: true},
					{ID: "f2", File: "b.go", Severity: SeverityError, Summary: "over-interpreted", Kept: false,
						CritiqueReason: "not supported by the code"},
				},
			},
			wantContains: []string{"genuine problem", "1 finding:"},
			wantExcludes: []string{"over-interpreted", "not supported by the code"},
		},
		{
			name: "before critique findings are shown even though Kept is false",
			in: Report{
				Critiqued: false,
				Summary:   "s",
				Findings:  []Finding{{ID: "f1", File: "a.go", Severity: SeverityInfo, Summary: "note", Kept: false}},
			},
			wantContains: []string{"note", "1 finding:"},
		},
		{
			name: "plural form for multiple findings",
			in: Report{
				Summary: "s",
				Findings: []Finding{
					{ID: "f1", File: "a.go", Severity: SeverityInfo, Summary: "one"},
					{ID: "f2", File: "b.go", Severity: SeverityInfo, Summary: "two"},
				},
			},
			wantContains: []string{"2 findings:"},
		},
		{
			name:         "empty report still renders something readable",
			in:           Report{},
			wantContains: []string{"No issues found."},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Render(tt.in)

			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("output does not contain %q:\n%s", want, got)
				}
			}
			for _, unwanted := range tt.wantExcludes {
				if strings.Contains(got, unwanted) {
					t.Errorf("output contains excluded text %q:\n%s", unwanted, got)
				}
			}

			prev := -1
			for _, want := range tt.wantOrder {
				at := strings.Index(got, want)
				if at < 0 {
					t.Fatalf("output does not contain %q:\n%s", want, got)
				}
				if at < prev {
					t.Errorf("%q appears out of order:\n%s", want, got)
				}
				prev = at
			}
		})
	}
}

func TestRenderDoesNotMutateInput(t *testing.T) {
	tests := []struct {
		name string
		in   Report
	}{
		{
			name: "sorting for display must not reorder the caller's findings",
			in: Report{
				Summary: "s",
				Findings: []Finding{
					{ID: "f1", File: "b.go", Severity: SeverityInfo, Summary: "low"},
					{ID: "f2", File: "a.go", Severity: SeverityError, Summary: "high"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := make([]Finding, len(tt.in.Findings))
			copy(before, tt.in.Findings)

			Render(tt.in)

			for i := range before {
				if tt.in.Findings[i] != before[i] {
					t.Errorf("Findings[%d] changed: got %+v, want %+v", i, tt.in.Findings[i], before[i])
				}
			}
		})
	}
}

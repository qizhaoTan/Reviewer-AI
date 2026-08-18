package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCritiqueVerdictExecute(t *testing.T) {
	tests := []struct {
		name string
		args string
		// wantErrContains 非空时期望调用失败，且 Output 含这些片段。
		wantErrContains []string
		wantKeep        bool
		wantReason      string
	}{
		{
			name:       "records a keep verdict",
			args:       `{"keep":true,"reason":"confirmed by reading the function"}`,
			wantKeep:   true,
			wantReason: "confirmed by reading the function",
		},
		{
			name:       "records a drop verdict",
			args:       `{"keep":false,"reason":"the nil case is handled by the caller"}`,
			wantKeep:   false,
			wantReason: "the nil case is handled by the caller",
		},
		{
			name:       "trims whitespace around the reason",
			args:       `{"keep":true,"reason":"  spaced out  "}`,
			wantKeep:   true,
			wantReason: "spaced out",
		},
		{
			name:            "rejects a missing reason when keeping",
			args:            `{"keep":true}`,
			wantErrContains: []string{"reason 为空", "保留"},
		},
		{
			name:            "rejects a missing reason when dropping",
			args:            `{"keep":false,"reason":"   "}`,
			wantErrContains: []string{"reason 为空", "丢弃"},
		},
		{
			name:            "rejects malformed json",
			args:            `{"keep": tru`,
			wantErrContains: []string{"不是合法的 JSON", `"keep"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CritiqueVerdictTool{}.Execute(context.Background(), "/repo", json.RawMessage(tt.args))

			if len(tt.wantErrContains) > 0 {
				if !got.IsError {
					t.Fatalf("Execute() IsError = false, want an error; Output = %q", got.Output)
				}
				for _, want := range tt.wantErrContains {
					if !strings.Contains(got.Output, want) {
						t.Errorf("Output %q does not contain %q", got.Output, want)
					}
				}
				if got.CritiqueVerdict != nil {
					t.Error("a rejected verdict must not be returned")
				}
				return
			}

			if got.IsError {
				t.Fatalf("Execute() IsError = true, Output = %q", got.Output)
			}
			if got.CritiqueVerdict == nil {
				t.Fatal("CritiqueVerdict is nil, want the recorded verdict")
			}
			if got.CritiqueVerdict.Keep != tt.wantKeep {
				t.Errorf("Keep = %v, want %v", got.CritiqueVerdict.Keep, tt.wantKeep)
			}
			if got.CritiqueVerdict.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", got.CritiqueVerdict.Reason, tt.wantReason)
			}
			if got.ReviewResult != nil {
				t.Error("ReviewResult must stay nil; only submit_review sets it")
			}
		})
	}
}

func TestCritiqueVerdictDefinition(t *testing.T) {
	tests := []struct {
		name         string
		wantRequired []string
	}{
		{name: "requires both keep and reason", wantRequired: []string{"keep", "reason"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := CritiqueVerdictTool{}.Definition()
			if def.Name != CritiqueVerdictName {
				t.Errorf("Name = %q, want %q", def.Name, CritiqueVerdictName)
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
		})
	}
}

// TestCritiqueVerdictAndSubmitReviewAreDistinct 固定住两个收尾工具互不串用：
// 复核者不该能改写审查结论，初审者也不该能给自己下裁决。
func TestCritiqueVerdictAndSubmitReviewAreDistinct(t *testing.T) {
	tests := []struct {
		name string
		tool ITool
		args string
		// wantsVerdict / wantsReview 是该工具应当填写的字段。
		wantsVerdict bool
		wantsReview  bool
	}{
		{
			name: "submit_verdict fills only CritiqueVerdict",
			tool: CritiqueVerdictTool{}, args: `{"keep":true,"reason":"ok"}`,
			wantsVerdict: true,
		},
		{
			name: "submit_review fills only ReviewResult",
			tool: SubmitReviewTool{}, args: `{"summary":"ok","findings":[]}`,
			wantsReview: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.tool.Execute(context.Background(), t.TempDir(), json.RawMessage(tt.args))
			if got.IsError {
				t.Fatalf("Execute() failed: %s", got.Output)
			}
			if (got.CritiqueVerdict != nil) != tt.wantsVerdict {
				t.Errorf("CritiqueVerdict set = %v, want %v", got.CritiqueVerdict != nil, tt.wantsVerdict)
			}
			if (got.ReviewResult != nil) != tt.wantsReview {
				t.Errorf("ReviewResult set = %v, want %v", got.ReviewResult != nil, tt.wantsReview)
			}
		})
	}
}

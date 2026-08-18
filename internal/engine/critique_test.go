package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/review"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
	"github.com/qizhaoTan/Reviewer-AI/internal/tool"
)

// verdictProvider 按"意见的 summary"决定裁决，让每条意见的复核结果可预期。
// 它必须是并发安全的——复核本来就是多 goroutine 同时调用同一个 provider。
type verdictProvider struct {
	// keepIf 返回该条意见应当被保留还是丢弃。
	keepIf func(userMessage string) bool
	// failIf 返回非 nil 时，该条复核以错误告终。
	failIf func(userMessage string) error
	// turnsBeforeVerdict 是给出裁决前先做几轮工具调用，用来验证多轮循环。
	turnsBeforeVerdict int

	mu sync.Mutex
	// perFindingTurns 记录每条意见（以 user 消息为键）已经跑了几轮。
	perFindingTurns map[string]int
	// maxConcurrent 记录观察到的最大并发数。
	inFlight, maxConcurrent atomic.Int64
	// calls 是 Generate 的总调用次数。
	calls atomic.Int64
}

func (p *verdictProvider) Generate(_ context.Context, msgs []schema.Message, _ []schema.ToolDefinition) (*schema.Message, error) {
	cur := p.inFlight.Add(1)
	for {
		peak := p.maxConcurrent.Load()
		if cur <= peak || p.maxConcurrent.CompareAndSwap(peak, cur) {
			break
		}
	}
	defer p.inFlight.Add(-1)
	p.calls.Add(1)

	// msgs[1] 是这条意见的复核上下文，用它作为该条意见的标识。
	key := msgs[1].Content

	if p.failIf != nil {
		if err := p.failIf(key); err != nil {
			return nil, err
		}
	}

	p.mu.Lock()
	if p.perFindingTurns == nil {
		p.perFindingTurns = map[string]int{}
	}
	p.perFindingTurns[key]++
	turn := p.perFindingTurns[key]
	p.mu.Unlock()

	// 先花掉若干轮做只读调查，再给裁决。
	if turn <= p.turnsBeforeVerdict {
		return &schema.Message{
			Role:      schema.RoleAssistant,
			ToolCalls: []schema.ToolCall{{ID: fmt.Sprintf("g%d", turn), Name: "glob", Arguments: json.RawMessage(`{"pattern":"*.go"}`)}},
		}, nil
	}

	keep := p.keepIf == nil || p.keepIf(key)
	args, _ := json.Marshal(tool.Verdict{Keep: keep, Reason: fmt.Sprintf("verdict for turn %d", turn)})
	return &schema.Message{
		Role:      schema.RoleAssistant,
		ToolCalls: []schema.ToolCall{{ID: "v1", Name: tool.CritiqueVerdictName, Arguments: args}},
	}, nil
}

func critiqueTestTools() []tool.ITool {
	return []tool.ITool{tool.ReadFileTool{}, tool.GlobTool{}, tool.GrepTool{}, tool.CritiqueVerdictTool{}}
}

func critiqueTestChanges() []gitdiff.Change {
	return []gitdiff.Change{{Status: "M", Path: "config.go", Patch: testPatch}}
}

func findingsNamed(summaries ...string) []review.Finding {
	out := make([]review.Finding, 0, len(summaries))
	for i, s := range summaries {
		out = append(out, review.Finding{
			ID: fmt.Sprintf("f%d", i+1), File: "config.go",
			Severity: review.SeverityWarning, Summary: s,
		})
	}
	return out
}

func TestCritique(t *testing.T) {
	tests := []struct {
		name     string
		findings []review.Finding
		keepIf   func(string) bool
		failIf   func(string) error
		turns    int
		// wantKept 按 Finding 顺序给出期望的 Kept 值。
		wantKept []bool
		// wantReasonContains 按顺序断言 CritiqueReason 含有的片段，空串表示不检查。
		wantReasonContains []string
	}{
		{
			name:     "keeps every finding when the critic agrees",
			findings: findingsNamed("a", "b", "c"),
			wantKept: []bool{true, true, true},
		},
		{
			name:     "drops the findings the critic rejects",
			findings: findingsNamed("keep me", "drop me", "keep me too"),
			keepIf:   func(msg string) bool { return !strings.Contains(msg, "drop me") },
			wantKept: []bool{true, false, true},
		},
		{
			name:     "drops every finding when the critic rejects all",
			findings: findingsNamed("a", "b"),
			keepIf:   func(string) bool { return false },
			wantKept: []bool{false, false},
		},
		{
			name:     "investigates with tools before deciding",
			findings: findingsNamed("needs digging"),
			turns:    3,
			wantKept: []bool{true},
		},
		{
			name:     "a failing critique keeps the finding rather than silently dropping it",
			findings: findingsNamed("fine", "explodes"),
			failIf: func(msg string) error {
				if strings.Contains(msg, "explodes") {
					return errors.New("provider is down")
				}
				return nil
			},
			wantKept:           []bool{true, true},
			wantReasonContains: []string{"", "provider is down"},
		},
		{
			name:     "no findings needs no critique",
			findings: nil,
			wantKept: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &verdictProvider{keepIf: tt.keepIf, failIf: tt.failIf, turnsBeforeVerdict: tt.turns}
			deps := CritiqueDeps{
				LLM:         llm,
				Tools:       critiqueTestTools(),
				RepoRoot:    t.TempDir(),
				Concurrency: 4,
			}
			in := review.Report{Summary: "initial", Findings: tt.findings}

			got, err := Critique(context.Background(), deps, in, critiqueTestChanges())
			if err != nil {
				t.Fatalf("Critique() error = %v", err)
			}

			if !got.Critiqued {
				t.Error("Critiqued = false, want true")
			}
			if got.Summary != in.Summary {
				t.Errorf("Summary = %q, want it unchanged (%q)", got.Summary, in.Summary)
			}
			if len(got.Findings) != len(tt.wantKept) {
				t.Fatalf("len(Findings) = %d, want %d: critique must not add or remove findings",
					len(got.Findings), len(tt.wantKept))
			}
			for i, wantKeep := range tt.wantKept {
				if got.Findings[i].Kept != wantKeep {
					t.Errorf("Findings[%d] (%s) Kept = %v, want %v",
						i, got.Findings[i].Summary, got.Findings[i].Kept, wantKeep)
				}
				if got.Findings[i].CritiqueReason == "" {
					t.Errorf("Findings[%d] has no CritiqueReason; both keeping and dropping must be explained", i)
				}
				if got.Findings[i].ID != tt.findings[i].ID {
					t.Errorf("Findings[%d].ID = %q, want %q: order must be preserved",
						i, got.Findings[i].ID, tt.findings[i].ID)
				}
			}
			for i, want := range tt.wantReasonContains {
				if want == "" {
					continue
				}
				if !strings.Contains(got.Findings[i].CritiqueReason, want) {
					t.Errorf("Findings[%d].CritiqueReason = %q, want it to contain %q",
						i, got.Findings[i].CritiqueReason, want)
				}
			}
		})
	}
}

func TestCritiqueDoesNotMutateInput(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "the caller's report is left untouched"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := review.Report{Summary: "initial", Findings: findingsNamed("a", "b")}
			before := append([]review.Finding(nil), in.Findings...)

			llm := &verdictProvider{keepIf: func(string) bool { return false }}
			deps := CritiqueDeps{LLM: llm, Tools: critiqueTestTools(), RepoRoot: t.TempDir(), Concurrency: 2}

			if _, err := Critique(context.Background(), deps, in, critiqueTestChanges()); err != nil {
				t.Fatalf("Critique() error = %v", err)
			}

			if in.Critiqued {
				t.Error("input Report.Critiqued was flipped to true")
			}
			for i := range before {
				if in.Findings[i] != before[i] {
					t.Errorf("input Findings[%d] changed: got %+v, want %+v", i, in.Findings[i], before[i])
				}
			}
		})
	}
}

func TestCritiqueRespectsConcurrencyLimit(t *testing.T) {
	tests := []struct {
		name        string
		findings    int
		concurrency int
		wantMaxPeak int64
	}{
		{name: "limit of 1 serializes the critiques", findings: 6, concurrency: 1, wantMaxPeak: 1},
		{name: "limit of 3 caps the peak", findings: 12, concurrency: 3, wantMaxPeak: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summaries := make([]string, tt.findings)
			for i := range summaries {
				summaries[i] = fmt.Sprintf("finding %d", i)
			}
			// 每条先跑两轮工具调用，拉长单条耗时，让并发窗口足够宽。
			llm := &verdictProvider{turnsBeforeVerdict: 2}
			deps := CritiqueDeps{
				LLM: llm, Tools: critiqueTestTools(), RepoRoot: t.TempDir(),
				Concurrency: tt.concurrency,
			}

			got, err := Critique(context.Background(), deps,
				review.Report{Summary: "s", Findings: findingsNamed(summaries...)}, critiqueTestChanges())
			if err != nil {
				t.Fatalf("Critique() error = %v", err)
			}
			if len(got.Findings) != tt.findings {
				t.Fatalf("len(Findings) = %d, want %d", len(got.Findings), tt.findings)
			}
			if peak := llm.maxConcurrent.Load(); peak > tt.wantMaxPeak {
				t.Errorf("observed peak concurrency %d, want at most %d", peak, tt.wantMaxPeak)
			}
		})
	}
}

func TestCritiqueMaxTurns(t *testing.T) {
	tests := []struct {
		name     string
		maxTurns int
		// turnsBeforeVerdict 大于 maxTurns 时复核永远给不出裁决。
		turnsBeforeVerdict int
		wantKept           bool
		wantReasonContains string
	}{
		{
			name:     "gives up after the turn budget and keeps the finding",
			maxTurns: 3, turnsBeforeVerdict: 99,
			wantKept: true, wantReasonContains: "exceeded max critique turns (3)",
		},
		{
			name:     "finishes within budget",
			maxTurns: 5, turnsBeforeVerdict: 2,
			wantKept: true, wantReasonContains: "verdict for turn",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &verdictProvider{turnsBeforeVerdict: tt.turnsBeforeVerdict}
			deps := CritiqueDeps{
				LLM: llm, Tools: critiqueTestTools(), RepoRoot: t.TempDir(),
				Concurrency: 1, MaxTurns: tt.maxTurns,
			}

			got, err := Critique(context.Background(), deps,
				review.Report{Summary: "s", Findings: findingsNamed("x")}, critiqueTestChanges())
			if err != nil {
				t.Fatalf("Critique() error = %v", err)
			}
			if got.Findings[0].Kept != tt.wantKept {
				t.Errorf("Kept = %v, want %v", got.Findings[0].Kept, tt.wantKept)
			}
			if !strings.Contains(got.Findings[0].CritiqueReason, tt.wantReasonContains) {
				t.Errorf("CritiqueReason = %q, want it to contain %q",
					got.Findings[0].CritiqueReason, tt.wantReasonContains)
			}
		})
	}
}

func TestBuildCritiqueMessages(t *testing.T) {
	tests := []struct {
		name           string
		finding        review.Finding
		patch          string
		languagePrompt string
		systemSuffix   string // 非空时断言复核 system prompt 以此结尾
		wantContains   []string
		wantExcludes   []string
	}{
		{
			name: "language prompt is appended to the critique system prompt",
			finding: review.Finding{
				ID: "f1", File: "config.go", Severity: review.SeverityInfo, Summary: "note",
			},
			patch:          testPatch,
			languagePrompt: "- Your response needs to be in Chinese!!!",
			systemSuffix:   "\n\n- Your response needs to be in Chinese!!!",
		},
		{
			name: "includes the finding, its anchor and the file diff",
			finding: review.Finding{
				ID: "f1", File: "config.go", StartLine: 12, EndLine: 14,
				Severity: review.SeverityError, Summary: "swallowed error",
				Detail: "wrap it", Anchor: "return nil, err",
			},
			patch:        testPatch,
			wantContains: []string{"config.go", "lines: 12-14", "error", "swallowed error", "wrap it", "return nil, err", "```diff"},
		},
		{
			name: "single line finding renders one line number",
			finding: review.Finding{
				ID: "f1", File: "config.go", StartLine: 13, EndLine: 13,
				Severity: review.SeverityInfo, Summary: "note",
			},
			patch:        testPatch,
			wantContains: []string{"line: 13"},
			wantExcludes: []string{"lines: 13-13"},
		},
		{
			name: "file level finding renders no line number",
			finding: review.Finding{
				ID: "f1", File: "config.go", Severity: review.SeverityInfo, Summary: "note",
			},
			patch:        testPatch,
			wantExcludes: []string{"line:", "lines:"},
		},
		{
			name: "missing diff is stated explicitly rather than left blank",
			finding: review.Finding{
				ID: "f1", File: "gone.go", Severity: review.SeverityInfo, Summary: "note",
			},
			patch:        "",
			wantContains: []string{"No diff is available for gone.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := buildCritiqueMessages(tt.finding, tt.patch, tt.languagePrompt)
			if len(msgs) != 2 {
				t.Fatalf("len(msgs) = %d, want 2", len(msgs))
			}
			if msgs[0].Role != schema.RoleSystem || msgs[0].Content == "" {
				t.Errorf("msgs[0] = %+v, want a non-empty system message", msgs[0])
			}
			if tt.systemSuffix != "" && !strings.HasSuffix(msgs[0].Content, tt.systemSuffix) {
				t.Errorf("system message does not end with %q:\n%s", tt.systemSuffix, msgs[0].Content)
			}
			if tt.languagePrompt == "" && msgs[0].Content != critiqueSystemPrompt {
				t.Error("empty language prompt should leave the critique system prompt untouched")
			}
			if msgs[1].Role != schema.RoleUser {
				t.Errorf("msgs[1].Role = %q, want %q", msgs[1].Role, schema.RoleUser)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(msgs[1].Content, want) {
					t.Errorf("user message does not contain %q:\n%s", want, msgs[1].Content)
				}
			}
			for _, unwanted := range tt.wantExcludes {
				if strings.Contains(msgs[1].Content, unwanted) {
					t.Errorf("user message contains excluded text %q:\n%s", unwanted, msgs[1].Content)
				}
			}
		})
	}
}

// TestCritiqueSystemPromptPushesBack 固定住复核提示词里对抗"附和初审"的措辞。
// 同一个模型自我批判时天然不愿否定自己，这几句是唯一的对冲。
func TestCritiqueSystemPromptPushesBack(t *testing.T) {
	tests := []struct {
		name     string
		contains string
	}{
		{name: "tells the critic that dropping is its job", contains: "dropping a weak comment is your job"},
		{name: "tells the critic to verify against real code", contains: "go read that function"},
		{name: "forbids adding or editing findings", contains: "cannot edit the comment or add new ones"},
		{name: "names the verdict tool", contains: "submit_verdict"},
	}

	lower := strings.ToLower(critiqueSystemPrompt)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(lower, strings.ToLower(tt.contains)) {
				t.Errorf("critique system prompt is missing %q:\n%s", tt.contains, critiqueSystemPrompt)
			}
		})
	}
}

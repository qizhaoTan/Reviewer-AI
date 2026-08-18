package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

func call(name, args string) schema.ToolCall {
	return schema.ToolCall{Name: name, Arguments: json.RawMessage(args)}
}

// TestCallDeduperFirstSeenRound 覆盖去重判定的每条边界：同名同参才算重复，
// 参数只差空白/键序也要算重复（模型每轮重新序列化参数，键序没有保证），
// 而工具名或参数值不同必须放行——误判成重复会让模型拿不到它真正需要的结果。
func TestCallDeduperFirstSeenRound(t *testing.T) {
	tests := []struct {
		name      string
		recorded  schema.ToolCall
		probe     schema.ToolCall
		wantRound int
		wantSeen  bool
	}{
		{
			name:      "exact same call is a repeat",
			recorded:  call("grep", `{"pattern":"CountType603"}`),
			probe:     call("grep", `{"pattern":"CountType603"}`),
			wantRound: 3,
			wantSeen:  true,
		},
		{
			name:      "whitespace differences still count as a repeat",
			recorded:  call("grep", `{"pattern":"CountType603"}`),
			probe:     call("grep", `{ "pattern" : "CountType603" }`),
			wantRound: 3,
			wantSeen:  true,
		},
		{
			name:      "key order differences still count as a repeat",
			recorded:  call("grep", `{"pattern":"foo","path":"a.go"}`),
			probe:     call("grep", `{"path":"a.go","pattern":"foo"}`),
			wantRound: 3,
			wantSeen:  true,
		},
		{
			name:     "different argument value is not a repeat",
			recorded: call("grep", `{"pattern":"CountType603"}`),
			probe:    call("grep", `{"pattern":"CountType601"}`),
			wantSeen: false,
		},
		{
			name:     "same arguments to a different tool is not a repeat",
			recorded: call("grep", `{"path":"a.go"}`),
			probe:    call("read_file", `{"path":"a.go"}`),
			wantSeen: false,
		},
		{
			name:     "nested argument difference is not a repeat",
			recorded: call("grep", `{"opts":{"mode":"content"}}`),
			probe:    call("grep", `{"opts":{"mode":"files_with_matches"}}`),
			wantSeen: false,
		},
		{
			name:      "malformed json falls back to raw bytes and still matches itself",
			recorded:  call("grep", `not json`),
			probe:     call("grep", `not json`),
			wantRound: 3,
			wantSeen:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newCallDeduper()
			d.record(tt.recorded, 3)

			round, seen := d.firstSeenRound(tt.probe)
			if seen != tt.wantSeen {
				t.Fatalf("firstSeenRound seen = %v, want %v", seen, tt.wantSeen)
			}
			if seen && round != tt.wantRound {
				t.Errorf("firstSeenRound round = %d, want %d", round, tt.wantRound)
			}
		})
	}
}

// TestCallDeduperRecordKeepsEarliestRound 确认重复登记不会把轮次覆盖成后来的那次：
// 提醒语要指回最早那条结果，指到后面反而让模型往回翻得更远。
func TestCallDeduperRecordKeepsEarliestRound(t *testing.T) {
	tests := []struct {
		name      string
		rounds    []int
		wantRound int
	}{
		{name: "second record does not overwrite the first", rounds: []int{2, 7}, wantRound: 2},
		{name: "single record keeps its round", rounds: []int{5}, wantRound: 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newCallDeduper()
			tc := call("grep", `{"pattern":"x"}`)
			for _, r := range tt.rounds {
				d.record(tc, r)
			}

			round, seen := d.firstSeenRound(tc)
			if !seen {
				t.Fatalf("firstSeenRound seen = false, want true")
			}
			if round != tt.wantRound {
				t.Errorf("firstSeenRound round = %d, want %d", round, tt.wantRound)
			}
		})
	}
}

// TestRepeatNotice 确认提醒语把三件事都说清楚了：这次被跳过、原结果在哪一轮、
// 接下来该干什么。少了最后一条，模型很可能换个等价写法再查一遍。
func TestRepeatNotice(t *testing.T) {
	tests := []struct {
		name     string
		tc       schema.ToolCall
		round    int
		contains []string
	}{
		{
			name:     "mentions tool name, round and the way out",
			tc:       call("grep", `{"pattern":"x"}`),
			round:    3,
			contains: []string{"grep", "第 3 轮", "submit_review", "已跳过"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := repeatNotice(tt.tc, tt.round)
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Errorf("repeatNotice = %q, missing %q", got, want)
				}
			}
		})
	}
}

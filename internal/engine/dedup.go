package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

// callDeduper 记录本次循环里已经执行过的工具调用，用于在模型重复发起
// 完全相同的调用时拦下来。
//
// 为什么需要它：实测中模型判定改动无误后不会收工，而是反复搜同一个符号
// （一次 20 行的改动里，同一个常量被 grep 了 6 次），把一次审查从几轮拖到
// 二十几轮。这不是上下文丢失——engine 只追加不裁剪、provider 也原样回放
// ToolCalls，历史结果确实还在上下文里，模型只是没去看。
//
// 拦截的收益不在于省下工具执行时间（grep 一次才几毫秒），而在于省下后续的
// 模型往返：把"你已经查过了"作为一个显式信号写进上下文，比默默把同样的结果
// 再喂一遍更容易让模型收敛——后者跟它自己重新执行一次没有任何区别，
// 自然也不会改变它的行为。
//
// 只拦截成功的调用：失败的调用（比如正则写错了）重试一次是合理行为，
// 拦下来只会让模型卡在一个它有权修正的错误上。
type callDeduper struct {
	// seen 把"工具名 + 规范化参数"的指纹映射到它首次成功执行时的轮次（从 1 开始），
	// 供提示语引用具体轮次，让模型知道该往回翻哪里。
	seen map[string]int
}

func newCallDeduper() *callDeduper {
	return &callDeduper{seen: make(map[string]int)}
}

// firstSeenRound 返回该调用此前成功执行时的轮次与 true；从未成功执行过则返回 (0, false)。
func (d *callDeduper) firstSeenRound(tc schema.ToolCall) (int, bool) {
	round, ok := d.seen[callFingerprint(tc)]
	return round, ok
}

// record 登记一次成功执行的调用。重复登记时保留最早的轮次。
func (d *callDeduper) record(tc schema.ToolCall, round int) {
	fp := callFingerprint(tc)
	if _, ok := d.seen[fp]; ok {
		return
	}
	d.seen[fp] = round
}

// repeatNotice 是回喂给模型的提醒文本，代替重复执行的结果。
//
// 措辞上明确三件事：这次调用被跳过了、原结果在哪一轮、以及接下来该做什么。
// 只说"重复了"而不给出路，模型很可能换个等价写法再查一遍。
func repeatNotice(tc schema.ToolCall, round int) string {
	return fmt.Sprintf(
		"已跳过：你在第 %d 轮就用完全相同的参数调用过 %s，那次的结果还在上面的对话里。"+
			"回头重读那条结果，不要再搜一遍。"+
			"如果判断这批改动所需的信息你已经有了，现在就调用 %s。",
		round, tc.Name, "submit_review")
}

// callFingerprint 为一次工具调用算出指纹：工具名 + 规范化后的参数。
//
// 参数要先规范化再比较，否则 {"path":"a.go"} 和 {"path": "a.go"} 这种只差空格、
// 或者键顺序不同的等价调用会被当成两次不同的调用漏过去——模型每轮重新序列化
// 参数，键顺序本来就没有保证。规范化失败（参数不是合法 JSON）时退回用原始
// 字节，宁可漏判也不能误判：把两次不同的调用错认成重复，会让模型拿不到它
// 真正需要的结果。
func callFingerprint(tc schema.ToolCall) string {
	sum := sha256.Sum256(append([]byte(tc.Name+"\x00"), canonicalJSON(tc.Arguments)...))
	return hex.EncodeToString(sum[:])
}

// canonicalJSON 把一段 JSON 重新序列化成键有序、无多余空白的形式。
// 解析失败时原样返回输入。
func canonicalJSON(raw json.RawMessage) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	var b []byte
	return appendCanonical(b, v)
}

// appendCanonical 按"map 的键排序、其余原样"的规则序列化 v。
//
// 不直接用 json.Marshal：encoding/json 确实会对 map[string]any 的键排序，
// 但那是它的实现细节而非契约，指纹的正确性不该押在这上面。
func appendCanonical(b []byte, v any) []byte {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b = append(b, '{')
		for i, k := range keys {
			if i > 0 {
				b = append(b, ',')
			}
			kb, _ := json.Marshal(k)
			b = append(b, kb...)
			b = append(b, ':')
			b = appendCanonical(b, val[k])
		}
		return append(b, '}')
	case []any:
		b = append(b, '[')
		for i, item := range val {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendCanonical(b, item)
		}
		return append(b, ']')
	default:
		vb, err := json.Marshal(val)
		if err != nil {
			return append(b, "null"...)
		}
		return append(b, vb...)
	}
}

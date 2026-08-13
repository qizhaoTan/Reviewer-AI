package tool

import "github.com/qizhaoTan/Reviewer-AI/internal/review"

// Result 是单次工具调用的返回值：Output 是给模型看的文本，
// IsError 标记这次调用是否失败。工具只对"给定输入产出什么结果"负责，
// 不关心调用方的 ToolCallID 之类的调度信息，那属于调用方（如 engine）的职责。
type Result struct {
	Output  string
	IsError bool

	// ReviewResult 是这次工具调用产出的结构化审查结果，只有 submit_review 会填，
	// 其余工具留 nil。
	//
	// 为什么不塞进 Output：Output 的职责是"回喂给模型看的文本"，把 JSON 序列化
	// 进去等于让一个面向模型的字段兼任程序间的数据总线，两种职责混在一起。
	//
	// 为什么不让工具持有一个输出指针：那样工具就有了状态，多个审查链路
	// （阶段五的并发子 Agent）共享同一个实例时会互相覆盖，只能每条链路各 new
	// 一个。走返回值则工具保持无状态，可以安全共享单个实例。
	//
	// 为什么不用 context 传递：context 传业务数据会把编译期可查的依赖变成运行期
	// 才发现的——取不到时只能返回一个工具错误，问题会伪装成"模型提交失败"，
	// 排查要绕一大圈。
	//
	// 代价是公共的 Result 里多了一个只服务于单个工具的字段。这个代价可见且有界，
	// 换来的是：调用方靠 ReviewResult != nil 就能判断审查是否已提交，
	// 不必按工具名做字符串比较。
	ReviewResult *review.Report

	// CritiqueVerdict 是复核者对单条意见的裁决，只有 submit_verdict 会填，
	// 其余工具留 nil。语义和取舍同 ReviewResult——复核循环靠它非 nil
	// 判断这一条已经裁决完毕。
	CritiqueVerdict *Verdict
}

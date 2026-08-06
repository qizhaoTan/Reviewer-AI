package tool

// Result 是单次工具调用的返回值：Output 是给模型看的文本，
// IsError 标记这次调用是否失败。工具只对"给定输入产出什么结果"负责，
// 不关心调用方的 ToolCallID 之类的调度信息，那属于调用方（如 main.go）的职责。
type Result struct {
	Output  string
	IsError bool
}

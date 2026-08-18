package tool

import (
	"os/exec"
)

// searchTimeoutSeconds 是 glob/grep 单次调用的超时秒数。
// 与 main.go 里包住整个 tool loop 的大超时是独立的两层：
// 单次搜索卡住不该拖垮整场审查的预算，所以给一个短得多的固定值。
const searchTimeoutSeconds = 10

// maxGlobResults 与 maxGrepResults 参考 Claude Code 的 Glob/Grep 工具设计：
// 结果超出上限时提示模型缩小范围，而不是静默截断（避免模型误以为已经看到全部结果）。
const (
	maxGlobResults = 100
	maxGrepResults = 250
)

// ignoredDirNames 是 glob/grep 遍历仓库时默认跳过的目录名。
// 后续可迭代方向：改为解析 .gitignore（ripgrep 原生支持，这里先用硬编码黑名单代替，
// 因为自实现 .gitignore 解析器的工作量不小，不是当前阶段的重点）。
var ignoredDirNames = map[string]bool{
	".git":         true,
	".svn":         true,
	".hg":          true,
	"node_modules": true,
	"vendor":       true,
}

// isHiddenName 判断一个文件/目录名是否是隐藏项（以 "." 开头）。
// glob/grep 两条路径统一"不搜隐藏文件"这一语义：rg 路径靠 rg 的默认行为（即使加了
// --no-ignore 也不会加 --hidden），标准库回退路径靠本函数在遍历时跳过。
func isHiddenName(name string) bool {
	return len(name) > 1 && name[0] == '.'
}

// ripgrepAvailable 判断当前 PATH 上是否有可用的 ripgrep(rg) 二进制。
// glob/grep 两个工具都优先走 rg（更快、原生支持 .gitignore），
// 没有 rg 时各自回退到标准库实现的功能子集。
//
// 后续可迭代方向：像 Claude Code 那样做三级兜底（system → go:embed 内置 → 按平台分发的
// vendor 二进制），第一版先只做 system 检测 + 标准库兜底，跨平台打包留到真有分发需求时再做。
func ripgrepAvailable() bool {
	_, err := exec.LookPath("rg")
	return err == nil
}

# Reviewer-AI Development Guide

## 产品范围

Reviewer-AI 是一个用大语言模型审查 Git 暂存区变更的 Go 命令行工具。端到端流程已经实现完毕：加载配置 → 通过 `git diff --cached` 采集暂存区变更 → 拼装审查提示词 → 调用配置好的 provider（兼容 OpenAI 或 Anthropic 协议）→ 模型按需调用只读工具（`glob`、`grep`、`read_file`）补充上下文 → 把最终的 Markdown 审查意见打印到终端。

这还是个早期、持续迭代中的工具。主流程已经跑通，但工具集、提示词措辞、输出格式都预期会继续演进——见下方"迭代计划 / 待办事项"。

## 架构

- `internal/gitdiff` —— 确定性地采集暂存区变更（`git diff --cached`）。不涉及任何大模型逻辑，也要保持这样。
- `internal/schema` —— `provider` 和 `tool` 共用的 `Message`/`ToolCall`/`ToolDefinition` 类型定义，是大模型层和其他一切的契约。
- `internal/provider` —— 把 OpenAI 和 Anthropic 的 SDK 统一封装到一个 `schema.IProvider` 接口后面（`Generate(ctx, messages, tools) (*Message, error)`）。两个 provider 都已经实现了工具调用与各自原生协议的双向翻译。
- `internal/config` —— 加载 `~/.reviewer/config.json`（JSON 格式，可用 `-config` 参数或 `$REVIEWER_AI_CONFIG` 覆盖路径），把 `roles.primary` 对照 `models` map 解析成 `provider.Config`。
- `internal/prompt` —— 把暂存区变更拼装成初始的 `[system, user]` 消息对。system prompt 的措辞集中定义在这里，方便单独迭代而不影响调用方。
- `internal/tool` —— 模型可调用的只读工具，每个都实现 `ITool` 接口（`Definition() schema.ToolDefinition` + `Execute(ctx, repoRoot, args) Result`）。`FindToolByName` 负责分发，所以 `cmd/reviewer-ai` 不需要为每个工具写一个 `switch` 分支。目前有 `ReadFileTool`、`GlobTool`、`GrepTool`。
- `internal/log` —— 基于 `log/slog` 的结构化日志，用于让工具调用过程可观测（工具名、参数、成功/失败）。
- `cmd/reviewer-ai` —— 审查主循环：调用 provider，执行返回的工具调用，把结果作为工具结果消息喂回去（`schema.Message{Role: RoleUser, ToolCallID: ...}`），如此循环，直到模型给出不再携带工具调用的答案，或触达 30 轮的安全上限。

## 工程约定

- 保持 `internal/gitdiff` 的确定性，不引入任何大模型相关的逻辑。
- 新增工具时实现 `tool.ITool`，然后注册进 `cmd/reviewer-ai/main.go` 里的 `[]tool.ITool` 切片。不要重新引入 `switch tc.Name` 式的分发——这正是引入 `ITool`/`FindToolByName` 想要消除的"两处维护、容易漏改"问题。
- 工具的错误信息必须同时说明"错在哪"和"该怎么改"（例如"path escapes the repository root ... provide a path relative to the repo root"），不能只说失败了。模型是靠这些错误信息自我纠正的，含糊的错误信息会多耗费一轮调用。
- `glob`、`grep` 优先使用 `PATH` 上的 `rg`（ripgrep）二进制（如果存在），但必须始终保留标准库回退实现，确保没有外部依赖时工具也能用。不要在没有讨论过的情况下引入新的外部二进制依赖。
- 为每一个输入边界行为写测试，尤其是暂存区 vs 未暂存改动的区分，以及 `internal/tool` 里每一条路径安全分支（路径穿越、软链接逃逸、二进制文件、超大文件）。
- 所有 Go 单元测试都用表驱动风格 + 具名子测试，即使只有一个用例也要写成表驱动的形式，方便以后加用例时不用重构。
- 优先使用 Go 标准库，除非确实有明确需求才引入外部依赖。
- 提交改动前跑 `go build ./...`、`go test ./...`、`gofmt -l .`（应该没有任何输出）、`go vet ./...`。
- 在子目录新增 `CLAUDE.md` 时，要在同一目录创建 `AGENTS.md` 作为相对路径软链接指向它（`ln -s CLAUDE.md AGENTS.md`）。

## 迭代计划 / 待办事项

以下是已知但刻意延后的缺口，不是没想到，而是第一版故意不做：

- **`.gitignore` 感知搜索** —— `glob`/`grep` 目前只是跳过硬编码的目录黑名单（`.git`、`node_modules`、`vendor` 等），没有真正解析 `.gitignore`。
- **更多 `grep` 输出模式** —— 目前只有 `files_with_matches` 和 `content` 两种，`count` 模式和 `-A/-B/-C` 上下文行数参数还没实现。
- **结构化审查输出** —— 模型的回答目前是自由格式的 Markdown 直接打印到终端，还没有 `{file, line, severity, comment}` 这类结构化输出。
- **子 Agent / 并发审查** —— 最初设想的"针对大改动派生多个子 Agent 并发审查不同关注点"还没开始做。
- **ripgrep 分发** —— 目前只用系统已安装的 `rg`，没有内置/按平台打包的兜底二进制（这块工作量不小，等真正有跨机器分发需求时再做）。

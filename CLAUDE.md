# Reviewer-AI Development Guide

## 产品范围

Reviewer-AI 是一个用大语言模型审查 Git 暂存区变更的 Go 命令行工具。端到端流程：加载配置 → 采集暂存区变更（`git diff --cached`）→ 拼装提示词 → 调用 provider（兼容 OpenAI 或 Anthropic 协议）→ 模型按需调用只读工具补充上下文 → 模型调用 `submit_review` 提交**结构化意见** → 每条意见并发跑一轮**复核**过滤误报 → 渲染 Markdown 到终端，同时落盘到本地 SQLite。

另有一个 `cr web` 子命令，提供只读的历史浏览界面，并支持对单条意见追问、触发增量重审。

这仍是个早期、持续迭代中的工具。核心流程已稳定，提示词措辞和输出格式预期会继续演进——见下方"迭代计划 / 待办事项"。

## 架构

**确定性层（不含任何大模型逻辑）**

- `internal/gitdiff` —— 采集暂存区变更（`git diff --cached`）、`StageAll`、`CurrentBranch`。保持确定性，不要引入大模型逻辑。
- `internal/schema` —— `Message`/`ToolCall`/`ToolDefinition` 类型定义，是大模型层与其他一切的契约。
- `internal/config` —— 加载 `~/.reviewer/config.json`（`-config` 参数或 `$REVIEWER_AI_CONFIG` 可覆盖路径），把 `roles.primary` 对照 `models` map 解析成 provider 配置。
- `internal/store` —— SQLite（`modernc.org/sqlite`，纯 Go 无 CGO）持久化运行记录，默认 `~/.reviewer/runs.db`。
- `internal/log` —— 基于 `log/slog` 的结构化日志，让工具调用过程可观测。

**大模型层**

- `internal/provider` —— 把 OpenAI 和 Anthropic 的 SDK 统一封装到 `schema.IProvider` 接口后面（`Generate(ctx, messages, tools) (*Message, error)`），双向翻译工具调用与各自原生协议。
- `internal/prompt` —— 拼装各阶段的提示词。措辞集中定义在这里，方便单独迭代而不影响调用方。
- `internal/tool` —— 模型可调用的工具，每个实现 `ITool`（`Definition()` + `Execute(ctx, repoRoot, args) Result`）。`FindToolByName` 负责分发。
  - 只读工具（三阶段共用）：`ReadFileTool`、`GlobTool`、`GrepTool`
  - 收尾工具（每阶段专属）：`SubmitReviewTool`（`submit_review`）、`CritiqueVerdictTool`（`submit_verdict`）、`WithdrawFindingTool`（`withdraw_finding`）

**编排层**

- `internal/review` —— `Finding` / `Report` 结构体（`file`/`start_line`/`end_line`/`severity`/`summary`/`detail`/`kept`/`status`）、Markdown 渲染（`render.go`）、行号锚定（`anchor.go`）、增量重审的快照比较（`incremental.go`）。
- `internal/engine` —— 编排一次完整运行：`engine.go` 驱动 agentic 循环并在每个节点落盘；`critique.go` 并发复核；`dedup.go` 拦截重复的工具调用；`recoverable.go` 区分可续跑与致命错误；`interactive.go` / `discuss.go` 支撑 Web 侧的追问与重审。engine 不导入 config/flag——依赖由调用方组装成 `Deps` 传入。
- `internal/web` —— 只读历史浏览 + 追问 + 增量重审的 HTTP 服务（`/`、`/run`、`/reply`、`/rereview`、`/delete`、`/delete-all`）。模板内联在 `templates.go`。
- `cmd/reviewer-ai` —— 子命令分发（默认审查 / `web` / `version`）与依赖装配。

## 关键设计决策

这些是踩过坑之后的选择，改动前先理解原因：

- **运行记录按内容 hash 匹配，而非"最近一条"** —— 覆盖 `git stash` / `stash pop` 后内容回到原样的场景。只要内容相同就能命中历史记录，命中 completed 的直接复用、不调模型。
- **runKey 包含分支名** —— 否则在分支 A 中断的审查，切到分支 B 后会被误判为同一次运行继续恢复，模型会拿着 A 的 diff 去审 B 的暂存区。
- **复核用独立的 ctx，不共享初审的超时预算** —— 复核是一批全新的模型调用，不该去分初审剩下的时间。
- **复核失败不把运行标记为 failed** —— 留在 `in_progress`，初审结果仍然有效，下次可续跑。
- **工具调用去重** —— 实测模型判定改动无误后不会收工，而是反复搜同一个符号（20 行改动里同一个常量被 grep 6 次），把审查从几轮拖到二十几轮。这不是上下文丢失，历史结果确实还在上下文里，模型只是没去看。
- **`readOnlyTools()` 每次返回新切片** —— 调用方会往返回值上 append 各自的收尾工具，共享底层数组会让两次 append 互相覆盖。

## 工程约定

- 保持 `internal/gitdiff` 的确定性，不引入任何大模型相关的逻辑。
- 新增工具时实现 `tool.ITool`，然后注册进 `cmd/reviewer-ai/main.go`。不要重新引入 `switch tc.Name` 式的分发——这正是引入 `ITool`/`FindToolByName` 想消除的"两处维护、容易漏改"问题。
- 工具的错误信息必须同时说明"错在哪"和"该怎么改"（例如 "path escapes the repository root ... provide a path relative to the repo root"），不能只说失败了。模型靠这些错误信息自我纠正，含糊的错误信息会多耗一轮调用。
- `glob`、`grep` 优先使用 `PATH` 上的 `rg`，但必须始终保留标准库回退实现。不要在没有讨论过的情况下引入新的外部二进制依赖。
- 为每一个输入边界行为写测试，尤其是暂存区 vs 未暂存改动的区分，以及 `internal/tool` 里每一条路径安全分支（路径穿越、软链接逃逸、二进制文件、超大文件）。
- 所有 Go 单元测试都用表驱动风格 + 具名子测试，即使只有一个用例也写成表驱动的形式，方便以后加用例时不用重构。
- 优先使用 Go 标准库，除非确实有明确需求才引入外部依赖。
- 提交改动前跑 `go build ./...`、`go test ./...`、`gofmt -l .`（应无输出）、`go vet ./...`。
- 在子目录新增 `CLAUDE.md` 时，要在同一目录创建 `AGENTS.md` 作为相对路径软链接指向它（`ln -s CLAUDE.md AGENTS.md`）。

## 开源相关

- 仓库以 Apache-2.0 发布。新增源文件不需要加 license header（当前仓库没有这个惯例），但不要引入与 Apache-2.0 不兼容的依赖。
- **不要把主机地址、内网 IP、部署路径写进仓库。** 分发目标一律放在未跟踪的 `deploy.mk` 里（模板见 `deploy.mk.example`）。
- `docs/roadmap-v2*.md` 是内部规划文档，已在 `.gitignore` 中，不要提交。

## 迭代计划 / 待办事项

已知但刻意延后的缺口：

- **标准库回退路径的 `.gitignore` 感知** —— 目前只有 ripgrep 路径尊重 `.gitignore`（并支持 `no_ignore` 参数）；标准库回退只跳过硬编码的目录黑名单（`.git`、`.svn`、`.hg`、`node_modules`、`vendor`），导致装了 `rg` 和没装 `rg` 的搜索结果可能不一致。
- **更多 `grep` 输出模式** —— 目前只有 `files_with_matches` 和 `content`，`count` 模式和 `-A/-B/-C` 上下文行参数还没实现。
- **ripgrep 分发** —— 目前只用系统已安装的 `rg`，没有内置/按平台打包的兜底二进制。

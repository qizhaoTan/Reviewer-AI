# Reviewer-AI

用大语言模型审查 Git 暂存区变更的命令行工具。在 `git commit` 之前，先让模型读一遍你的改动。

与"把 diff 丢给聊天窗口"的区别在于：模型可以调用只读工具（glob / grep / read_file）主动补充 diff 之外的上下文，产出的是**结构化意见**（文件、行号、严重级别），并且每条意见都会经过一轮独立的**并发复核**来过滤误报。审查记录落在本地 SQLite 里，可以用内置的 Web 界面回看、追问、增量重审。

> 早期项目，仍在迭代中。核心流程已稳定，提示词与输出格式还会继续调整。

## 工作流程

```
git add -A
    ↓
采集暂存区 diff（git diff --cached）
    ↓
初审：模型调用 glob/grep/read_file 补充上下文，
      最终调用 submit_review 提交结构化意见
    ↓
复核：每条意见派生一个独立的子任务并发裁决（keep / drop），
      过滤掉初审的误报
    ↓
输出 Markdown 到终端 + 落盘到 ~/.reviewer/runs.db
```

## 特性

- **结构化意见** —— 每条意见带 `file` / `start_line` / `end_line` / `severity` / `summary` / `detail`，不是一坨自由格式文本。
- **并发复核过滤误报** —— 初审产出的每条意见都会被单独送去复核，模型给出 keep/drop 裁决和理由。并发度可配置。
- **可续跑** —— 审查按内容 hash 落盘。中断后重跑会恢复已有的对话历史继续，而不是从零开始重问一遍。同一份内容审查过就直接复用结果，不再调用模型。
- **分支隔离** —— 运行记录按 `仓库路径 + 分支名` 隔离，切分支不会串台。
- **增量重审** —— 改完代码再审时，patch 逐字节未变的文件直接复用上次意见，只把真正改动过的文件送给模型。
- **Web 界面** —— 回看历史审查、对单条意见追问（模型可调用 `withdraw_finding` 撤回自己的意见）、触发增量重审。
- **双协议支持** —— 兼容 OpenAI 和 Anthropic 两套协议，任何兼容这两者的服务都能接。
- **工具调用去重** —— 拦截模型重复发起的完全相同的工具调用，避免它在同一个符号上反复 grep 把轮数拖长。

## 安装

```bash
go install github.com/qizhaoTan/Reviewer-AI/cmd/reviewer-ai@latest
```

或从源码构建（产物为 `cr`）：

```bash
git clone https://github.com/qizhaoTan/Reviewer-AI.git
cd Reviewer-AI
make            # 等价于 make regenerate，产出 ./cr
```

需要 Go 1.25+。

## 配置

创建 `~/.reviewer/config.json`（可用 `-config` 参数或 `$REVIEWER_AI_CONFIG` 环境变量指定其他路径）：

```json
{
  "models": {
    "gpt": {
      "provider": "openai",
      "api_key": "sk-...",
      "base_url": "https://api.openai.com/v1",
      "model": "gpt-5",
      "context_window": 400000,
      "max_tokens": 32000,
      "timeout_seconds": 300
    },
    "claude": {
      "provider": "anthropic",
      "api_key": "sk-ant-...",
      "base_url": "https://api.anthropic.com",
      "model": "claude-opus-4-6",
      "max_tokens": 32000,
      "timeout_seconds": 300
    }
  },
  "roles": {
    "primary": "claude"
  },
  "critique": {
    "concurrency": 4,
    "max_turns": 12
  },
  "auto_stage": true
}
```

| 字段 | 说明 |
| --- | --- |
| `models` | 一组具名模型配置。`provider` 只能是 `openai` 或 `anthropic`，决定用哪套协议说话。 |
| `roles.primary` | 当前生效的是 `models` 里的哪一个。 |
| `critique.concurrency` | 复核阶段的并发数。 |
| `critique.max_turns` | 单条意见复核时允许的最大工具调用轮数。 |
| `auto_stage` | 为 true 时审查前自动 `git add -A`。 |
| `language_prompt` | 可选，覆盖默认的输出语言指令。 |

## 使用

```bash
# 审查当前仓库的暂存区
cr

# 审查指定仓库
cr -repo /path/to/repo

# 忽略可续跑的记录，从头重新审查
cr -fresh

# 启动 Web 界面回看历史（默认 :8090，自动打开浏览器）
cr web
cr web -addr :9000 -no-open

# 版本号
cr version
```

审查结果同时打印到终端并落盘到 `~/.reviewer/runs.db`。

## 模型可用的工具

初审、复核、追问三个阶段共用一组只读工具：

| 工具 | 用途 |
| --- | --- |
| `glob` | 按模式发现文件（如 `internal/**/*.go`），上限 100 条 |
| `grep` | 按正则搜索文件内容，支持 `files_with_matches` / `content` 两种输出模式，上限 250 条 |
| `read_file` | 读取文件全文或指定行范围 |

各阶段另有一个专属的收尾工具：初审用 `submit_review` 提交结构化意见，复核用 `submit_verdict` 对单条意见下裁决，Web 追问用 `withdraw_finding` 撤回意见。

`glob` / `grep` 优先使用 `PATH` 上的 `rg`（ripgrep），没有时回退到标准库实现——不装 ripgrep 也能正常工作。两条路径的过滤行为略有差别：

- **有 `rg`**：默认排除 `.gitignore` 忽略的文件，两个工具都接受 `no_ignore` 参数来包含它们。
- **无 `rg`（标准库回退）**：不解析 `.gitignore`，只跳过硬编码的目录黑名单（`.git`、`.svn`、`.hg`、`node_modules`、`vendor`），`no_ignore` 参数在这条路径上不生效。

两条路径都不搜索隐藏文件（名字以 `.` 开头的文件和目录）。

## 分发（可选）

`make dist` / `make upload` 会把交叉编译产物 scp 到你自己的机器。主机地址不入库，复制模板后按需填写：

```bash
cp deploy.mk.example deploy.mk
$EDITOR deploy.mk       # 已在 .gitignore 中
make upload
```

其他构建目标：`make win` / `linux` / `mac` / `mac-arm` / `cross` / `test` / `clean`。

## 开发

```bash
go build ./...
go test ./...
gofmt -l .        # 应无输出
go vet ./...
```

包结构与工程约定见 [CLAUDE.md](CLAUDE.md)。

## 已知缺口

刻意延后、不是没想到：

- **标准库回退路径不解析 `.gitignore`** —— 只跳过硬编码的目录黑名单，导致装了 `rg` 和没装 `rg` 的搜索结果可能不一致。
- `grep` 缺 `count` 输出模式和 `-A/-B/-C` 上下文行参数。
- 没有内置按平台打包的 ripgrep 兜底二进制，只用系统已装的 `rg`。

## License

[Apache License 2.0](LICENSE)

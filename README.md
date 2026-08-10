# Reviewer-AI

Reviewer-AI 是一个 Go 编写的命令行工具，在你提交代码前，用大语言模型审查 Git 暂存区的变更。

## 它做了什么

1. 读取 Git 索引（`git diff --cached`）——刻意排除未暂存的工作区改动。
2. 用暂存区的 diff 拼装审查提示词。
3. 发给配置好的大模型 provider（兼容 OpenAI 或 Anthropic 协议的服务均可）。
4. 模型可以调用三个只读工具来补充 diff 之外的上下文：
   - `glob` —— 按模式发现仓库里有哪些文件（如 `internal/**/*.go`）
   - `grep` —— 按正则表达式搜索文件内容
   - `read_file` —— 读取指定文件的完整内容或某个行范围
5. 把模型给出的最终审查意见以 Markdown 格式打印到终端。

这还是个早期、持续迭代中的工具：审查主流程已经完整跑通，但工具集、提示词、输出格式都还会继续演进。

## 使用步骤

### 1. 编译

```bash
go build -o reviewer ./cmd/reviewer-ai
```

开发阶段也可以直接 `go run ./cmd/reviewer-ai`。

### 2. 配置

创建 `~/.reviewer/config.json`（也可以用 `-config` 参数或 `$REVIEWER_AI_CONFIG` 环境变量指定其他路径）。这个文件声明一组具名的模型配置，并指定当前生效的是哪一个：

```json
{
  "models": {
    "openai-fast": {
      "provider": "openai",
      "api_key": "sk-...",
      "base_url": "https://api.openai.com/v1",
      "model": "gpt-4.1",
      "context_window": 128000,
      "max_tokens": 32000,
      "timeout_seconds": 180
    },
    "claude": {
      "provider": "anthropic",
      "api_key": "sk-ant-...",
      "base_url": "https://api.anthropic.com",
      "model": "claude-sonnet-5",
      "max_tokens": 32000,
      "timeout_seconds": 180
    }
  },
  "roles": {
    "primary": "openai-fast"
  }
}
```

- `roles.primary` 决定本次审查用 `models` 里的哪一条配置——这是你自己起的别名，不是 provider 名字。
- `provider` 取值 `"openai"` 或 `"anthropic"`；两者都支持自定义 `base_url`，所以任何兼容 OpenAI 或 Anthropic 协议的服务都能接。
- `timeout_seconds` 限定的是整次审查运行的总预算（tool loop 里所有模型调用共用这一个预算），不是单次 HTTP 调用的超时。
- `context_window` 目前只是解析进来，还没有任何逻辑消费它。
- 文件必须是标准 JSON，不能有多余的逗号。

### 3. 运行

在一个有暂存区改动的 Git 仓库里执行：

```bash
reviewer
# 或者开发阶段：
go run ./cmd/reviewer-ai
```

参数：

| 参数 | 默认值 | 含义 |
|---|---|---|
| `-config` | `~/.reviewer/config.json`（或 `$REVIEWER_AI_CONFIG`） | 配置文件路径 |
| `-repo` | `.` | 要审查的 Git 仓库路径 |

暂存区为空时，会打印 `No staged changes to review.` 并以退出码 0 结束。

工具调用过程会实时打印到终端（工具名、参数、是否成功），方便你观察模型在形成审查意见时都拉取了哪些上下文。

## 架构

| 包 | 职责 |
|---|---|
| `internal/gitdiff` | 通过 `git diff --cached` 确定性地采集暂存区变更，不涉及任何大模型逻辑。 |
| `internal/config` | 加载 `config.json`，把 `roles.primary` 解析成生效的 `provider.Config`。 |
| `internal/provider` | 把 OpenAI 和 Anthropic 的 SDK 统一封装到一个 `schema.IProvider` 接口后面（`Generate(ctx, messages, tools)`）。 |
| `internal/schema` | `provider` 和 `tool` 共用的消息/工具调用类型定义。 |
| `internal/prompt` | 把暂存区变更拼装成初始的 `[system, user]` 消息对。 |
| `internal/tool` | 模型可调用的只读工具（`glob`、`grep`、`read_file`），统一实现 `ITool` 接口。 |
| `internal/log` | 基于 `log/slog` 的结构化日志，用于展示工具调用过程。 |
| `cmd/reviewer-ai` | 把以上所有模块串成审查主流程：加载配置 → 采集 diff → 拼装提示词 → 调用 provider → 执行工具调用 → 循环直到给出最终答案。 |

### 审查主循环

`cmd/reviewer-ai` 的主循环调用 provider，如果返回结果里带有工具调用，就执行它们并把结果作为工具结果消息喂回去，如此循环，直到模型给出不再携带工具调用的纯文本回答，或触达 30 轮的安全上限。

### 工具安全性

`read_file`、`glob`、`grep` 都是只读的，且被限制在仓库根目录内：
- `read_file` 拒绝绝对路径、`..` 路径穿越，以及指向仓库外的软链接；同时拒绝二进制文件，并对文件大小设上限（超大文本文件会被截断并给出明确提示）。
- `glob`/`grep` 优先使用 `PATH` 上的 `rg`（ripgrep）二进制（如果存在），没有时回退到标准库实现（自己写了一个支持 `**` 的 glob 匹配器，因为标准库 `filepath.Match` 不支持这个语法）。两者都会跳过 `.git`、`node_modules`、`vendor` 等目录，并对结果数量设上限，超出时给出"请缩小范围"的明确提示而不是静默截断。
- 每一条工具错误信息都同时说明"错在哪"和"该怎么改"，让模型能自我纠正，而不是卡住。

## 开发

```bash
go build ./...
go test ./...
gofmt -l .   # 应该没有任何输出
go vet ./...
```

工程约定和当前迭代计划见 `CLAUDE.md`。

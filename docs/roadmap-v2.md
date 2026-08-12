# Reviewer-AI v2 迭代设计文档

## Context

v1 已经把审查主流程完整跑通：加载配置 → 采集暂存区 diff → 拼装提示词 → 调用 provider → 模型按需调用 `read_file`/`glob`/`grep` 三个只读工具 → 打印 Markdown 审查报告到终端。这是一个"一次运行、一次输出"的批处理型 CLI。

用起来之后暴露的下一层需求是五个方向：结构化输出、子 Agent 并发审查、用户交互（reply/review）、审查流程可视化、持久化。这五个方向不是五个孤立的功能，而是同一个"从批处理工具进化成有状态的审查助手"目标的五个切面，彼此有强依赖：

- **持久化**是地基——没有持久化，就没法做增量重审（不知道上次审查到哪）、没法做 Web 可视化（没有历史可看）、没法做用户交互后的状态回放（reply/review 之后进程可能已经退出）。
- **结构化输出**是复审和用户交互的前提——用户要 reply/review 某一条具体意见，程序得先知道"意见"在数据结构上长什么样、有没有唯一 ID 能被引用。
- **Web 可视化**在结构化输出和持久化之后顺理成章——本质是把 SQLite 里的历史数据渲染出来，参考 `/Users/tan/work/hq/ai-assistant` 的 `web` 包模式（服务端渲染、零前端依赖、通过小接口跟业务逻辑解耦）。
- **用户交互**（reply/review + 增量重审）依赖前三者都到位：需要结构化的问题项作为 reply/review 的操作对象，需要持久化的会话来承载多轮对话，需要 diff 快照作为增量重审的基线。
- **子 Agent 并发审查**放在最后，因为它是规模/性能维度的优化，不影响前四项的正确性，且设计上是独立的（模型自主拆分 + 程序校验执行），可以在前面的地基稳固之后再叠加。

因此确定的开发顺序是：

**持久化 → 结构化输出 + 复审 → Web 可视化 → 用户交互（reply/review + 增量重审）→ 子 Agent 并发审查**

## 关键设计决策（已与 Tan 确认）

1. **运行形态**：`reviewer` CLI 保持一次性运行（审查完退出）的定位不变。交互层（reply/review 对话框）通过另一个常驻的 Web 服务承载，读写同一份持久化数据。不把整个工具改造成 daemon。
2. **增量重审基线**：持久化记录上次审查时每个文件的 diff 内容快照（`gitdiff.Change.Patch`）。重审时用新的 `git diff --cached` 结果与快照做"diff of diff"文本比较，只把真正变化的文件重新丢给模型审查，未变化的文件复用上次的审查结论。不依赖 git commit，纯粹基于已持久化的 diff 文本比较。
3. **复审机制**：第一版复审用同一个模型再跑一轮，把结构化的初审结果连同原始 diff 一起喂回去，让模型自我批判、过滤掉它自己认为低置信度/无根据的问题项。不引入独立的第二个"复审模型"配置项（那是后续可选项，现在不做）。
4. **子 Agent 拆分机制**：不由程序预设拆分维度（不是"按目录"也不是"按关注点"这种写死的规则）。给主 Agent 一个 `spawn_reviewers` 工具，工具的输入是模型自己决定的分组方案（每组包含一个 `focus` 说明和涉及的文件列表），模型看完完整 diff 后自主判断怎么分组最合理（可能按目录、可能按功能模块、可能按关注点，由模型灵活判断）。程序侧只做两件事：校验分组方案的合法性（文件列表不重不漏、都在本次 diff 范围内），以及并发执行审查、汇总结果。
5. **持久化存储**：SQLite（`modernc.org/sqlite`，纯 Go 实现无 cgo 依赖），复用 `/Users/tan/work/hq/ai-assistant/storage` 的设计模式（工具输出用占位符压缩存储、`created_at`/`updated_at` 索引、`LoadRecent` 按时间窗口恢复）。

## 阶段一：持久化（地基）

### 目标

把一次审查运行的完整状态（暂存区 diff 快照、发给模型的消息历史、最终结构化结果）落盘，支持中断后恢复，并为后续所有阶段提供数据基础。

### 设计

新增 `internal/store` 包（参考 `ai-assistant/storage` 的接口形状，但数据模型是 Reviewer-AI 自己的）：

```go
package store

// Run 是一次审查运行的完整持久化记录。
type Run struct {
    ID          string            // 唯一标识，建议用 repo 路径的 hash + 时间戳
    RepoPath    string
    Status      RunStatus         // pending / in_progress / completed / failed
    CreatedAt   time.Time
    UpdatedAt   time.Time
    Snapshot    []gitdiff.Change  // 本次审查时的暂存区快照（增量重审的基线）
    Messages    []schema.Message  // 发给模型的完整消息历史（含工具调用往返）
    // Findings 字段在阶段二（结构化输出）加入，阶段一先只做消息+快照持久化
}

type RunStatus string
const (
    StatusPending    RunStatus = "pending"
    StatusInProgress RunStatus = "in_progress"
    StatusCompleted  RunStatus = "completed"
    StatusFailed     RunStatus = "failed"
)

type Store struct { /* 内部持有 *sql.DB */ }

func New(dsn string) (*Store, error)
func (s *Store) Close() error

// SaveRun 幂等 upsert 一条运行记录（按 ID）。main.go 在每次 Generate 往返后调用，
// 保证中断后能从最后一次成功持久化的状态恢复。
func (s *Store) SaveRun(ctx context.Context, run Run) error

// LoadRun 按 ID 加载一条运行记录；不存在返回 (nil, nil)。
func (s *Store) LoadRun(ctx context.Context, id string) (*Run, error)

// LoadLatestRun 按 repoPath 查找最近一次记录（用于增量重审找基线，以及
// "reviewer -resume" 无需记住 ID 就能恢复最近一次中断的运行）。
func (s *Store) LoadLatestRun(ctx context.Context, repoPath string) (*Run, error)

// ListRuns 按 repoPath 列出历史运行（供 Web 可视化的列表页使用）。
func (s *Store) ListRuns(ctx context.Context, repoPath string, limit int) ([]Run, error)
```

`main.go` 的改动：
- 运行开始时，先按 `repoPath` 查有没有 `StatusInProgress` 的未完成记录 → 有则询问/自动进入"恢复模式"（用已持久化的 `Messages` 直接续跑 `Generate` 循环，不重新构建 prompt）。
- 每完成一轮 `Generate` + 工具调用（即 `msgs` 有变化时），调用 `SaveRun` 落盘一次（同步写，SQLite 本地磁盘毫秒级，不是性能瓶颈；参考 `ai-assistant/session.Session.PushMessage` 的同步持久化理由）。
- 审查正常结束时 `Status` 置为 `completed`；异常退出前（`fail()` 函数里）尽量置为 `failed` 并保留已有的部分消息历史。

工具输出的存储优化：参考 `ai-assistant/storage.replaceToolResultContents`，工具结果消息（`Role==RoleUser && ToolCallID != ""`）落盘时把 `Content` 替换成占位提示（如"内容已过期，如需要可重新调用工具"），恢复时如果模型还需要这些内容会重新调用工具获取，避免数据库因为大文件读取内容而膨胀。

### 涉及文件
- 新增 `internal/store/store.go`、`store_test.go`
- 修改 `cmd/reviewer-ai/main.go`：接入恢复逻辑 + 每轮落盘
- 新增依赖 `modernc.org/sqlite`

### 完成标准
- 手动测试：审查一个较大的 diff（触发多轮工具调用），中途用 `kill -9` 杀掉进程，重新运行 `reviewer -resume` 能从中断点继续，不重新调用已经问过的工具。
- 表驱动测试覆盖 `SaveRun`/`LoadRun`/`LoadLatestRun`/`ListRuns` 的基本行为和边界情况（记录不存在、重复 ID upsert）。

---

## 阶段二：结构化输出 + 复审

### 目标

把模型的审查意见从自由文本 Markdown 变成可编程处理的结构化数据（每条意见有文件、行号、严重程度、描述、唯一 ID），并加入复审环节过滤低置信度意见。

### 设计

**结构化意见的数据结构**（新增 `internal/review` 包）：

```go
package review

type Severity string
const (
    SeverityInfo    Severity = "info"
    SeverityWarning Severity = "warning"
    SeverityError   Severity = "error"
)

// Finding 是一条具体的审查意见。
type Finding struct {
    ID       string   `json:"id"`       // 本次运行内唯一，如 "f1"/"f2"，供用户交互阶段引用
    File     string   `json:"file"`
    Line     int      `json:"line,omitempty"`      // 0 表示不针对具体行（文件级/整体意见）
    Severity Severity `json:"severity"`
    Summary  string   `json:"summary"`             // 一句话概括
    Detail   string   `json:"detail,omitempty"`     // 详细说明/建议
}

// Report 是一次审查的完整结构化结果。
type Report struct {
    Findings []Finding `json:"findings"`
    Summary  string    `json:"summary"` // 整体评价（如"本次改动整体质量良好，X 个问题需要关注"）
}
```

**让模型产出结构化结果**：不再让模型直接输出自由 Markdown 作为最终答案，而是给它一个 `submit_review` 工具（跟 `read_file`/`glob`/`grep` 同属只读工具体系，但这个是"写"工具——它的 `Execute` 不做外部副作用，只是把参数解析成 `Report` 并终止循环）。`internal/prompt` 的 system prompt 改为要求模型最终必须调用 `submit_review(findings, summary)` 而不是直接回答文本。`main.go` 的循环检测到 `submit_review` 调用时，解析出 `Report`，跳出循环（而不是继续等待"无工具调用的最终文本"）。

这个改动同时简化了 v1 里"模型有没有说完"的判断逻辑——从"没有 ToolCalls 就是最终答案"变成"调用了 `submit_review` 就是最终答案"，更明确。

**复审**：`internal/review` 加一个 `Critique` 步骤：

```go
// Critique 把初审 Report 连同原始 diff 再次喂给同一个 provider，
// 要求它自我批判：过滤掉自己认为缺乏依据/置信度低的 Finding，
// 可以合并重复项，但不能新增 Finding（复审只做减法，不做增法，
// 避免复审阶段引入初审没看到的新臆测）。
func Critique(ctx context.Context, llm schema.IProvider, changes []gitdiff.Change, initial Report) (Report, error)
```

复审同样通过一次 `Generate` 调用完成，用专门的复审 prompt（"你是一个严格的复审者，下面是初审给出的问题列表和原始 diff，删除你认为没有根据或价值不大的项，只保留确有必要向用户提出的意见"），要求模型再次调用 `submit_review`（复用同一个工具/schema，保证输出格式一致）。

**输出**：复审后的 `Report` 既要落盘（阶段一的 `store.Run` 加一个 `Findings []review.Finding` 字段），也要渲染成人类可读的终端输出（可以简单地把 `Report` 格式化成 Markdown 列表打印，不需要花哨的终端 UI）。

### 涉及文件
- 新增 `internal/review/review.go`（`Finding`/`Report` 类型 + `Critique` 函数）、`review_test.go`
- 新增 `internal/tool/submitreview.go`（`SubmitReviewTool` 实现 `ITool`）+ 测试
- 修改 `internal/prompt/prompt.go`：system prompt 改为要求调用 `submit_review` 收尾
- 修改 `cmd/reviewer-ai/main.go`：循环终止条件从"无 ToolCalls"改为"调用了 submit_review"；调用 `review.Critique` 做复审；调用 `store.Run.Findings` 持久化
- 修改 `internal/store/store.go`：`Run` 加 `Findings` 字段

### 完成标准
- 真实审查一次，确认模型会调用 `submit_review` 而不是输出自由文本。
- 故意在 diff 里放一个容易被误判的改动（比如一个无害的变量重命名），观察复审是否能过滤掉初审对它的过度解读。
- 终端输出的结构化结果人类可读，格式清晰（按 severity 分组或排序）。

---

## 阶段三：审查流程可视化（Web）

### 目标

启动一个只读的常驻 Web 服务，展示历史审查记录：运行列表、每次运行的完整消息历史（含工具调用往返）、结构化审查结果，用于排查审查过程问题和后续优化提示词/工具。

### 设计

直接照搬 `/Users/tan/work/hq/ai-assistant/web` 的模式：`html/template` 服务端渲染，零前端依赖，通过两个小接口跟数据源解耦。

新增 `internal/web` 包：

```go
package web

// RunSource 提供历史运行记录的只读访问，由 internal/store.Store 实现。
type RunSource interface {
    ListRuns(ctx context.Context, repoPath string, limit int) ([]store.Run, error)
    LoadRun(ctx context.Context, id string) (*store.Run, error)
}

type Server struct { /* ... */ }
func NewServer(source RunSource, addr string) *Server
func (s *Server) ListenAndServe() error
```

页面：
- `/` —— 列表页：按仓库路径、时间倒序列出历史运行，每行显示状态、时间、Findings 数量摘要。
- `/run?id=` —— 详情页：完整消息历史（含每次工具调用的参数和结果，类似 ai-assistant 详情页的"完整上下文历史"）+ 结构化 Findings 列表（复审前/复审后对比，方便你观察复审到底过滤掉了什么）。

新增子命令 `reviewer web`（复用同一个二进制，加一个子命令而不是新建一个可执行文件）：

```bash
reviewer web -addr :8090
```

启动后常驻，只读，不需要鉴权（参考 ai-assistant 的"团队内部调试用，监听内网、无鉴权"定位——Reviewer-AI 目前是个人工具，同样从简）。

### 涉及文件
- 新增 `internal/web/server.go`、`templates.go`、`server_test.go`
- 修改 `cmd/reviewer-ai/main.go`：增加 `web` 子命令分发（用 `os.Args[1]` 简单判断，暂不引入 cobra；如果后续子命令变多再评估是否需要）

### 完成标准
- `reviewer web` 启动后，浏览器打开能看到阶段一、二跑出来的历史运行记录列表和详情。
- 详情页能看清每一轮工具调用的参数和结果，以及复审前后 Findings 的差异。

---

## 阶段四：用户交互（reply / review）+ 增量重审

### 目标

在 Web 界面上，用户可以针对某一条具体 Finding 做出两种反馈：
1. **reply**——用户认为这条意见不成立，通过对话说服模型撤回。
2. **review**——用户认可意见并已按建议修改代码、重新 stage，触发一次**增量**重审（只审查上次审查后真正变化的文件）。

### 设计

**数据模型扩展**（`internal/store`）：

```go
// FindingStatus 记录一条 Finding 在交互过程中的状态。
type FindingStatus string
const (
    FindingOpen        FindingStatus = "open"
    FindingWithdrawn   FindingStatus = "withdrawn"   // 经 reply 对话后模型同意撤回
    FindingAcknowledged FindingStatus = "acknowledged" // 用户 review 确认，等待/已完成修复验证
)

// Run 增加：
// Findings []FindingRecord  (取代阶段二的 []review.Finding，多了 Status 和对话历史)
type FindingRecord struct {
    review.Finding
    Status FindingStatus
    // Discussion 是围绕这条 Finding 的追加对话（用户 reply 的内容 + 模型的回应），
    // 复用 schema.Message 类型，保持跟主消息历史同一套结构。
    Discussion []schema.Message
}
```

**reply 流程**：
1. Web 详情页每条 Finding 旁边有"回复"输入框，用户写"我认为这里没问题，因为……"提交。
2. 后端把这段话包装成 `schema.Message{Role: RoleUser, Content: "...针对 Finding f3 的回复：<用户输入>"}`，连同该 Finding 的原始上下文（`Summary`/`Detail`/相关 diff 片段）一起发给 provider，要求模型判断"用户的理由是否站得住脚，站得住就调用一个 `withdraw_finding(id, reason)` 工具确认撤回，站不住就礼貌说明为什么坚持"。
3. 模型的回应（无论是撤回确认还是坚持理由）都追加进该 Finding 的 `Discussion`，持久化；如果模型调用了 `withdraw_finding`，该 Finding 的 `Status` 改为 `withdrawn`。
4. 这是一个小的独立 `Generate` 调用（不复用主审查循环），prompt 更聚焦（只给这一条 Finding 的上下文，不需要整个 diff 和其他工具）。

**review 流程（增量重审）**：
1. 用户点击"我已修复，重新审查"。
2. 后端拿当前仓库最新的 `git diff --cached` 结果，与该 `Run.Snapshot`（上次审查时的快照）逐文件比较 `Patch` 文本：
   - 文本相同 → 该文件复用上次的 Findings，不重新审查。
   - 文本不同或是新增/删除的文件 → 归入"需要重新审查"的文件集合。
3. 只把"需要重新审查"的文件集合按阶段二的流程重新走一遍（拼 prompt → Generate → 工具调用 → submit_review → Critique），得到一份新的部分 `Report`。
4. 合并结果：未变化文件的旧 Findings（排除已经 `withdrawn` 的）+ 新一轮审查产出的 Findings，写入一条新的 `Run` 记录（`ParentRunID` 字段指向上一条，形成审查历史链，Web 列表页可以展示这个演进关系）。

**Web 交互端点**：
- `POST /run/{id}/finding/{findingID}/reply` —— 提交 reply，同步返回模型回应。
- `POST /run/{id}/rereview` —— 触发增量重审（这个可能耗时较长，考虑返回一个新 `Run` 的 `pending` 记录 ID，前端轮询状态，或者简单点先做成同步阻塞直到完成——第一版建议同步阻塞，简单可靠，避免过早引入异步任务队列的复杂度）。

### 涉及文件
- 修改 `internal/store`：`FindingRecord`/`FindingStatus`、`Run.ParentRunID`
- 新增 `internal/review/discuss.go`：reply 对话的 `Generate` 封装
- 新增 `internal/review/incremental.go`：`DiffSnapshot(old, new []gitdiff.Change) (unchanged, changed []gitdiff.Change)` 增量比较逻辑 + 测试（这是本阶段除交互 UI 外最核心的纯逻辑，务必表驱动覆盖：文件新增/删除/修改/未变化/rename 等情况）
- 新增 `internal/tool/withdrawfinding.go`：`WithdrawFindingTool`
- 修改 `internal/web`：加 reply/rereview 的 POST handler 和对应页面元素

### 完成标准
- 针对一条 Finding 提交一个有说服力的 reply，观察模型是否正确调用 `withdraw_finding` 并在 Web 上看到状态变化。
- 修复一个 Finding 指出的问题、重新 stage 后触发 rereview，确认只有改动过的文件被重新送审（可以在日志/Web 详情页里看到"跳过未变化文件"的记录），未改动文件的 Findings 原样保留。

---

## 阶段五：子 Agent 并发审查

### 目标

大改动场景下，让主 Agent 自主判断是否需要拆分成多个子任务并发审查，而不是一个 Agent 顺序处理所有文件。

### 设计

新增一个只读性质的"编排"工具（`internal/tool/spawnreviewers.go`）：

```go
type SpawnReviewersInput struct {
    Groups []ReviewGroup `json:"groups"`
}
type ReviewGroup struct {
    Focus string   `json:"focus"` // 模型自己描述的这组关注点，如"数据库迁移相关的改动"
    Files []string `json:"files"` // 涉及的文件路径（必须是本次 diff 涉及的文件的子集）
}
```

`SpawnReviewersTool.Execute` 不是"执行后返回文本结果给同一个对话"，而是特殊路径：`main.go` 的循环识别到这个工具调用后，走一条不同的处理分支——

1. **校验**：`Groups` 里所有 `Files` 的并集必须等于本次 diff 涉及的全部文件（不多不少，不重复）；`Files` 长度为 0 的分组视为非法。校验失败时把清晰的错误信息作为工具结果喂回去，让模型重新调用（复用现有"工具错误要说明怎么改"的约定），不允许校验失败还继续往下走。
2. **并发执行**：校验通过后，为每个 `ReviewGroup` 独立起一个 goroutine，各自用该分组的 `Focus` + 对应文件的 `Change` 子集，走一遍完整的阶段二流程（拼 prompt → Generate → 工具循环 → submit_review → Critique），互不共享消息历史（每个子审查是独立的对话）。
3. **汇总**：所有分组完成后（用 `sync.WaitGroup` 或 `errgroup` 等待），把各组的 `Report.Findings` 合并成一份最终 `Report`（`File` 字段本来就有，合并后不需要额外加前缀区分来源；如果需要溯源可以加一个 `Finding.Group` 字段记录它来自哪个分组）。
4. 主 Agent 的这一轮 `Generate` 循环到这里就结束了——`spawn_reviewers` 调用即视为主 Agent 把审查工作"委托"出去，不再需要主 Agent 自己继续跑 `submit_review`。

**System prompt 调整**：告诉模型，如果 diff 涉及的文件数量较多、明显分属不同关注点，可以调用 `spawn_reviewers` 把审查工作拆分给多个并发子审查（每个子审查会独立看到它被分配到的文件的 diff），否则就按原来的方式自己审查完调用 `submit_review`。不强制模型必须拆分——小改动继续用单 Agent 路径，符合直觉。

**并发数控制**：加一个简单上限（比如最多 5 个并发分组），超过时报错让模型重新分组（分组数过多本身也说明拆分粒度不合理）。

### 涉及文件
- 新增 `internal/tool/spawnreviewers.go` + 测试（重点测校验逻辑：文件并集校验、空分组、超过并发上限）
- 修改 `cmd/reviewer-ai/main.go`：识别 `spawn_reviewers` 调用后的特殊分支处理（并发执行 + 汇总），这部分逻辑较适合抽成 `internal/review` 里的一个函数（如 `review.RunConcurrent(ctx, llm, groups) (Report, error)`），保持 `main.go` 精简
- 修改 `internal/prompt/prompt.go`：system prompt 里加入 `spawn_reviewers` 的使用说明

### 完成标准
- 构造一个涉及多个明显不相关模块的大改动（比如同时改 `internal/tool` 和 `internal/config`），观察模型是否会主动调用 `spawn_reviewers` 拆分，以及拆分后的 Findings 是否完整覆盖所有文件、没有重复审查。
- 构造一个小改动（1-2 个文件），确认模型不会画蛇添足地调用 `spawn_reviewers`，仍走单 Agent 路径。
- 恶意/异常分组方案（文件遗漏、文件重复、超过并发上限）能被正确拒绝并提示模型重新分组。

---

## 各阶段之间的依赖小结

```
阶段一 持久化
   └─▶ 阶段二 结构化输出 + 复审   （Run.Findings 依赖 Run 结构本身）
          └─▶ 阶段三 Web 可视化   （展示 Run 历史，含 Findings）
                 └─▶ 阶段四 用户交互 + 增量重审   （FindingRecord 依赖 Finding；增量重审依赖 Snapshot）
                        └─▶ 阶段五 子 Agent 并发   （复用阶段二的单组审查流程，独立于交互层）
```

阶段五虽然排在最后，但设计上只依赖阶段二的"单文件集合审查"流程被抽成可复用函数，不依赖阶段三、四——如果你后续想调整顺序（比如先做子 Agent 并发，交互层往后放），是可行的，文档里会保持这一点足够解耦。

# Reviewer-AI v2 开发 TODO List

配套文档：
- `docs/roadmap-v2.md` —— 每个 Step 的详细设计、涉及文件、决策依据，这里只列执行清单。
- `docs/design-notes-structured-output.md` —— **阶段二实现过程中偏离原设计的决策及推理**（anchor 取代行号、`Result.ReviewResult` 的选型、工具与 engine 的职责边界等）。与 `roadmap-v2.md` 冲突时以该文档为准。

沿用 v1 的开发方式：严格按顺序、每个阶段做完停下来梳理清楚再进入下一个阶段。

---

## 阶段一：持久化（地基）

- [x] **1.1** 引入 `modernc.org/sqlite` 依赖，新建 `internal/store/store.go`：`Run`/`RunStatus` 类型，`New`/`Close`/`SaveRun`/`LoadRun`/`LoadLatestRun`/`ListRuns`，建表 + 索引。
- [x] **1.2** 工具结果占位符压缩存储逻辑（参考 `ai-assistant/storage.replaceToolResultContents`）。
- [x] **1.3** 新建 `internal/store/store_test.go`：表驱动测试覆盖 `SaveRun` upsert 语义、`LoadRun` 不存在返回 nil、`LoadLatestRun`、`ListRuns` 排序与 limit。
- [x] **1.4** 修改 `cmd/reviewer-ai/main.go`：启动时查找同 repo 未完成的 `in_progress` 记录并支持恢复（新增 `-resume` 或自动检测，两种方式都行，先选一种能跑通的）；每轮 `Generate`+工具调用后落盘一次；正常/异常结束时更新 `Status`。
- [x] **1.5** `go build ./... && go test ./... && gofmt -l . && go vet ./...` 全绿。
- [x] **1.6** 手工验证：跑一次会触发多轮工具调用的审查，中途 `kill -9`，重新运行确认能从中断点恢复，不重复调用已经问过的工具。
- [x] ⏸ **停下来，等待确认**：阶段一讲解 + 确认理解后再继续。

## 阶段二：结构化输出 + 复审

- [x] **2.1** 新建 `internal/review/review.go`：`Finding`/`Severity`/`Report` 类型定义。
      附带：`internal/gitdiff/hunk.go`（unified diff 解析）+ `internal/review/anchor.go`（anchor 反推行号）——
      模型不再报行号，改为提供逐字代码片段由算法定位，见设计笔记"一"。
- [x] **2.2** 新建 `internal/tool/submitreview.go`：`SubmitReviewTool` 实现 `ITool`，一次性提交完整 findings 列表 + summary
      （空列表表示确认无问题）。结果通过新增的 `tool.Result.ReviewResult` 字段返回，工具保持无状态、可被所有 Agent 共享，
      选型过程见设计笔记"二""五"。
- [x] **2.3** 修改 `internal/prompt/prompt.go`：system prompt 改为要求最终调用 `submit_review` 收尾，不再是"无工具调用的自由文本"。
      附带 `TestSystemPromptRequiresSubmitReview` 固定住几条不能丢的措辞（禁止自由文本作答、空审查也要显式提交、不要自己算行号）。
- [x] **2.4** 修改 `internal/engine`（不是 main.go，阶段一已把循环搬进 engine）：循环终止条件改为
      `result.ReviewResult != nil`；模型若用自由文本作答，回一条提示消息让它补调 `submit_review`，而不是把文本当成结果收下。
      新增 `internal/engine/run_test.go`（fake provider 驱动完整 tool loop）。
      ~~遗留：命中 completed 缓存时拿不到历史结果~~ → 已由 2.6 解决。
- [x] **2.5** 新建 `internal/engine/critique.go`（**不是 review 包**：`tool` 已经导入 `review`，复核循环要用工具
      就只能放在能同时导入两者的 engine 层）：每条 Finding 起一个独立 tool loop 并发复核，errgroup 限流。
      配套 `internal/tool/critiqueverdict.go`（`submit_verdict` 工具，结果走 `Result.CritiqueVerdict`）、
      `config.Critique` 配置段（`concurrency` / `max_turns`，默认 10 / 30）。参数见设计笔记"六"。
      单条复核失败时**保留**该意见并记录原因，不因基础设施抖动而静默吞掉审查意见。
- [x] **2.6** 修改 `internal/store`：`Run` 加 `Findings []review.Finding`、`Summary`、`Critiqued` 字段
      （含 `Kept=false` 的被丢弃项也落盘），数据库加列 + 旧库迁移；命中 completed 缓存时复用 `Findings` 而非最后一条消息。
      附带：初审一提交就落盘（不等复核）；新增"初审已完成、复核未完成"的恢复分支，直接从复核接着跑；
      `engine.Run` 返回值收敛回 `(*store.Run, error)`，结果从 `run.Report()` 取。
- [x] **2.7** 终端输出：`internal/review/render.go` 把 `Report` 渲染成文本列表
      （severity 降序 → 文件 → 行号排序，`[ERROR] path:12-14 (f1)` 形式，只展示通过复核的意见）。
- [x] **2.8** 新建/补充测试：表驱动覆盖参数解析、非法输入。
      - [x] `internal/review/review_test.go`、`internal/review/anchor_test.go`、`internal/gitdiff/hunk_test.go`
      - [x] `internal/tool/submitreview_test.go`（含单实例并发共享的 `-race` 测试）
      - [x] `internal/engine/critique_test.go`（并发上限、失败保留、轮数上限、复核提示词措辞）、
            `internal/tool/critiqueverdict_test.go`、`config_test.go` 的 critique 段
      - [x] `internal/review/render_test.go`、`internal/engine/run_test.go`（终止条件、持久化、缓存复用、从复核恢复）、
            `internal/store/store_test.go` 的 findings 往返与旧库迁移
- [x] **2.9** `go build ./... && go test ./... -race && gofmt -l . && go vet ./...` 全绿。
- [x] **2.10** 端到端验证（用本地 OpenAI 兼容 stub 跑真实二进制，不依赖外部 API）：
      - [x] 模型用自由文本作答时被提示补调 `submit_review`，补调后正常收尾
      - [x] 复核并发执行，过滤掉刻意构造的"过度解读"意见，终端只展示保留项
      - [x] anchor 反推行号正确（多次核对工作区文件真实行号）
      - [x] 同样内容第二次运行零模型调用，输出与首次逐字节一致；数据库里被丢弃的意见带
            `kept=false` 和 `critique_reason` 完整保留
      - [ ] **待 Tan 用真实模型验收**：模型是否习惯性调用 `submit_review`（还是总要被提醒一轮）、
            anchor 命中率（有多少条降级成文件级意见）、复核砍掉的是否都该砍
- [ ] ⏸ **停下来，等待确认**：阶段二讲解 + 确认理解后再继续。

## 阶段三：审查流程可视化（Web）

- [ ] **3.1** 新建 `internal/web/server.go`：`RunSource` 接口、`Server`/`NewServer`/`ListenAndServe`。
- [ ] **3.2** 新建 `internal/web/templates.go`：列表页 + 详情页的 `html/template` 模板（参考 `ai-assistant/web/templates.go` 的结构）。
- [ ] **3.3** 列表页：按 repo 路径、时间倒序展示历史 `Run`，含状态、时间、Findings 数量。
- [ ] **3.4** 详情页：完整消息历史（含每次工具调用的参数/结果）+ 结构化 Findings（复审前后对比）。
- [ ] **3.5** 修改 `cmd/reviewer-ai/main.go`：加 `reviewer web -addr :8090` 子命令分发。
- [ ] **3.6** 新建 `internal/web/server_test.go`：用一个假的 `RunSource` 实现测试 handler 行为（参考 `ai-assistant/web/server_test.go` 的测法）。
- [ ] **3.7** `go build ./... && go test ./... && gofmt -l . && go vet ./...` 全绿。
- [ ] **3.8** 手工验证：`reviewer web` 启动后浏览器打开，确认能看到阶段一二跑出来的历史记录，详情页信息完整可读。
- [ ] ⏸ **停下来，等待确认**：阶段三讲解 + 确认理解后再继续。

## 阶段四：用户交互（reply / review）+ 增量重审

- [ ] **4.1** 修改 `internal/store`：`FindingStatus`、`FindingRecord`（Finding + Status + Discussion）、`Run.ParentRunID`。
- [ ] **4.2** 新建 `internal/review/incremental.go`：`DiffSnapshot(old, new []gitdiff.Change) (unchanged, changed []gitdiff.Change)`，纯逻辑函数。
- [ ] **4.3** 新建 `internal/review/incremental_test.go`：表驱动覆盖文件新增/删除/修改/未变化/重命名等边界情况——这是本阶段最容易出 bug 的核心逻辑，优先把测试写扎实。
- [ ] **4.4** 新建 `internal/tool/withdrawfinding.go`：`WithdrawFindingTool`。
- [ ] **4.5** 新建 `internal/review/discuss.go`：reply 对话的独立 `Generate` 封装（聚焦单条 Finding 的上下文，不复用主审查的全量 prompt）。
- [ ] **4.6** 修改 `internal/web`：加 `POST /run/{id}/finding/{findingID}/reply` handler + 页面上的回复输入框。
- [ ] **4.7** 修改 `internal/web`：加 `POST /run/{id}/rereview` handler（第一版同步阻塞直到完成），触发增量重审流程并创建新 `Run`（`ParentRunID` 关联）。
- [ ] **4.8** `go build ./... && go test ./... && gofmt -l . && go vet ./...` 全绿。
- [ ] **4.9** 手工验证 reply：对一条 Finding 提交有说服力的回复，确认模型正确调用 `withdraw_finding`，Web 上状态变化正确。
- [ ] **4.10** 手工验证 review：修复一个问题、重新 stage，触发 rereview，确认只有改动过的文件被重新送审，未改动文件的 Findings 原样保留。
- [ ] ⏸ **停下来，等待确认**：阶段四讲解 + 确认理解后再继续。

## 阶段五：子 Agent 并发审查

- [ ] **5.1** 新建 `internal/tool/spawnreviewers.go`：`SpawnReviewersTool`，`ReviewGroup` 类型，校验逻辑（文件并集完整不重复、空分组拒绝、并发数上限）。
- [ ] **5.2** 新建 `internal/tool/spawnreviewers_test.go`：表驱动覆盖各种非法分组场景。
- [ ] **5.3** 新建 `internal/review/concurrent.go`：`RunConcurrent(ctx, llm, groups) (Report, error)`，为每个分组独立走一遍阶段二审查流程并发执行、汇总结果。
- [ ] **5.4** 修改 `cmd/reviewer-ai/main.go`：识别 `spawn_reviewers` 调用后走并发分支，不再继续主循环等待 `submit_review`。
- [ ] **5.5** 修改 `internal/prompt/prompt.go`：system prompt 加入 `spawn_reviewers` 使用说明（大改动可拆分，小改动无需拆分）。
- [ ] **5.6** `go build ./... && go test ./... && gofmt -l . && go vet ./...` 全绿。
- [ ] **5.7** 手工验证：构造多模块大改动，观察模型是否主动拆分、Findings 是否完整无重复；构造小改动确认不会画蛇添足拆分；构造非法分组方案确认被正确拒绝。
- [ ] ⏸ **停下来，等待确认**：阶段五讲解 + 全流程回顾，讨论 v3 方向。

---

## 全局回归检查点（每个阶段结束都要过一遍）

- [ ] `go build ./...`
- [ ] `go test ./...`
- [ ] `gofmt -l .`（应无输出）
- [ ] `go vet ./...`
- [ ] 对应阶段的手工端到端验证

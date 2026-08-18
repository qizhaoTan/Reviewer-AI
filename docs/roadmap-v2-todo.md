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

- [x] **3.0**（计划外前置）`internal/store` 新增 `ListAllRuns(ctx, limit)`：跨仓库、跨分支列出全部记录。
      原设计让 Web 用 `ListRuns(ctx, repoPath, limit)`，但它按 `repo_path` **精确**过滤，而存进去的是
      `buildRunKey` 拼出的 `路径#分支` 复合键——列表页作为总入口没有"当前仓库"可传，传空串会得到空列表。
      单开一个方法而不是让空 `repoPath` 表示通配：后者让同一参数背两种语义，而引擎恢复流程正依赖精确语义。
      配套测试固定住"空 repoPath 不是通配符"这条约定。
- [x] **3.1** 新建 `internal/web/server.go`：`RunSource` 接口、`Server`/`NewServer`/`ListenAndServe`。
      附带 `Handler()` 单独暴露路由，让测试不必真的监听端口。
- [x] **3.2** 新建 `internal/web/templates.go`：列表页 + 详情页的 `html/template` 模板，共用一份 `sharedCSS`。
- [x] **3.3** 列表页：全部历史 `Run` 按更新时间倒序，含状态徽标、时间、仓库/分支（复合键拆两列展示）、
      文件数、保留/丢弃意见数。
- [x] **3.4** 详情页：元信息 + 整体评价 + **保留组 / 被复核丢弃组分开展示**（含 anchor 与复核理由）
      + 完整消息历史（含每次工具调用的名称、ID、参数）。
- [x] **3.5** 修改 `cmd/reviewer-ai/main.go`：`main` 拆成 `runReview`/`runWeb`，按 `os.Args[1] == "web"` 分发，
      各自用独立 `flag.NewFlagSet`；不带子命令时行为与之前完全一致。
- [x] **3.6** 新建 `internal/web/server_test.go`：假 `RunSource` 驱动，覆盖列表/详情渲染、
      复核未执行时的计数口径、404/400/500 三条错误路径、模型输出的 HTML 转义、`splitRunKey` 边界。
- [x] **3.7** `go build ./... && go test ./... -race && gofmt -l . && go vet ./...` 全绿。
- [x] **3.8** 手工验证：`reviewer-ai web -addr :8099` 对真实 `~/.reviewer/runs.db` 跑通——
      旧库（缺 findings/summary/critiqued 三列）自动迁移成功，3 条历史记录正常展示，
      详情页完整渲染一次 43 条消息、32 次工具调用的真实运行；404/400/500 状态码均正确。
- [x] **3.9**（Tan 追加）启动 `web` 子命令后自动打开浏览器：按 `runtime.GOOS` 选
      `open`/`xdg-open`/`rundll32`，都是系统自带命令，不引第三方库。在独立 goroutine 里延迟
      300ms 再开（`ListenAndServe` 阻塞，且要等端口就绪，否则浏览器可能抢在监听前拿到连接拒绝）。
      打不开浏览器只警告不退出——URL 已经打在 stderr 上。加 `-no-open` 供无头环境关闭。
- [x] **3.10**（Tan 追加）详情页分级折叠，用原生 `<details>`，零 JS：
      - 「保留的意见」默认**展开**（这是打开页面的主要目的），「被复核丢弃的意见」「消息历史」默认**折叠**
      - 单条意见：级别 + 文件行号 + 一句话摘要**常显**，detail/anchor/复核理由折叠；三者皆空时不渲染折叠框
      - 单条消息再折叠一层：角色徽标、tool_call_id、工具名、正文前 80 字预览常显（换行折成空格、按 rune 截断）
      - 折叠只是视觉折叠，内容仍在 HTML 里——浏览器页内搜索能命中，无需任何请求即可展开（有测试固定）
      - 验证：真实 43 条消息的运行上 1 个区块展开 / 2 个折叠 / 43 条消息逐条折叠；
        对「全部折叠」和「全部展开」两个方向都做了变异测试，确认断言真的会失败
- [ ] ⏸ **停下来，等待确认**：阶段三讲解 + 确认理解后再继续。

## 阶段四：用户交互（reply / review）+ 增量重审

- [x] **4.1** `review.Finding` 加 `Status`（`open`/`withdrawn`）+ `Discussion []schema.Message`；新增 `Report.ActiveFindings()`。
      **偏离原设计**：原计划在 `store` 里新建 `FindingRecord` 包装 `review.Finding`。改为直接扩展 `Finding`——
      它已是全链路通用货币（`KeptFindings`/`Critique`/`Render`/`web` 都直接吃它），换类型要改四处，
      且会让持久化包去定义业务模型的变体，依赖方向是反的。同时砍掉 `acknowledged`：
      reply/rereview 两条流程都没有它的写入点，是个没人写也没人读的字段。
      `Kept`（归复核）与 `Status`（归交互）保持两道独立闸门，合并会让"复核砍的"和"用户说服撤回的"再也分不开。
      副作用：`Finding` 因含切片字段不再可比较，四处测试的 `!=` 改成 `reflect.DeepEqual`。
- [x] **4.2** 新建 `internal/review/incremental.go`：`DiffSnapshot` + `CarryOverFindings`。
      **偏离原设计**：返回三组而非两组，多出 `Vanished`——`Unchanged`/`Changed` 是对 new 的划分，
      而"上次审过、这次不在暂存区"的文件在 new 里没有对应项，塞进哪组都是错的，
      两组签名下它会被静默丢弃、旧意见跟着复用，用户看到针对非暂存文件的意见无从下手。
      判据是 patch 逐字节相等（不比 status）：同一份 patch 意味着模型看到完全相同的输入。
      查证确认 `LoadStaged` 带 `--no-renames`，所以 rename 永远拆成 D+A，不会出现 status R。
- [x] **4.3** 新建 `internal/review/incremental_test.go`：11 个 `DiffSnapshot` 用例（首次审查/全同/单文件改动/
      新增/消失/staged deletion/rename 拆分/全部 unstage/status 变但 patch 未变/双空/四类混合）
      + 顺序与内容保持 + 6 个 `CarryOverFindings` 用例。含一条"Unchanged+Changed 必须构成 new 的划分"的不重不漏断言。
      三轮变异测试（去掉 patch 比较 / 带过撤回意见 / 不检测消失文件）分别有 4、1、3 个用例失败。
- [x] **4.4** 新建 `internal/tool/withdrawfinding.go`：`WithdrawFindingTool` + `Result.Withdrawal`。
      工具**不收** finding id（一次对话只围绕一条意见，让模型填只多一种填错的可能，而填错＝撤销了另一条意见）。
      描述里明确"坚持时不要调用本工具"，并有测试固定这句措辞。
- [x] **4.5** 新建 `internal/engine/discuss.go`：`Reply` 对话循环。
      **与复核循环的关键区别**：复核必须调 `submit_verdict` 收尾，而 reply 的两种结局不对称——
      撤回要调工具，坚持只需正常说话，所以终止条件是"这一轮没有工具调用"。
      把坚持设成零成本，是为了不给模型"顺手把事办完"的捷径。
      system prompt 方向与复核相反（复核压"附和初审"，这里压"附和用户"），
      用户输入按不可信数据包装成"作者的异议"并声明它不是指令。
      两轮变异测试（无工具调用改 continue / 撤回不上报）分别有 3、2 个用例失败。
- [x] **4.6** 修改 `internal/web`：新增 `Reviewer` 接口（`Reply`/`Rereview`），由 cmd 注入 `engine.Interactive`，
      web 仍不 import engine。`POST /reply`、`POST /rereview` 两个端点 + `beginAction` 共用前置校验。
      详情页：撤回标记与删除线、讨论记录（默认展开）、逐条回复框、结果提示条、上一轮记录链接。
      回复框只给"未撤回且通过复核"的意见（Dropped 组传 `Interactive=false` 复用同一份子模板）。
      用 303 重定向而不是原地渲染：POST 后停在原地，刷新就会重复提交（重审是要花钱的）。
- [x] **4.7** 新建 `internal/engine/interactive.go`：`Interactive` 实现两个动作；
      `store` 加 `Run.ParentRunID`（+ 建表列、旧库迁移、`SaveRun`/`scanRun`）与 `SaveFindings`。
      `SaveFindings` 只写 findings 一列而不复用 `SaveRun`：Web 侧那份 Run 是读出来的快照，
      整行 upsert 会用它盖掉命令行正在推进的 messages。
      重审结果写成**新** Run（`ParentRunID` 指回基线）而不是原地更新：旧记录是那次审查的事实存档。
      快照存**完整**当前暂存区（只存改动子集会让未变化文件下次被误判成新增）；
      合并后 `renumber` 重排 ID（两个来源各自从 f1 编号，不重排必然撞车，而 reply 靠 ID 定位意见）。
      三轮变异测试（不重排 ID / 只存部分快照 / 带过全部旧意见）均被对应用例捕获。
- [x] **4.8** `go build ./... && go test ./... -race && gofmt -l . && go vet ./...` 全绿。
- [x] **4.9/4.10** 端到端验证（对真实 `~/.reviewer/runs.db`，动库前已备份）：
      旧库缺 `parent_run_id` 列自动迁移成功、真实数据一字未改；
      4 条意见 / 2 条 kept 的记录正好渲染 2 个回复框 + 1 个重审按钮；
      `GET /reply` → 405、缺 run/finding → 400、空回复 → 303 带错误提示；
      重审在空暂存区下正确报 "nothing is staged in /Users/tan/Reviewer-AI"；
      提示条正常渲染且 `<script>` 被转义。
      - [ ] **待 Tan 用真实模型验收**：有说服力的 reply 能否让模型调用 `withdraw_finding`、
            无理取闹的 reply 会不会让它照样让步（这是 sycophancy 措辞是否起效的真正考验）；
            改一个文件后重审，未改动文件的意见是否原样保留、改动文件是否真的重审
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

// Package web 提供历史审查记录的查看与管理服务。
//
// 定位：个人调试用，本机监听、无鉴权——用来回答"这次审查模型到底看了什么、
// 复核砍掉了哪些意见、为什么砍"，而不是给团队用的正式看板。
//
// 解耦：web 不依赖 store.Store 这个具体类型，只认本包定义的 RunSource 接口，
// 由 cmd 入口注入实现。这样 web 包能用假数据独立测试，展现层的依赖也不会
// 反向渗进持久化层。
package web

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qizhaoTan/Reviewer-AI/internal/review"
	"github.com/qizhaoTan/Reviewer-AI/internal/store"
)

// RunSource 提供 Web 层需要的记录访问能力，由 *store.Store 实现。
//
// 只声明这三个方法而不是直接收 *store.Store：列表页要的是"全部记录"，
// 详情页要的是"按 ID 取一条"，加上一个删除。Store 上其余的写入方法和
// 恢复用查询与展现层无关，不该暴露给它。
type RunSource interface {
	// ListAllRuns 跨仓库、跨分支列出历史运行，按更新时间倒序，最多 limit 条。
	ListAllRuns(ctx context.Context, limit int) ([]store.Run, error)
	// LoadRun 按 ID 加载一条记录；不存在时返回 (nil, nil)。
	LoadRun(ctx context.Context, id string) (*store.Run, error)
	// DeleteRun 删除一条记录，返回该记录此前是否存在。
	//
	// 删除放在 RunSource 而不是 Reviewer 里：它不需要调模型，纯粹是对本地
	// 数据的操作。放进 Reviewer 会让"模型没配好"连删记录都做不了，而清理
	// 一条跑坏的记录恰恰是这种时候最想做的事。
	DeleteRun(ctx context.Context, id string) (bool, error)
}

// Reviewer 是需要调用大模型的那部分交互能力，由 cmd 入口注入 engine 的实现。
//
// 单开一个接口而不是让 web 直接 import engine：web 的职责是"把请求翻译成一次
// 动作、把结果渲染成 HTML"，至于这次动作要跑几轮模型、用哪些工具、怎么落盘，
// 是 engine 的事。接口只有两个方法，web 也就只能做这两件事。
//
// 为 nil 时（比如以后加一个纯只读模式）两个交互端点返回 503，页面照常可读——
// 查看历史记录不该因为模型没配好就整个用不了。
type Reviewer interface {
	// Reply 就用户对某条意见的异议跑一次对话，落盘后返回更新过的记录。
	// findingID 不存在时返回错误。
	Reply(ctx context.Context, runID, findingID, userReply string) (*store.Run, error)

	// Rereview 基于 runID 这次审查的快照做一次增量重审，返回新建的运行记录。
	// 未变化文件的意见原样保留，只有真正改动过的文件会重新送审。
	Rereview(ctx context.Context, runID string) (*store.Run, error)
}

// defaultListLimit 是列表页展示的最大记录数。够翻查最近的调试记录，
// 又不至于让一个跑了几百次的库把整页撑爆。
const defaultListLimit = 200

// Server 是历史记录查看服务。
type Server struct {
	source   RunSource
	reviewer Reviewer
	addr     string
}

// NewServer 装配 Web 服务。addr 形如 ":8090"；reviewer 可以为 nil，
// 此时只提供只读浏览。
func NewServer(source RunSource, reviewer Reviewer, addr string) *Server {
	return &Server{source: source, reviewer: reviewer, addr: addr}
}

// Handler 返回装配好路由的 http.Handler。单独暴露出来是为了测试能直接
// 打这个 handler，不必真的监听端口。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/run", s.handleRun)
	mux.HandleFunc("/reply", s.handleReply)
	mux.HandleFunc("/rereview", s.handleRereview)
	mux.HandleFunc("/delete", s.handleDelete)
	return mux
}

// ListenAndServe 阻塞运行 HTTP 服务，返回的 error 与 http.Server.ListenAndServe 一致。
func (s *Server) ListenAndServe() error {
	return http.ListenAndServe(s.addr, s.Handler())
}

// indexRow 是列表页每行的数据。
type indexRow struct {
	ID        string
	RepoPath  string // 从 runKey 拆出的仓库路径
	Branch    string // 从 runKey 拆出的分支名
	Status    store.RunStatus
	UpdatedAt time.Time
	Files     int // 本次审查覆盖的文件数
	Kept      int // 通过复核、会展示给用户的意见数
	Dropped   int // 被复核砍掉的意见数
	Critiqued bool
}

// handleIndex 渲染列表页：全部历史运行，按更新时间倒序。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// 只服务根路径，其余未注册路径落到这里应当是 404，而不是误显列表。
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	runs, err := s.source.ListAllRuns(r.Context(), defaultListLimit)
	if err != nil {
		http.Error(w, "加载运行记录失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	rows := make([]indexRow, len(runs))
	for i, run := range runs {
		repoPath, branch := splitRunKey(run.RepoPath)
		kept, dropped := countFindings(run)
		rows[i] = indexRow{
			ID:        run.ID,
			RepoPath:  repoPath,
			Branch:    branch,
			Status:    run.Status,
			UpdatedAt: run.UpdatedAt,
			Files:     len(run.Snapshot),
			Kept:      kept,
			Dropped:   dropped,
			Critiqued: run.Critiqued,
		}
	}

	page := indexPage{
		Rows:   rows,
		Notice: r.URL.Query().Get("notice"),
		Error:  r.URL.Query().Get("error"),
	}
	if err := indexTmpl.Execute(w, page); err != nil {
		http.Error(w, "渲染列表页失败: "+err.Error(), http.StatusInternalServerError)
	}
}

// indexPage 是列表页的数据。
//
// 比直接传 []indexRow 多包一层是为了带上操作结果提示：删除完成后要重定向回
// 列表页，用户得知道刚才那一下到底删掉了没有。
type indexPage struct {
	Rows   []indexRow
	Notice string
	Error  string
}

// countFindings 统计一次运行里"保留"与"丢弃"的意见数。
//
// 复核未跑完（Critiqued=false）时所有意见的 Kept 都还是零值，此时把它们
// 全算作保留、丢弃数为 0——那是"还没判"而不是"判为丢弃"，混为一谈会让
// 列表页显示成"全被砍了"，与事实相反。
func countFindings(run store.Run) (kept, dropped int) {
	if !run.Critiqued {
		return len(run.Findings), 0
	}
	for _, f := range run.Findings {
		if f.Kept {
			kept++
			continue
		}
		dropped++
	}
	return kept, dropped
}

// splitRunKey 把引擎侧的 runKey（"仓库绝对路径#分支名"）拆回两段用于展示。
// 找不到分隔符时整串当作仓库路径、分支留空——阶段一之前的旧记录存的就是
// 裸路径，不该因为格式变了就显示不出来。
func splitRunKey(key string) (repoPath, branch string) {
	idx := strings.LastIndex(key, "#")
	if idx < 0 {
		return key, ""
	}
	return key[:idx], key[idx+1:]
}

// runPage 是详情页的数据。
type runPage struct {
	Run store.Run
	// RepoPath / Branch 是 Run.RepoPath 拆开后的展示形式。
	RepoPath string
	Branch   string
	// Kept / Dropped 把 Findings 按复核结论分成两组，让"复核到底过滤掉了
	// 什么"在页面上一眼可见——这正是这个 Web 服务存在的主要理由。
	//
	// 被用户说服撤回的意见仍然留在 Kept 里（它确实通过了复核），只是在模板里
	// 打上撤回标记并置灰。不把它们挪进 Dropped：那一组的含义是"复核砍掉的"，
	// 混进来会让"复核到底砍了多少"这个数字失真。
	Kept    []review.Finding
	Dropped []review.Finding

	// Interactive 表示这次部署是否接了模型，决定页面上要不要渲染回复框和
	// 重审按钮。渲染一个点了必然报错的按钮，比不渲染更糟。
	Interactive bool

	// Notice / Error 是上一次操作的结果提示，通过重定向后的查询参数带回来。
	Notice string
	Error  string
}

// handleRun 渲染单次运行的详情页。
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "缺少运行记录 id", http.StatusBadRequest)
		return
	}

	run, err := s.source.LoadRun(r.Context(), id)
	if err != nil {
		http.Error(w, "加载运行记录失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if run == nil {
		http.Error(w, "运行记录不存在: "+id, http.StatusNotFound)
		return
	}

	repoPath, branch := splitRunKey(run.RepoPath)
	page := runPage{
		Run: *run, RepoPath: repoPath, Branch: branch,
		Interactive: s.reviewer != nil,
		Notice:      r.URL.Query().Get("notice"),
		Error:       r.URL.Query().Get("error"),
	}
	for _, f := range run.Findings {
		// 与 countFindings 同一套口径：复核没跑完就全部按"保留"展示。
		if !run.Critiqued || f.Kept {
			page.Kept = append(page.Kept, f)
			continue
		}
		page.Dropped = append(page.Dropped, f)
	}

	if err := runTmpl.Execute(w, page); err != nil {
		http.Error(w, "渲染详情页失败: "+err.Error(), http.StatusInternalServerError)
	}
}

// handleReply 处理"对某条意见提出异议"的表单提交。
//
// 同步阻塞直到模型给出结论——一次 reply 是一轮小对话，通常几秒到几十秒，
// 为它引入任务队列和轮询是不划算的复杂度。慢的话浏览器转圈，能接受。
func (s *Server) handleReply(w http.ResponseWriter, r *http.Request) {
	runID, findingID, ok := s.beginAction(w, r, "finding")
	if !ok {
		return
	}

	reply := strings.TrimSpace(r.FormValue("reply"))
	if reply == "" {
		redirectToRun(w, r, runID, "", "回复内容为空，没有可讨论的内容")
		return
	}

	if _, err := s.reviewer.Reply(r.Context(), runID, findingID, reply); err != nil {
		// 失败原样回到详情页并把原因显示出来，而不是甩一个 500 白屏——
		// 用户刚写完一段话，至少得知道它为什么没被接受。
		redirectToRun(w, r, runID, "", "回复失败: "+err.Error())
		return
	}
	redirectToRun(w, r, runID, "已提交回复，模型的结论见该条意见下方的讨论记录", "")
}

// handleRereview 处理"我已修复，重新审查"的表单提交。
//
// 同样同步阻塞。这个可能要跑几分钟（改动过的文件要重走完整的初审+复核），
// 页面会一直转圈——第一版接受这个代价，换掉它需要的是一整套异步任务状态，
// 而那笔投入应该等到确实觉得慢了再花。
func (s *Server) handleRereview(w http.ResponseWriter, r *http.Request) {
	runID, _, ok := s.beginAction(w, r, "")
	if !ok {
		return
	}

	newRun, err := s.reviewer.Rereview(r.Context(), runID)
	if err != nil {
		redirectToRun(w, r, runID, "", "重新审查失败: "+err.Error())
		return
	}
	// 跳到新建的那条记录：用户要看的是重审之后的结果，不是他刚才离开的旧记录。
	redirectToRun(w, r, newRun.ID, "增量重审完成，未改动文件的意见已原样保留", "")
}

// handleDelete 处理"删除这条审查记录"的表单提交。
//
// 不走 beginAction：删除不需要模型，reviewer 为 nil 时也必须能用。
// 删完回列表页而不是详情页——那条记录已经没了，详情页只会 404。
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "该端点只接受 POST", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "解析表单失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	runID := r.FormValue("run")
	if runID == "" {
		http.Error(w, "缺少运行记录 id", http.StatusBadRequest)
		return
	}

	deleted, err := s.source.DeleteRun(r.Context(), runID)
	if err != nil {
		redirectToIndex(w, r, "", "删除失败: "+err.Error())
		return
	}
	if !deleted {
		// 不当作错误：多半是用户在两个标签页里各点了一次删除。记录已经
		// 不在了，用户想要的结果已经达成，只是提示措辞要说实话。
		redirectToIndex(w, r, "", "记录不存在，可能已被删除: "+runID)
		return
	}
	redirectToIndex(w, r, "已删除记录 "+runID, "")
}

// beginAction 是两个 POST 端点共用的前置检查：方法、reviewer 是否就绪、
// 必填参数。ok 为 false 时响应已经写好，调用方直接返回即可。
//
// findingParam 为空表示这个端点不需要 finding id。
func (s *Server) beginAction(w http.ResponseWriter, r *http.Request, findingParam string) (runID, findingID string, ok bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "该端点只接受 POST", http.StatusMethodNotAllowed)
		return "", "", false
	}
	if s.reviewer == nil {
		http.Error(w, "本次启动未接入模型，交互功能不可用", http.StatusServiceUnavailable)
		return "", "", false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "解析表单失败: "+err.Error(), http.StatusBadRequest)
		return "", "", false
	}

	runID = r.FormValue("run")
	if runID == "" {
		http.Error(w, "缺少运行记录 id", http.StatusBadRequest)
		return "", "", false
	}
	if findingParam != "" {
		findingID = r.FormValue(findingParam)
		if findingID == "" {
			http.Error(w, "缺少意见 id", http.StatusBadRequest)
			return "", "", false
		}
	}
	return runID, findingID, true
}

// redirectToRun 把操作结果作为查询参数带回详情页。
//
// 用 303 重定向而不是直接渲染：POST 之后停在原地，用户一刷新就会重复提交一次
// 回复（或者再触发一次重审），而重审是要花钱的。重定向后地址栏是 GET，刷新无害。
func redirectToRun(w http.ResponseWriter, r *http.Request, runID, notice, errMsg string) {
	target := url.URL{Path: "/run"}
	q := url.Values{"id": {runID}}
	if notice != "" {
		q.Set("notice", notice)
	}
	if errMsg != "" {
		q.Set("error", errMsg)
	}
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

// redirectToIndex 与 redirectToRun 同理，只是落点是列表页——删除之后
// 详情页已经不存在了，只能回列表。
func redirectToIndex(w http.ResponseWriter, r *http.Request, notice, errMsg string) {
	target := url.URL{Path: "/"}
	q := url.Values{}
	if notice != "" {
		q.Set("notice", notice)
	}
	if errMsg != "" {
		q.Set("error", errMsg)
	}
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

// 模板：服务端渲染，零前端依赖。html/template 自动转义，杜绝注入。
var (
	indexTmpl = template.Must(template.New("index").Funcs(tmplFuncs).Parse(indexHTML))
	runTmpl   = template.Must(template.New("run").Funcs(tmplFuncs).Parse(runHTML))
)

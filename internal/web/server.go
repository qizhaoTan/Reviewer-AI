// Package web 提供一个只读的历史审查记录查看服务。
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
	"strings"
	"time"

	"github.com/qizhaoTan/Reviewer-AI/internal/review"
	"github.com/qizhaoTan/Reviewer-AI/internal/store"
)

// RunSource 提供历史运行记录的只读访问，由 *store.Store 实现。
//
// 只声明这两个方法而不是直接收 *store.Store：列表页要的是"全部记录"，
// 详情页要的是"按 ID 取一条"，Store 上其余的写入方法和恢复用查询都不该
// 暴露给一个只读的展现层。
type RunSource interface {
	// ListAllRuns 跨仓库、跨分支列出历史运行，按更新时间倒序，最多 limit 条。
	ListAllRuns(ctx context.Context, limit int) ([]store.Run, error)
	// LoadRun 按 ID 加载一条记录；不存在时返回 (nil, nil)。
	LoadRun(ctx context.Context, id string) (*store.Run, error)
}

// defaultListLimit 是列表页展示的最大记录数。够翻查最近的调试记录，
// 又不至于让一个跑了几百次的库把整页撑爆。
const defaultListLimit = 200

// Server 是历史记录查看服务。
type Server struct {
	source RunSource
	addr   string
}

// NewServer 装配 Web 服务。addr 形如 ":8090"。
func NewServer(source RunSource, addr string) *Server {
	return &Server{source: source, addr: addr}
}

// Handler 返回装配好路由的 http.Handler。单独暴露出来是为了测试能直接
// 打这个 handler，不必真的监听端口。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/run", s.handleRun)
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

	if err := indexTmpl.Execute(w, rows); err != nil {
		http.Error(w, "渲染列表页失败: "+err.Error(), http.StatusInternalServerError)
	}
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
	Kept    []review.Finding
	Dropped []review.Finding
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
	page := runPage{Run: *run, RepoPath: repoPath, Branch: branch}
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

// 模板：服务端渲染，零前端依赖。html/template 自动转义，杜绝注入。
var (
	indexTmpl = template.Must(template.New("index").Funcs(tmplFuncs).Parse(indexHTML))
	runTmpl   = template.Must(template.New("run").Funcs(tmplFuncs).Parse(runHTML))
)

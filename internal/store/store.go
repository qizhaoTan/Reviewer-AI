// Package store 提供审查运行（Run）的 SQLite 持久化。
//
// 设计要点：
//   - 工具输出（RoleUser + ToolCallID != ""）不存原始内容，只保留占位消息，
//     因为工具输出体积大，且恢复后模型可以按需重新调用工具获得同样的内容。
//   - Snapshot 和 Messages 都以 JSON 整体存入 TEXT 列：这两个字段只会整体读写，
//     不需要按内部字段做 SQL 查询，拆表反而增加复杂度。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/review"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

// RunStatus 记录一次审查运行所处的阶段。
type RunStatus string

const (
	StatusPending    RunStatus = "pending"
	StatusInProgress RunStatus = "in_progress"
	StatusCompleted  RunStatus = "completed"
	StatusFailed     RunStatus = "failed"
)

// Run 是一次审查运行的完整持久化记录。
type Run struct {
	ID        string
	RepoPath  string
	Status    RunStatus
	CreatedAt time.Time
	UpdatedAt time.Time
	Snapshot  []gitdiff.Change // 本次审查时的暂存区快照，供后续增量重审比较基线
	Messages  []schema.Message // 发给模型的完整消息历史（含工具调用往返）

	// Findings 是模型提交的结构化审查意见，含复核判定为丢弃的那些
	// （Kept=false）——保留它们是为了能回答"复核到底砍掉了什么、为什么"。
	// 初审一提交就落盘，不等复核完成：复核是一批重活，跑到一半崩了却没存
	// 初审结果的话，恢复时得让模型重审一遍。
	Findings []review.Finding

	// Summary 是模型对本次改动的整体评价。
	Summary string

	// Critiqued 标记复核阶段是否已经跑完，用于区分"复核判定丢弃"和"复核还没跑、
	// Kept 只是零值"这两种含义完全不同的 Kept=false。恢复时也靠它判断该从
	// 主循环还是复核阶段接着跑。
	Critiqued bool

	// ParentRunID 指向本次增量重审所基于的上一条运行记录，把一次次重审串成
	// 一条审查历史链。首次审查（没有基线）时为空。
	//
	// 存 ID 而不是把上一条整个嵌进来：链可以很长，嵌套存储会让每条记录都
	// 带上它全部祖先的副本，一次重审就把数据库撑大一倍。
	ParentRunID string
}

// Report 把 Run 上的三个字段还原成一份 review.Report。
// 命中历史记录时调用方拿到的就是这个，与刚跑完一次审查得到的结构完全一致。
func (r Run) Report() review.Report {
	return review.Report{
		Findings:  r.Findings,
		Summary:   r.Summary,
		Critiqued: r.Critiqued,
	}
}

// SetReport 把一份 review.Report 摊平到 Run 的对应字段上，是 Report 的逆操作。
func (r *Run) SetReport(report review.Report) {
	r.Findings = report.Findings
	r.Summary = report.Summary
	r.Critiqued = report.Critiqued
}

// NewRunID 生成一个新的 Run ID。抽成函数是为了调用方（main.go）和测试都能拿到
// 与 store 内部一致的 ID 格式，不需要各自约定。
func NewRunID() string {
	return uuid.NewString()
}

// Store 是 Run 持久化的入口，内部持有一个 SQLite 连接。
type Store struct {
	db *sql.DB
}

// DefaultDSN 返回默认的 SQLite DSN：~/.reviewer/runs.db，与 config.json 同目录，
// 支持 $REVIEWER_AI_CONFIG 所在目录被覆盖时保持一致（复用 config.DefaultPath 的目录约定）。
func DefaultDSN() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	dbPath := filepath.Join(home, ".reviewer", "runs.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return "", fmt.Errorf("create %q: %w", filepath.Dir(dbPath), err)
	}
	return "file:" + dbPath + "?_pragma=journal_mode(WAL)", nil
}

// New 打开（或创建）SQLite 数据库并建表。dsn 为空时使用 DefaultDSN()。
func New(dsn string) (*Store, error) {
	if dsn == "" {
		resolved, err := DefaultDSN()
		if err != nil {
			return nil, fmt.Errorf("resolve default dsn: %w", err)
		}
		dsn = resolved
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database %q: %w", dsn, err)
	}
	// SQLite 单文件不支持多写连接并发写入；限制为单连接避免 "database is locked"。
	db.SetMaxOpenConns(1)

	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭底层数据库连接。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS runs (
			id            TEXT PRIMARY KEY,
			repo_path     TEXT    NOT NULL,
			status        TEXT    NOT NULL,
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL,
			snapshot      TEXT    NOT NULL,
			snapshot_hash TEXT    NOT NULL DEFAULT '',
			messages      TEXT    NOT NULL,
			findings      TEXT    NOT NULL DEFAULT '[]',
			summary       TEXT    NOT NULL DEFAULT '',
			critiqued     INTEGER NOT NULL DEFAULT 0,
			parent_run_id TEXT    NOT NULL DEFAULT ''
		);
	`); err != nil {
		return err
	}

	// 兼容在这些列引入之前创建的旧库：CREATE TABLE IF NOT EXISTS 对已存在的表
	// 不会补列，所以逐个显式检查并 ALTER TABLE 补上。默认值让旧记录读出来是
	// "没有意见、没有复核过"，语义上正确——那些运行确实是在结构化输出之前跑的。
	for _, col := range []struct{ name, ddl string }{
		{"snapshot_hash", `ALTER TABLE runs ADD COLUMN snapshot_hash TEXT NOT NULL DEFAULT ''`},
		{"findings", `ALTER TABLE runs ADD COLUMN findings TEXT NOT NULL DEFAULT '[]'`},
		{"summary", `ALTER TABLE runs ADD COLUMN summary TEXT NOT NULL DEFAULT ''`},
		{"critiqued", `ALTER TABLE runs ADD COLUMN critiqued INTEGER NOT NULL DEFAULT 0`},
		{"parent_run_id", `ALTER TABLE runs ADD COLUMN parent_run_id TEXT NOT NULL DEFAULT ''`},
	} {
		exists, err := columnExists(db, "runs", col.name)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := db.Exec(col.ddl); err != nil {
				return err
			}
		}
	}

	_, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_runs_repo_updated ON runs(repo_path, updated_at);
		CREATE INDEX IF NOT EXISTS idx_runs_repo_snapshot_hash ON runs(repo_path, snapshot_hash);
	`)
	return err
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// SaveRun 幂等地 upsert 一条运行记录（按 ID）。调用方在每轮 Generate 往返后
// 调用它，保证进程中断后能从最后一次成功持久化的状态恢复。snapshot_hash 由
// SaveRun 内部从 run.Snapshot 派生计算，不接受调用方传入——避免出现
// "改了 Snapshot 却忘记同步 hash" 这种不一致。
func (s *Store) SaveRun(ctx context.Context, run Run) error {
	snapshotJSON, err := json.Marshal(run.Snapshot)
	if err != nil {
		return fmt.Errorf("marshal snapshot for run %s: %w", run.ID, err)
	}
	messagesJSON, err := json.Marshal(compactToolResults(run.Messages))
	if err != nil {
		return fmt.Errorf("marshal messages for run %s: %w", run.ID, err)
	}
	// findings 为 nil 时存 "[]" 而不是 "null"，让列里始终是一个合法的 JSON 数组，
	// 读出来也就始终是空切片而非 nil。
	findings := run.Findings
	if findings == nil {
		findings = []review.Finding{}
	}
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		return fmt.Errorf("marshal findings for run %s: %w", run.ID, err)
	}
	snapshotHash := gitdiff.SnapshotHash(run.Snapshot)

	now := time.Now()
	createdAt := run.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO runs (id, repo_path, status, created_at, updated_at, snapshot, snapshot_hash, messages, findings, summary, critiqued, parent_run_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status        = excluded.status,
			updated_at    = excluded.updated_at,
			snapshot      = excluded.snapshot,
			snapshot_hash = excluded.snapshot_hash,
			messages      = excluded.messages,
			findings      = excluded.findings,
			summary       = excluded.summary,
			critiqued     = excluded.critiqued,
			parent_run_id = excluded.parent_run_id
	`, run.ID, run.RepoPath, string(run.Status), createdAt.UnixNano(), now.UnixNano(),
		string(snapshotJSON), snapshotHash, string(messagesJSON),
		string(findingsJSON), run.Summary, run.Critiqued, run.ParentRunID)
	if err != nil {
		return fmt.Errorf("save run %s: %w", run.ID, err)
	}
	return nil
}

// SaveFindings 只更新一条运行记录的 findings 列，不碰 messages / status /
// snapshot。
//
// 为什么不复用 SaveRun：reply 只改动一条意见的 Status 和 Discussion，而 SaveRun
// 会把调用方手里那份 Run 的**每个**字段都写回去。Web 侧的 Run 是先 LoadRun 读出来
// 的一份快照，如果此时命令行正在同一条记录上跑审查并推进 messages，SaveRun 就会
// 拿这份读旧了的快照把新进展覆盖掉。只写一列把这个窗口缩到最小。
//
// 这不是完整的并发控制（两个 reply 同时改不同意见仍会互相覆盖），但对"本机单人
// 使用"这个定位是够的：真正会撞车的是 Web 与 CLI 并行，而那正是这里挡住的。
func (s *Store) SaveFindings(ctx context.Context, id string, findings []review.Finding) error {
	if findings == nil {
		findings = []review.Finding{}
	}
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		return fmt.Errorf("marshal findings for run %s: %w", id, err)
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET findings = ?, updated_at = ? WHERE id = ?
	`, string(findingsJSON), time.Now().UnixNano(), id)
	if err != nil {
		return fmt.Errorf("save findings for run %s: %w", id, err)
	}
	// 影响 0 行意味着这个 ID 根本不存在。静默成功会让调用方以为撤回已经
	// 持久化，而实际上什么都没发生。
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("save findings for run %s: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("save findings: run %s does not exist", id)
	}
	return nil
}

// LoadRun 按 ID 加载一条运行记录；不存在时返回 (nil, nil)。
func (s *Store) LoadRun(ctx context.Context, id string) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+runColumns+`
		FROM runs WHERE id = ?
	`, id)
	run, err := scanRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("load run %s: %w", id, err)
	}
	return run, nil
}

// LoadLatestRun 按 repoPath 查找最近更新的一条记录；不存在时返回 (nil, nil)。
func (s *Store) LoadLatestRun(ctx context.Context, repoPath string) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+runColumns+`
		FROM runs WHERE repo_path = ?
		ORDER BY updated_at DESC
		LIMIT 1
	`, repoPath)
	run, err := scanRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("load latest run for %s: %w", repoPath, err)
	}
	return run, nil
}

// LoadRunByHash 在 repoPath 下按快照内容哈希查找运行记录，不限制状态、
// 不要求是最近一条——只要该 repoPath 下曾经有一次运行的暂存区内容与当前
// snapshotHash 完全一致，就会命中，哪怕这次运行早已 completed 或被更新的
// 运行覆盖了"最新"的位置。用于内容级复用：例如 git stash 后暂存区内容
// 与之前审查过的某次完全相同，重新审查时可以直接复用那次的结果。
// 同一 hash 有多条命中时返回最近更新的一条；不存在时返回 (nil, nil)。
func (s *Store) LoadRunByHash(ctx context.Context, repoPath, snapshotHash string) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+runColumns+`
		FROM runs WHERE repo_path = ? AND snapshot_hash = ?
		ORDER BY updated_at DESC
		LIMIT 1
	`, repoPath, snapshotHash)
	run, err := scanRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("load run by hash for %s: %w", repoPath, err)
	}
	return run, nil
}

// ListRuns 按 repoPath 精确列出历史运行，按更新时间倒序，最多 limit 条
// （limit <= 0 时不限制）。repoPath 必须是完整的 runKey（引擎侧是
// "仓库绝对路径#分支名"），传空字符串匹配不到任何记录——想列出全部记录
// 请用 ListAllRuns，不要指望空字符串被当成通配。
func (s *Store) ListRuns(ctx context.Context, repoPath string, limit int) ([]Run, error) {
	runs, err := s.queryRuns(ctx, `WHERE repo_path = ?`, limit, repoPath)
	if err != nil {
		return nil, fmt.Errorf("list runs for %s: %w", repoPath, err)
	}
	return runs, nil
}

// ListAllRuns 跨仓库、跨分支列出全部历史运行，按更新时间倒序，最多 limit 条
// （limit <= 0 时不限制）。供 Web 列表页作总入口使用：那里没有"当前仓库"
// 的概念，用户要看的就是这个库里存过的所有运行。
//
// 单独开一个方法而不是让 ListRuns 的空 repoPath 表示"不过滤"：后者让同一个
// 参数背两种语义，调用方少写一个值就从"精确查询"静默变成"全表扫描"，而
// 引擎恢复流程恰恰依赖 ListRuns 的精确语义。
func (s *Store) ListAllRuns(ctx context.Context, limit int) ([]Run, error) {
	runs, err := s.queryRuns(ctx, "", limit)
	if err != nil {
		return nil, fmt.Errorf("list all runs: %w", err)
	}
	return runs, nil
}

// queryRuns 是列表类查询的共用实现：拼 where 子句与可选的 LIMIT，逐行 scan。
// where 为空表示不过滤；args 必须与 where 里的占位符一一对应。
func (s *Store) queryRuns(ctx context.Context, where string, limit int, args ...any) ([]Run, error) {
	query := `SELECT ` + runColumns + ` FROM runs `
	if where != "" {
		query += where + ` `
	}
	query += `ORDER BY updated_at DESC `
	if limit > 0 {
		query += `LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run row: %w", err)
		}
		runs = append(runs, *run)
	}
	return runs, rows.Err()
}

// runColumns 是所有查询共用的列清单，顺序必须与 scanRun 的 Scan 参数一一对应。
// 抽成常量是因为之前四处 SELECT 各写一遍，加一列就要改四处，漏改一处就是
// "列数对不上"的运行期错误。
const runColumns = `id, repo_path, status, created_at, updated_at, snapshot, messages, findings, summary, critiqued, parent_run_id`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRun(row rowScanner) (*Run, error) {
	var run Run
	var status string
	var createdAt, updatedAt int64
	var snapshotJSON, messagesJSON, findingsJSON string
	if err := row.Scan(&run.ID, &run.RepoPath, &status, &createdAt, &updatedAt,
		&snapshotJSON, &messagesJSON, &findingsJSON, &run.Summary, &run.Critiqued, &run.ParentRunID); err != nil {
		return nil, err
	}
	run.Status = RunStatus(status)
	run.CreatedAt = time.Unix(0, createdAt)
	run.UpdatedAt = time.Unix(0, updatedAt)
	if err := json.Unmarshal([]byte(snapshotJSON), &run.Snapshot); err != nil {
		return nil, fmt.Errorf("unmarshal snapshot: %w", err)
	}
	if err := json.Unmarshal([]byte(messagesJSON), &run.Messages); err != nil {
		return nil, fmt.Errorf("unmarshal messages: %w", err)
	}
	if err := json.Unmarshal([]byte(findingsJSON), &run.Findings); err != nil {
		return nil, fmt.Errorf("unmarshal findings: %w", err)
	}
	return &run, nil
}

// toolResultPlaceholder 是工具结果消息落盘时的占位内容。
const toolResultPlaceholder = "内容已过期，如需要请重新调用工具"

// compactToolResults 返回 messages 的副本，把工具结果消息（RoleUser 且
// ToolCallID 非空）的 Content 替换成占位提示，避免大文件读取结果把数据库撑大。
// 恢复运行后，如果模型还需要这些内容，会重新调用工具获取。
//
// 暂时不启用压缩（直接原样返回）：调试恢复逻辑时需要在数据库里看到工具调用的
// 真实输出内容，占位符会掩盖问题。压缩逻辑本身保留，待调试阶段结束后重新启用。
const compactionEnabled = false

// ToolResultsCompacted 报告工具结果落盘时是否会被替换成占位符。
//
// 导出给展现层用：Web 详情页要在消息历史标题上说明"你看到的工具结果是不是
// 真实内容"。这个说明必须跟 compactionEnabled 联动而不是写死在模板里——
// 写死的那版曾经在开关关掉之后还宣称"已压缩为占位符"，调试时会让人以为
// 上下文被截断了，而实际上并没有。
func ToolResultsCompacted() bool { return compactionEnabled }

func compactToolResults(messages []schema.Message) []schema.Message {
	if !compactionEnabled {
		return messages
	}
	return replaceToolResultContents(messages)
}

// replaceToolResultContents 是压缩逻辑本身，与 compactionEnabled 开关无关，
// 便于独立测试压缩行为是否正确（见 store_test.go 的 TestCompactToolResults）。
func replaceToolResultContents(messages []schema.Message) []schema.Message {
	out := make([]schema.Message, len(messages))
	for i, m := range messages {
		out[i] = m
		if m.Role == schema.RoleUser && m.ToolCallID != "" {
			out[i].Content = toolResultPlaceholder
		}
	}
	return out
}

// DeleteRun 删除一条运行记录，并把所有以它为父的记录的 parent_run_id 置空。
// 记录不存在时返回 (false, nil)——重复点删除按钮不该报错。
//
// 为什么要顺带清理子记录：ParentRunID 是一条重审链上的反向指针，父记录一删，
// 子记录详情页上的"上一轮"链接就指向了一个不存在的 ID，点进去 404。置空
// 在语义上也是对的——那一轮确实不存在了。
//
// 不做级联删除：一条链上的后续重审是独立的审查结果，用户删掉某一轮通常是
// 想重审那一轮，而不是想把之后的成果一起丢掉。
//
// 两条语句放在一个事务里：只删了记录却没清子指针的话，那些死链就永远留在库里，
// 而且没有任何后续操作会去修它。
func (s *Store) DeleteRun(ctx context.Context, id string) (deleted bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("delete run %s: begin transaction: %w", id, err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	res, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete run %s: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete run %s: %w", id, err)
	}
	// 记录本来就不存在时直接提交空事务返回：此时不该去改任何子记录，
	// 因为"父记录不存在"这件事在删除之前就已经成立了。
	if affected == 0 {
		if err = tx.Commit(); err != nil {
			return false, fmt.Errorf("delete run %s: commit: %w", id, err)
		}
		return false, nil
	}

	if _, err = tx.ExecContext(ctx, `
		UPDATE runs SET parent_run_id = '', updated_at = ? WHERE parent_run_id = ?
	`, time.Now().UnixNano(), id); err != nil {
		return false, fmt.Errorf("delete run %s: clear child parent_run_id: %w", id, err)
	}

	if err = tx.Commit(); err != nil {
		return false, fmt.Errorf("delete run %s: commit: %w", id, err)
	}
	return true, nil
}

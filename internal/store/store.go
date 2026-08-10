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
			messages      TEXT    NOT NULL
		);
	`); err != nil {
		return err
	}

	// 兼容在 snapshot_hash 列引入之前创建的旧库：CREATE TABLE IF NOT EXISTS 对已存在的表
	// 不会补列，所以显式检查并 ALTER TABLE 补上（默认值为空字符串，旧记录不会自然匹配任何
	// 新算出的 hash，但至少不会导致 SELECT/INSERT 报 "no such column"）。
	hasSnapshotHash, err := columnExists(db, "runs", "snapshot_hash")
	if err != nil {
		return err
	}
	if !hasSnapshotHash {
		if _, err := db.Exec(`ALTER TABLE runs ADD COLUMN snapshot_hash TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}

	_, err = db.Exec(`
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
	snapshotHash := gitdiff.SnapshotHash(run.Snapshot)

	now := time.Now()
	createdAt := run.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO runs (id, repo_path, status, created_at, updated_at, snapshot, snapshot_hash, messages)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status        = excluded.status,
			updated_at    = excluded.updated_at,
			snapshot      = excluded.snapshot,
			snapshot_hash = excluded.snapshot_hash,
			messages      = excluded.messages
	`, run.ID, run.RepoPath, string(run.Status), createdAt.UnixNano(), now.UnixNano(), string(snapshotJSON), snapshotHash, string(messagesJSON))
	if err != nil {
		return fmt.Errorf("save run %s: %w", run.ID, err)
	}
	return nil
}

// LoadRun 按 ID 加载一条运行记录；不存在时返回 (nil, nil)。
func (s *Store) LoadRun(ctx context.Context, id string) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, repo_path, status, created_at, updated_at, snapshot, messages
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
		SELECT id, repo_path, status, created_at, updated_at, snapshot, messages
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
		SELECT id, repo_path, status, created_at, updated_at, snapshot, messages
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

// ListRuns 按 repoPath 列出历史运行，按更新时间倒序，最多 limit 条
// （limit <= 0 时不限制）。供 Web 可视化的列表页使用。
func (s *Store) ListRuns(ctx context.Context, repoPath string, limit int) ([]Run, error) {
	query := `
		SELECT id, repo_path, status, created_at, updated_at, snapshot, messages
		FROM runs WHERE repo_path = ?
		ORDER BY updated_at DESC
	`
	args := []any{repoPath}
	if limit > 0 {
		query += `LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs for %s: %w", repoPath, err)
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

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRun(row rowScanner) (*Run, error) {
	var run Run
	var status string
	var createdAt, updatedAt int64
	var snapshotJSON, messagesJSON string
	if err := row.Scan(&run.ID, &run.RepoPath, &status, &createdAt, &updatedAt, &snapshotJSON, &messagesJSON); err != nil {
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

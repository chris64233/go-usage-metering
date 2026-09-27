package usagemetering

import (
	"context"
	"database/sql"
	"strings"

	_ "modernc.org/sqlite"
)

// schema 是服务的全部持久化结构。首次打开时在一个事务内建表。
const schema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value INTEGER NOT NULL
);

-- 结算周期：租户内按起点排序、首尾相接的半开区间 [start, end)，end=0 表示开放周期。
CREATE TABLE IF NOT EXISTS periods (
	tenant              TEXT    NOT NULL,
	start_unix          INTEGER NOT NULL, -- 纳秒时间戳
	end_unix            INTEGER NOT NULL, -- 0 表示开放中
	closed              INTEGER NOT NULL,
	close_boundary_seq  INTEGER NOT NULL DEFAULT 0,
	closed_at_unix      INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (tenant, start_unix)
);

-- 用量事件。(tenant, event_id) 为外部幂等键；commit_seq 全局单调，唯一。
CREATE TABLE IF NOT EXISTS events (
	event_id                  TEXT    NOT NULL,
	tenant                    TEXT    NOT NULL,
	meter                     TEXT    NOT NULL,
	occurred_at_unix          INTEGER NOT NULL,
	quantity                  TEXT    NOT NULL, -- decimal 规范文本
	commit_seq                INTEGER NOT NULL,
	committed_at_unix         INTEGER NOT NULL,
	period_start_unix         INTEGER NOT NULL, -- 实际承载周期
	is_adjustment             INTEGER NOT NULL,
	origin_period_start_unix  INTEGER NOT NULL, -- 本应归属周期；0 表示早于系统内最早周期
	PRIMARY KEY (tenant, event_id)
);
CREATE INDEX IF NOT EXISTS idx_events_period ON events (tenant, period_start_unix);

-- 增量修正。修正必须引用已存在的原事件；(tenant, correction_id) 为外部幂等键。
CREATE TABLE IF NOT EXISTS corrections (
	correction_id             TEXT    NOT NULL,
	tenant                    TEXT    NOT NULL,
	event_id                  TEXT    NOT NULL,
	meter                     TEXT    NOT NULL, -- 继承自原事件
	occurred_at_unix          INTEGER NOT NULL,
	delta                     TEXT    NOT NULL, -- 有符号增量，decimal 规范文本
	floor                     TEXT    NOT NULL, -- 本次修正声明的业务下限
	commit_seq                INTEGER NOT NULL,
	committed_at_unix         INTEGER NOT NULL,
	period_start_unix         INTEGER NOT NULL,
	is_adjustment             INTEGER NOT NULL,
	origin_period_start_unix  INTEGER NOT NULL,
	PRIMARY KEY (tenant, correction_id),
	FOREIGN KEY (tenant, event_id) REFERENCES events (tenant, event_id)
);
CREATE INDEX IF NOT EXISTS idx_corrections_event ON corrections (tenant, event_id);
CREATE INDEX IF NOT EXISTS idx_corrections_period ON corrections (tenant, period_start_unix);
`

// openDB 打开（必要时创建）一个计量服务存储。
// dsn 为 modernc.org/sqlite 数据源，例如 "file:meter.db"、"/var/data/meter.db" 或 ":memory:"。
//
// 实现将连接池限制为单连接，并由 Service 上的互斥锁串行化所有写入事务，
// 从而让“提交序号分配 + 归属判定 + 关账边界”在进程内线性化，
// 保证上报与关闭并发时每个事件有且仅有一个周期归属。
func openDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "open sqlite %q", dsn)
	}
	// SQLite 单连接：避免文件锁竞争，确保事务与 PRAGMA 作用在同一连接上。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, wrapError(CodeInternal, err, "ping sqlite %q", dsn)
	}

	pragmas := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 10000",
		"PRAGMA journal_mode = WAL", // :memory: 下该语句无害
		"PRAGMA synchronous = FULL",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, wrapError(CodeInternal, err, "set pragma %q", p)
		}
	}

	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return wrapError(CodeInternal, err, "begin migration")
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(schema); err != nil {
		return wrapError(CodeInternal, err, "apply schema")
	}
	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES('commit_seq', 0) ON CONFLICT DO NOTHING`); err != nil {
		return wrapError(CodeInternal, err, "init commit_seq")
	}
	if err := tx.Commit(); err != nil {
		return wrapError(CodeInternal, err, "commit migration")
	}
	return nil
}

// isUniqueErr 判断是否为唯一约束冲突，覆盖 modernc.org/sqlite 的错误文本。
func isUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// isFKErr 判断是否为外键约束冲突（原事件不存在等）。
func isFKErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

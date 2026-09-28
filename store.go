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

-- 费率版本：按 (租户, 计量项, 生效时刻) 发布，发布后不可修改。
-- 同一 (租户, 计量项, 生效时刻) 只允许一份版本（唯一索引），因此任一时刻
-- “生效时间 <= 该时刻”的最新版本唯一，不存在两份费率在同一时刻同时有效。
CREATE TABLE IF NOT EXISTS rate_versions (
	tenant              TEXT    NOT NULL,
	meter               TEXT    NOT NULL,
	version             INTEGER NOT NULL, -- (租户, 计量项) 内单调分配，从 1 开始
	effective_from_unix INTEGER NOT NULL, -- 生效起点（含），按发生时间选择版本
	tiers_json          TEXT    NOT NULL, -- 分段价格不可变副本（JSON）
	currency            TEXT    NOT NULL,
	quantity_scale      INTEGER NOT NULL, -- 数量计价精度（小数位）
	amount_scale        INTEGER NOT NULL, -- 金额计价精度（小数位）
	published_at_unix   INTEGER NOT NULL,
	PRIMARY KEY (tenant, meter, version)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_rate_effective
	ON rate_versions (tenant, meter, effective_from_unix);

-- 账单草稿版本。每个周期可有多个历史版本（作废后保留），但至多一个 current：
-- 部分唯一索引（仅 status='current' 的行参与周期唯一约束）。
CREATE TABLE IF NOT EXISTS drafts (
	tenant              TEXT    NOT NULL,
	period_start_unix   INTEGER NOT NULL,
	version             INTEGER NOT NULL, -- 周期内单调递增，从 1 开始
	status              TEXT    NOT NULL, -- 'current' 或 'voided'
	boundary_seq        INTEGER NOT NULL, -- 冻结的关账提交边界（快照副本）
	total_amount        TEXT    NOT NULL, -- 全部明细金额之和（精确）
	currency            TEXT    NOT NULL,
	created_at_unix     INTEGER NOT NULL,
	voided_at_unix      INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (tenant, period_start_unix, version)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_draft_current
	ON drafts (tenant, period_start_unix) WHERE status = 'current';

-- 草稿计价明细：一行对应快照范围内一条事件或修正。
-- 行不可变且整体从属于一个草稿版本：并发重算时每个调整只会出现在一个版本里。
CREATE TABLE IF NOT EXISTS draft_lines (
	tenant            TEXT    NOT NULL,
	period_start_unix INTEGER NOT NULL,
	draft_version     INTEGER NOT NULL,
	line_seq          INTEGER NOT NULL, -- 草稿内稳定排序
	kind              TEXT    NOT NULL, -- 'event' / 'correction'
	ref_id            TEXT    NOT NULL, -- 事件号或修正号
	event_id          TEXT    NOT NULL, -- 原始事件号
	meter             TEXT    NOT NULL,
	is_adjustment     INTEGER NOT NULL, -- 是否迟到顺延数据（在草稿中单列）
	origin_period_start_unix INTEGER NOT NULL, -- 调整项本应归属的原周期；0 表示非调整项或无更早周期
	occurred_at_unix  INTEGER NOT NULL, -- 计价所依据的发生时间
	quantity          TEXT    NOT NULL, -- 计价数量（修正为有符号增量）
	rate_version      INTEGER NOT NULL, -- 适用的费率版本号
	rate_effective_at_unix INTEGER NOT NULL, -- 该费率版本的生效时间
	pricing_json      TEXT    NOT NULL, -- 冻结的费率副本（tiers/scale/currency）
	unit_amount       TEXT    NOT NULL, -- 分段计价后的单价（摊回，仅供展示）
	amount            TEXT    NOT NULL, -- 计价金额（修正按有符号数量计价，可为负）
	PRIMARY KEY (tenant, period_start_unix, draft_version, line_seq),
	UNIQUE (tenant, period_start_unix, draft_version, kind, ref_id),
	FOREIGN KEY (tenant, period_start_unix, draft_version)
		REFERENCES drafts (tenant, period_start_unix, version)
);
CREATE INDEX IF NOT EXISTS idx_draft_lines_ref
	ON draft_lines (tenant, period_start_unix, kind, ref_id);
CREATE INDEX IF NOT EXISTS idx_draft_lines_meter
	ON draft_lines (tenant, period_start_unix, draft_version, meter, is_adjustment);
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

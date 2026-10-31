package main

import (
	"database/sql"
	"fmt"
	"log"
)

// schema 迁移「台阶」：v0 → v1 → v2 …
//
// 规则（T123）：
//   - 启动时读 PRAGMA user_version，从当前值依次执行未跑过的台阶，每级一个独立函数
//   - 每级跑完把 user_version 抬到该级版本号；已是最新则不执行、不打印
//   - 只允许 CREATE TABLE / CREATE INDEX 新增，禁止 ALTER 老表或改老表列
//   - 加新结构＝在末尾追加一级并抬版本号，老库可从任意旧版本连续升级

type schemaStep struct {
	version int
	note    string
	apply   func(*sql.DB) error
}

var schemaSteps = []schemaStep{
	{version: 1, note: "新增 monitor_*", apply: migrateV1MonitorTables},
	{version: 2, note: "monitor_tokens 增加 base_url", apply: migrateV2MonitorBaseURL},
}

// migrateSchema 从当前 user_version 连续升级到最新。
func migrateSchema(db *sql.DB) error {
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("读取 user_version 失败: %w", err)
	}
	for _, step := range schemaSteps {
		if step.version <= current {
			continue
		}
		if err := step.apply(db); err != nil {
			return fmt.Errorf("schema v%d 失败: %w", step.version-1, err)
		}
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, step.version)); err != nil {
			return fmt.Errorf("写入 user_version=%d 失败: %w", step.version, err)
		}
		log.Printf("schema: v%d -> v%d（%s）", step.version-1, step.version, step.note)
		current = step.version
	}
	return nil
}

// migrateV1MonitorTables v0 → v1：新增三张盯盘表（不改 sections / tasks）。
func migrateV1MonitorTables(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS monitor_subs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    section_id      INTEGER NOT NULL REFERENCES sections(id) ON DELETE CASCADE,
    target_type     TEXT    NOT NULL DEFAULT 'team',
    team_id         TEXT    NOT NULL DEFAULT '',
    slot_id         TEXT    NOT NULL DEFAULT '',
    conversation_id TEXT    NOT NULL DEFAULT '',
    interval_sec    INTEGER NOT NULL DEFAULT 60,
    baseline        TEXT    NOT NULL DEFAULT '',
    last_seen       TEXT    NOT NULL DEFAULT '',
    created_at      TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL,
    UNIQUE(conversation_id, section_id)
);

CREATE TABLE IF NOT EXISTS monitor_tokens (
    conversation_id TEXT PRIMARY KEY,
    target_type     TEXT    NOT NULL DEFAULT 'team',
    team_id         TEXT    NOT NULL DEFAULT '',
    slot_id         TEXT    NOT NULL DEFAULT '',
    token           TEXT    NOT NULL DEFAULT '',
    updated_at      TEXT    NOT NULL,
    last_status     INTEGER NOT NULL DEFAULT 0,
    last_status_at  TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS monitor_pending (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    sub_id     INTEGER NOT NULL REFERENCES monitor_subs(id) ON DELETE CASCADE,
    payload    TEXT    NOT NULL,
    created_at TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_monitor_pending_sub ON monitor_pending(sub_id, id);
`)
	return err
}

// migrateV2MonitorBaseURL v1 → v2：monitor_tokens 增加 base_url（Pager 注册时上报的宿主地址）。
// 只动 v1 新建的 monitor_tokens，不碰 sections / tasks。
func migrateV2MonitorBaseURL(db *sql.DB) error {
	return ensureColumn(db, "monitor_tokens", "base_url", "TEXT NOT NULL DEFAULT ''")
}

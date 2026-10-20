package main

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound   = errors.New("记录不存在")
	ErrDupName    = errors.New("同名分区已存在")
	ErrDupKey     = errors.New("该认证密钥已被其他分区使用")
	ErrBadDue     = errors.New("截止时间格式无效，应为 YYYY-MM-DD HH:MM 或 RFC3339")
	ErrEmptyName  = errors.New("名称不能为空")
	ErrNoTitle    = errors.New("标题不能为空")
	ErrBadKey     = errors.New("认证密钥不能超过 128 个字符")
	ErrNoSection  = errors.New("分区不存在")
	ErrForbidden  = errors.New("该认证密钥不属于此分区")
	ErrBadDone    = errors.New("done 参数仅支持 true / false / all")
	ErrBadStatus  = errors.New("status 仅支持 open / doing / done")
	ErrBadSection = errors.New("section 参数无效")
)

const (
	timeLayout = "2006-01-02T15:04"
	stampFmt   = time.RFC3339
	schemaSQL  = `
CREATE TABLE IF NOT EXISTS sections (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL UNIQUE,
    auth_key   TEXT    NOT NULL UNIQUE,
    sort_order INTEGER NOT NULL DEFAULT 0,
    bark_enabled INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS tasks (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    section_id  INTEGER NOT NULL REFERENCES sections(id) ON DELETE CASCADE,
    title       TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    due_at      TEXT,
    status      TEXT    NOT NULL DEFAULT 'open',
    bark_override TEXT  NOT NULL DEFAULT 'inherit',
    created_at  TEXT    NOT NULL,
    updated_at  TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tasks_section ON tasks(section_id, due_at);`
)

type Section struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	KeyMasked   string `json:"key_masked"`
	SortOrder   int    `json:"sort_order"`
	BarkEnabled bool   `json:"bark_enabled"`
}

type Task struct {
	ID           int64   `json:"id"`
	SectionID    int64   `json:"section_id"`
	Title        string  `json:"title"`
	Description  string  `json:"description"`
	DueAt        *string `json:"due_at"`
	Status       string  `json:"status"`
	Done         bool    `json:"done"`
	BarkOverride string  `json:"bark_override"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

type duePatch struct {
	Set   bool
	Value *string
}

type TaskPatch struct {
	Title        *string
	Description  *string
	Due          duePatch
	BarkOverride *string
}

type Store struct {
	db *sql.DB
}

func OpenStore(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录失败: %w", err)
		}
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化数据表失败: %w", err)
	}
	if err := migrateTaskStatus(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("升级数据表失败: %w", err)
	}
	if err := ensureColumn(db, "sections", "bark_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		db.Close()
		return nil, fmt.Errorf("升级分区表失败: %w", err)
	}
	if err := ensureColumn(db, "tasks", "bark_override", "TEXT NOT NULL DEFAULT '"+"inherit"+"'"); err != nil {
		db.Close()
		return nil, fmt.Errorf("升级任务表失败: %w", err)
	}
	return &Store{db: db}, nil
}

// ensureColumn 在缺列时补列（幂等）。
func ensureColumn(db *sql.DB, table, column, ddl string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	rows.Close()
	if found {
		return nil
	}
	_, err = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + ddl)
	return err
}

// migrateTaskStatus 把旧的 done 布尔列迁移为 status（open/doing/done）。
func migrateTaskStatus(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(tasks)`)
	if err != nil {
		return err
	}
	hasDone, hasStatus := false, false
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		switch name {
		case "done":
			hasDone = true
		case "status":
			hasStatus = true
		}
	}
	rows.Close()
	if !hasDone {
		return nil
	}
	if !hasStatus {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN status TEXT NOT NULL DEFAULT 'open'`); err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE tasks SET status = CASE WHEN done = 1 THEN 'done' ELSE 'open' END`); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`ALTER TABLE tasks DROP COLUMN done`); err != nil {
		// 老版本 SQLite 不支持 DROP COLUMN：保留该列但不再使用
		return nil
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func newKey() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%024x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func maskKey(key string) string {
	runes := []rune(key)
	if len(runes) <= 4 {
		return strings.Repeat("•", 10)
	}
	return strings.Repeat("•", 10) + string(runes[len(runes)-4:])
}

func normalizeDue(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006/01/02 15:04",
	} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t.Format(timeLayout), nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
		return t.Add(23*time.Hour + 59*time.Minute).Format(timeLayout), nil
	}
	return "", ErrBadDue
}

func mapUnique(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "sections.name"):
		return ErrDupName
	case strings.Contains(msg, "sections.auth_key"):
		return ErrDupKey
	}
	return err
}

func affected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanSections(rows *sql.Rows) ([]Section, error) {
	defer rows.Close()
	out := []Section{}
	for rows.Next() {
		var sec Section
		var key string
		var bark int
		if err := rows.Scan(&sec.ID, &sec.Name, &key, &sec.SortOrder, &bark); err != nil {
			return nil, err
		}
		sec.BarkEnabled = bark != 0
		sec.KeyMasked = maskKey(key)
		out = append(out, sec)
	}
	return out, rows.Err()
}

func scanTasks(rows *sql.Rows) ([]Task, error) {
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		var t Task
		var status string
		var due sql.NullString
		var override string
		if err := rows.Scan(&t.ID, &t.SectionID, &t.Title, &t.Description, &due, &status, &override, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		if due.Valid {
			v := due.String
			t.DueAt = &v
		}
		t.Status = status
		t.Done = status == "done"
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) ListSections() ([]Section, error) {
	rows, err := s.db.Query(`SELECT id, name, auth_key, sort_order, bark_enabled FROM sections ORDER BY sort_order ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	return scanSections(rows)
}

func (s *Store) SectionByID(id int64) (Section, error) {
	var sec Section
	var key string
	var bark int
	err := s.db.QueryRow(`SELECT id, name, auth_key, sort_order, bark_enabled FROM sections WHERE id = ?`, id).
		Scan(&sec.ID, &sec.Name, &key, &sec.SortOrder, &bark)
	sec.BarkEnabled = bark != 0
	if errors.Is(err, sql.ErrNoRows) {
		return Section{}, ErrNotFound
	}
	if err != nil {
		return Section{}, err
	}
	sec.KeyMasked = maskKey(key)
	return sec, nil
}

func (s *Store) SectionByKey(key string) (Section, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return Section{}, ErrNotFound
	}
	rows, err := s.db.Query(`SELECT id, name, auth_key, sort_order, bark_enabled FROM sections ORDER BY sort_order ASC, id ASC`)
	if err != nil {
		return Section{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var sec Section
		var stored string
		var bark int
		if err := rows.Scan(&sec.ID, &sec.Name, &stored, &sec.SortOrder, &bark); err != nil {
			return Section{}, err
		}
		if subtle.ConstantTimeCompare([]byte(stored), []byte(key)) == 1 {
			sec.BarkEnabled = bark != 0
			sec.KeyMasked = maskKey(stored)
			return sec, nil
		}
	}
	if err := rows.Err(); err != nil {
		return Section{}, err
	}
	return Section{}, ErrNotFound
}

func (s *Store) ResolveSection(q string) (Section, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return Section{}, ErrNoSection
	}
	if id, err := strconv.ParseInt(q, 10, 64); err == nil {
		sec, err := s.SectionByID(id)
		if err == nil {
			return sec, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return Section{}, err
		}
	}
	var sec Section
	var key string
	var bark int
	err := s.db.QueryRow(`SELECT id, name, auth_key, sort_order, bark_enabled FROM sections WHERE name = ?`, q).
		Scan(&sec.ID, &sec.Name, &key, &sec.SortOrder, &bark)
	sec.BarkEnabled = bark != 0
	if errors.Is(err, sql.ErrNoRows) {
		return Section{}, ErrNoSection
	}
	if err != nil {
		return Section{}, err
	}
	sec.KeyMasked = maskKey(key)
	return sec, nil
}

func (s *Store) CreateSection(name, key string) (Section, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Section{}, "", ErrEmptyName
	}
	key = strings.TrimSpace(key)
	if key == "" {
		key = newKey()
	}
	if len([]rune(key)) > 128 {
		return Section{}, "", ErrBadKey
	}
	next := 0
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(sort_order), 0) + 1 FROM sections`).Scan(&next); err != nil {
		return Section{}, "", err
	}
	res, err := s.db.Exec(`INSERT INTO sections(name, auth_key, sort_order) VALUES(?, ?, ?)`, name, key, next)
	if err != nil {
		return Section{}, "", mapUnique(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Section{}, "", err
	}
	return Section{ID: id, Name: name, KeyMasked: maskKey(key), SortOrder: next}, key, nil
}

func (s *Store) RenameSection(id int64, name string) (Section, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Section{}, ErrEmptyName
	}
	res, err := s.db.Exec(`UPDATE sections SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return Section{}, mapUnique(err)
	}
	if err := affected(res); err != nil {
		return Section{}, err
	}
	return s.SectionByID(id)
}

func (s *Store) SetSectionKey(id int64, key string) (Section, string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		key = newKey()
	}
	if len([]rune(key)) > 128 {
		return Section{}, "", ErrBadKey
	}
	res, err := s.db.Exec(`UPDATE sections SET auth_key = ? WHERE id = ?`, key, id)
	if err != nil {
		return Section{}, "", mapUnique(err)
	}
	if err := affected(res); err != nil {
		return Section{}, "", err
	}
	sec, err := s.SectionByID(id)
	if err != nil {
		return Section{}, "", err
	}
	return sec, key, nil
}

func (s *Store) SectionKey(id int64) (string, error) {
	var key string
	err := s.db.QueryRow(`SELECT auth_key FROM sections WHERE id = ?`, id).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return key, nil
}

func (s *Store) ListTasks(sectionID *int64, status *string) ([]Task, error) {
	q := `SELECT id, section_id, title, description, due_at, status, bark_override, created_at, updated_at FROM tasks`
	var conds []string
	var args []any
	if sectionID != nil {
		conds = append(conds, "section_id = ?")
		args = append(args, *sectionID)
	}
	if status != nil && *status != "" && *status != "all" {
		if !validStatus[*status] {
			return nil, ErrBadStatus
		}
		conds = append(conds, "status = ?")
		args = append(args, *status)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY CASE status WHEN 'open' THEN 0 WHEN 'doing' THEN 1 ELSE 2 END, (due_at IS NULL) DESC, due_at ASC, id ASC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return scanTasks(rows)
}

func (s *Store) TaskByID(sectionID, id int64) (Task, error) {
	var t Task
	var status string
	var due sql.NullString
	err := s.db.QueryRow(
		`SELECT id, section_id, title, description, due_at, status, bark_override, created_at, updated_at FROM tasks WHERE id = ? AND section_id = ?`,
		id, sectionID,
	).Scan(&t.ID, &t.SectionID, &t.Title, &t.Description, &due, &status, &t.BarkOverride, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, err
	}
	if due.Valid {
		v := due.String
		t.DueAt = &v
	}
	t.Status = status
	t.Done = status == "done"
	if t.BarkOverride == "" {
		t.BarkOverride = "inherit"
	}
	return t, nil
}

func (s *Store) CreateTask(sectionID int64, title, description string, due *string) (Task, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Task{}, ErrNoTitle
	}
	if _, err := s.SectionByID(sectionID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Task{}, ErrNoSection
		}
		return Task{}, err
	}
	var dueVal any
	if due != nil {
		normalized, err := normalizeDue(*due)
		if err != nil {
			return Task{}, err
		}
		if normalized != "" {
			dueVal = normalized
		}
	}
	now := time.Now().Format(stampFmt)
	res, err := s.db.Exec(
		`INSERT INTO tasks(section_id, title, description, due_at, status, created_at, updated_at) VALUES(?, ?, ?, ?, 'open', ?, ?)`,
		sectionID, title, description, dueVal, now, now,
	)
	if err != nil {
		return Task{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Task{}, err
	}
	return s.TaskByID(sectionID, id)
}

func (s *Store) UpdateTask(sectionID, id int64, p TaskPatch) (Task, error) {
	current, err := s.TaskByID(sectionID, id)
	if err != nil {
		return Task{}, err
	}
	title := current.Title
	description := current.Description
	dueVal := any(nil)
	if current.DueAt != nil {
		dueVal = *current.DueAt
	}
	if p.Title != nil {
		title = strings.TrimSpace(*p.Title)
		if title == "" {
			return Task{}, ErrNoTitle
		}
	}
	if p.Description != nil {
		description = *p.Description
	}
	if p.Due.Set {
		if p.Due.Value == nil {
			dueVal = nil
		} else {
			normalized, err := normalizeDue(*p.Due.Value)
			if err != nil {
				return Task{}, err
			}
			if normalized == "" {
				dueVal = nil
			} else {
				dueVal = normalized
			}
		}
	}
	res, err := s.db.Exec(
		`UPDATE tasks SET title = ?, description = ?, due_at = ?, updated_at = ? WHERE id = ? AND section_id = ?`,
		title, description, dueVal, time.Now().Format(stampFmt), id, sectionID,
	)
	if err != nil {
		return Task{}, err
	}
	if err := affected(res); err != nil {
		return Task{}, err
	}
	return s.TaskByID(sectionID, id)
}

var validStatus = map[string]bool{"open": true, "doing": true, "done": true}

// SetStatus 设置任务状态（open / doing / done）。
func (s *Store) SetStatus(sectionID, id int64, status string) (Task, error) {
	status = strings.TrimSpace(status)
	if !validStatus[status] {
		return Task{}, ErrBadStatus
	}
	res, err := s.db.Exec(
		`UPDATE tasks SET status = ?, updated_at = ? WHERE id = ? AND section_id = ?`,
		status, time.Now().Format(stampFmt), id, sectionID,
	)
	if err != nil {
		return Task{}, err
	}
	if err := affected(res); err != nil {
		return Task{}, err
	}
	return s.TaskByID(sectionID, id)
}

// SetDone 兼容旧接口：true → done，false → open，nil → 在 done/open 间切换。
func (s *Store) SetDone(sectionID, id int64, done *bool) (Task, error) {
	if done != nil {
		if *done {
			return s.SetStatus(sectionID, id, "done")
		}
		return s.SetStatus(sectionID, id, "open")
	}
	current, err := s.TaskByID(sectionID, id)
	if err != nil {
		return Task{}, err
	}
	if current.Status == "done" {
		return s.SetStatus(sectionID, id, "open")
	}
	return s.SetStatus(sectionID, id, "done")
}

func (s *Store) DeleteTask(sectionID, id int64) error {
	res, err := s.db.Exec(`DELETE FROM tasks WHERE id = ? AND section_id = ?`, id, sectionID)
	if err != nil {
		return err
	}
	return affected(res)
}

/* ---------------- 排序 / 分页 / 完成推送开关 ---------------- */

// SetSectionBark 开关分区「任务完成推送」。
func (s *Store) SetSectionBark(id int64, enabled bool) (Section, error) {
	val := 0
	if enabled {
		val = 1
	}
	res, err := s.db.Exec(`UPDATE sections SET bark_enabled = ? WHERE id = ?`, val, id)
	if err != nil {
		return Section{}, err
	}
	if err := affected(res); err != nil {
		return Section{}, err
	}
	return s.SectionByID(id)
}

type SectionOrder struct {
	ID        int64 `json:"id"`
	SortOrder int   `json:"sort_order"`
}

// ReorderSections 按给定顺序重排分区，并把 sort_order 归一化为 0..n-1。
func (s *Store) ReorderSections(items []SectionOrder) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, it := range items {
		if _, err := tx.Exec(`UPDATE sections SET sort_order = ? WHERE id = ?`, i, it.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListSectionsPaged 分页取分区（limit<=0 表示全部），返回总数。
func (s *Store) ListSectionsPaged(limit, offset int) ([]Section, int, error) {
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sections`).Scan(&total); err != nil {
		return nil, 0, err
	}
	q := `SELECT id, name, auth_key, sort_order, bark_enabled FROM sections ORDER BY sort_order ASC, id ASC`
	args := []any{}
	if limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, 0, err
	}
	list, err := scanSections(rows)
	return list, total, err
}

// ListTasksPaged 分页取任务（limit<=0 表示全部），返回总数。
func (s *Store) ListTasksPaged(sectionID *int64, status *string, limit, offset int) ([]Task, int, error) {
	where := ""
	args := []any{}
	if sectionID != nil {
		where = ` WHERE section_id = ?`
		args = append(args, *sectionID)
	}
	if status != nil && *status != "" && *status != "all" {
		if !validStatus[*status] {
			return nil, 0, ErrBadStatus
		}
		if where == "" {
			where = ` WHERE status = ?`
		} else {
			where += ` AND status = ?`
		}
		args = append(args, *status)
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	q := `SELECT id, section_id, title, description, due_at, status, bark_override, created_at, updated_at FROM tasks` + where +
		` ORDER BY CASE status WHEN 'open' THEN 0 WHEN 'doing' THEN 1 ELSE 2 END, (due_at IS NULL) DESC, due_at ASC, id ASC`
	queryArgs := append([]any{}, args...)
	if limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		queryArgs = append(queryArgs, limit, offset)
	}
	rows, err := s.db.Query(q, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	list, err := scanTasks(rows)
	return list, total, err
}

type SectionCounts struct {
	Total   int `json:"total"`
	Open    int `json:"open"`
	Doing   int `json:"doing"`
	Done    int `json:"done"`
	Overdue int `json:"overdue"`
}

// CountsBySection 一次聚合出每个分区的计数（供分页接口使用，不拉全量任务）。
func (s *Store) CountsBySection(now string) (map[int64]SectionCounts, error) {
	rows, err := s.db.Query(`SELECT section_id,
		COUNT(*),
		SUM(CASE WHEN status = 'open' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status = 'doing' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status = 'done' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status != 'done' AND due_at IS NOT NULL AND due_at < ? THEN 1 ELSE 0 END)
		FROM tasks GROUP BY section_id`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]SectionCounts{}
	for rows.Next() {
		var id int64
		var c SectionCounts
		if err := rows.Scan(&id, &c.Total, &c.Open, &c.Doing, &c.Done, &c.Overdue); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// SetBarkOverride 设置任务级完成推送覆盖（inherit / on / off）。
func (s *Store) SetBarkOverride(sectionID, id int64, override string) (Task, error) {
	v := strings.TrimSpace(override)
	switch v {
	case "inherit", "on", "off":
	default:
		return Task{}, ErrBadStatus
	}
	res, err := s.db.Exec(`UPDATE tasks SET bark_override = ?, updated_at = ? WHERE id = ? AND section_id = ?`,
		v, time.Now().Format(stampFmt), id, sectionID)
	if err != nil {
		return Task{}, err
	}
	if err := affected(res); err != nil {
		return Task{}, err
	}
	return s.TaskByID(sectionID, id)
}

// DeleteSection 硬删除分区，并在同一事务内显式删除其全部任务（不依赖外键级联）。
// 返回被删除的任务数。
func (s *Store) DeleteSection(id int64) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`DELETE FROM tasks WHERE section_id = ?`, id)
	if err != nil {
		return 0, err
	}
	deleted := 0
	if n, err := res.RowsAffected(); err == nil {
		deleted = int(n)
	}
	res2, err := tx.Exec(`DELETE FROM sections WHERE id = ?`, id)
	if err != nil {
		return 0, err
	}
	if n, err := res2.RowsAffected(); err == nil && n == 0 {
		return 0, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

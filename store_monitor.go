package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 盯盘（T123）数据层：三张新表只在 schemaSQL 里定义，本文件只做读写。
//
// 安全约定：monitor_tokens.token 永不进日志、永不进错误文本、永不进 API 响应。
// MonitorSub 的 Token 字段带 json:"-"，对外结构体（monitorView）也不含 token。

type MonitorSub struct {
	ID             int64  `json:"id"`
	SectionID      int64  `json:"section_id"`
	SectionName    string `json:"section_name,omitempty"`
	TargetType     string `json:"target_type"`
	TeamID         string `json:"team_id"`
	SlotID         string `json:"slot_id"`
	ConversationID string `json:"conversation_id"`
	IntervalSec    int    `json:"interval_sec"`
	LastSeen       string `json:"last_seen"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`

	Baseline string `json:"-"`
	Token    string `json:"-"`
}

type MonitorPending struct {
	ID        int64  `json:"id"`
	SubID     int64  `json:"sub_id"`
	Payload   string `json:"payload"`
	CreatedAt string `json:"created_at"`
}

// MonitorInput 注册 / 心跳共用入参（SectionID 由鉴权后的分区决定）。
type MonitorInput struct {
	TargetType     string
	TeamID         string
	SlotID         string
	ConversationID string
	IntervalSec    int
	Token          string
}

func normalizeTargetType(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "team":
		return "team", nil
	case "conv", "conversation":
		return "conv", nil
	}
	return "", ErrBadBody
}

// UpsertMonitor 按 (conversation_id, section_id) 幂等：已存在则更新间隔/目标并保留 baseline。
func (s *Store) UpsertMonitor(sectionID int64, in MonitorInput) (MonitorSub, bool, error) {
	targetType, err := normalizeTargetType(in.TargetType)
	if err != nil {
		return MonitorSub{}, false, err
	}
	if strings.TrimSpace(in.ConversationID) == "" {
		return MonitorSub{}, false, ErrBadBody
	}
	if targetType == "team" && (strings.TrimSpace(in.TeamID) == "" || strings.TrimSpace(in.SlotID) == "") {
		return MonitorSub{}, false, ErrBadBody
	}
	interval := in.IntervalSec
	if interval <= 0 {
		interval = defaultMonitorInterval
	}
	if interval < minMonitorInterval {
		interval = minMonitorInterval
	}
	if interval > maxMonitorInterval {
		interval = maxMonitorInterval
	}
	now := time.Now().Format(stampFmt)

	var id int64
	err = s.db.QueryRow(`SELECT id FROM monitor_subs WHERE conversation_id = ? AND section_id = ?`,
		in.ConversationID, sectionID).Scan(&id)
	if err == nil {
		if _, err := s.db.Exec(`UPDATE monitor_subs
SET target_type = ?, team_id = ?, slot_id = ?, interval_sec = ?, updated_at = ?
WHERE id = ?`,
			targetType, strings.TrimSpace(in.TeamID), strings.TrimSpace(in.SlotID), interval, now, id); err != nil {
			return MonitorSub{}, false, err
		}
		if err := s.UpsertMonitorToken(in.ConversationID, targetType, in.TeamID, in.SlotID, in.Token, now); err != nil {
			return MonitorSub{}, false, err
		}
		sub, err := s.MonitorByID(id)
		return sub, false, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return MonitorSub{}, false, err
	}

	res, err := s.db.Exec(`INSERT INTO monitor_subs
(section_id, target_type, team_id, slot_id, conversation_id, interval_sec, baseline, last_seen, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, '', ?, ?, ?)`,
		sectionID, targetType, strings.TrimSpace(in.TeamID), strings.TrimSpace(in.SlotID), in.ConversationID, interval, now, now, now)
	if err != nil {
		return MonitorSub{}, false, err
	}
	id, err = res.LastInsertId()
	if err != nil {
		return MonitorSub{}, false, err
	}
	if err := s.UpsertMonitorToken(in.ConversationID, targetType, in.TeamID, in.SlotID, in.Token, now); err != nil {
		return MonitorSub{}, false, err
	}
	sub, err := s.MonitorByID(id)
	return sub, true, err
}

// MonitorHeartbeat 只更新不新建：行不存在返回 ErrNotFound（＝盯盘已被清除）。
func (s *Store) MonitorHeartbeat(sectionID int64, conversationID, token string) (MonitorSub, error) {
	now := time.Now().Format(stampFmt)
	res, err := s.db.Exec(`UPDATE monitor_subs SET last_seen = ?, updated_at = ? WHERE conversation_id = ? AND section_id = ?`,
		now, now, conversationID, sectionID)
	if err != nil {
		return MonitorSub{}, err
	}
	if err := affected(res); err != nil {
		return MonitorSub{}, err
	}
	if strings.TrimSpace(token) != "" {
		if err := s.UpsertMonitorToken(conversationID, "", "", "", token, now); err != nil {
			return MonitorSub{}, err
		}
	}
	return s.MonitorByConversation(sectionID, conversationID)
}

func (s *Store) MonitorByID(id int64) (MonitorSub, error) {
	row := s.db.QueryRow(`SELECT m.id, m.section_id, s.name, m.target_type, m.team_id, m.slot_id,
       m.conversation_id, m.interval_sec, m.baseline, m.last_seen, m.created_at, m.updated_at
FROM monitor_subs m JOIN sections s ON s.id = m.section_id WHERE m.id = ?`, id)
	return scanMonitorSub(row)
}

func (s *Store) MonitorByConversation(sectionID int64, conversationID string) (MonitorSub, error) {
	row := s.db.QueryRow(`SELECT m.id, m.section_id, s.name, m.target_type, m.team_id, m.slot_id,
       m.conversation_id, m.interval_sec, m.baseline, m.last_seen, m.created_at, m.updated_at
FROM monitor_subs m JOIN sections s ON s.id = m.section_id WHERE m.section_id = ? AND m.conversation_id = ?`,
		sectionID, conversationID)
	return scanMonitorSub(row)
}

type scanner interface{ Scan(dest ...any) error }

func scanMonitorSub(row scanner) (MonitorSub, error) {
	var m MonitorSub
	err := row.Scan(&m.ID, &m.SectionID, &m.SectionName, &m.TargetType, &m.TeamID, &m.SlotID,
		&m.ConversationID, &m.IntervalSec, &m.Baseline, &m.LastSeen, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MonitorSub{}, ErrNotFound
	}
	if err != nil {
		return MonitorSub{}, err
	}
	return m, nil
}

// ListMonitors sectionID 为 nil 时列出全部（内核用），不限分区。
func (s *Store) ListMonitors(sectionID *int64) ([]MonitorSub, error) {
	q := `SELECT m.id, m.section_id, s.name, m.target_type, m.team_id, m.slot_id,
       m.conversation_id, m.interval_sec, m.baseline, m.last_seen, m.created_at, m.updated_at
FROM monitor_subs m JOIN sections s ON s.id = m.section_id`
	args := []any{}
	if sectionID != nil {
		q += ` WHERE m.section_id = ?`
		args = append(args, *sectionID)
	}
	q += ` ORDER BY m.section_id, m.id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MonitorSub{}
	for rows.Next() {
		m, err := scanMonitorSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) DeleteMonitor(id int64) error {
	res, err := s.db.Exec(`DELETE FROM monitor_subs WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

func (s *Store) DeleteMonitorsBySection(sectionID int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM monitor_subs WHERE section_id = ?`, sectionID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) DeleteAllMonitors() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM monitor_subs`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) SetMonitorBaseline(id int64, baseline string) error {
	_, err := s.db.Exec(`UPDATE monitor_subs SET baseline = ?, updated_at = ? WHERE id = ?`,
		baseline, time.Now().Format(stampFmt), id)
	return err
}

func (s *Store) MonitorCountsBySection() (map[int64]int, error) {
	rows, err := s.db.Query(`SELECT section_id, COUNT(*) FROM monitor_subs GROUP BY section_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var sid int64
		var n int
		if err := rows.Scan(&sid, &n); err != nil {
			return nil, err
		}
		out[sid] = n
	}
	return out, rows.Err()
}

/* ---------------- token ---------------- */

func (s *Store) UpsertMonitorToken(conversationID, targetType, teamID, slotID, token, now string) error {
	if strings.TrimSpace(conversationID) == "" {
		return nil
	}
	if strings.TrimSpace(token) == "" {
		// 没有新 token 就只补目标信息，不清空旧 token
		_, err := s.db.Exec(`UPDATE monitor_tokens SET target_type = COALESCE(NULLIF(?, ''), target_type),
       team_id = COALESCE(NULLIF(?, ''), team_id), slot_id = COALESCE(NULLIF(?, ''), slot_id), updated_at = ?
WHERE conversation_id = ?`, targetType, teamID, slotID, now, conversationID)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO monitor_tokens
(conversation_id, target_type, team_id, slot_id, token, updated_at, last_status, last_status_at)
VALUES (?, ?, ?, ?, ?, ?, 0, '')
ON CONFLICT(conversation_id) DO UPDATE SET
  target_type = COALESCE(NULLIF(excluded.target_type, ''), target_type),
  team_id     = COALESCE(NULLIF(excluded.team_id, ''), team_id),
  slot_id     = COALESCE(NULLIF(excluded.slot_id, ''), slot_id),
  token       = excluded.token,
  updated_at  = excluded.updated_at`,
		conversationID, targetType, teamID, slotID, token, now)
	return err
}

// MonitorToken 取某会话的 token（不存在返回空串，不报错：token 缺失按「跳过」处理）。
func (s *Store) MonitorToken(conversationID string) string {
	var tok string
	if err := s.db.QueryRow(`SELECT token FROM monitor_tokens WHERE conversation_id = ?`, conversationID).Scan(&tok); err != nil {
		return ""
	}
	return tok
}

func (s *Store) SetMonitorTokenStatus(conversationID string, status int, at string) error {
	_, err := s.db.Exec(`UPDATE monitor_tokens SET last_status = ?, last_status_at = ? WHERE conversation_id = ?`,
		status, at, conversationID)
	return err
}

// MonitorTokenStatuses 供 GET /api/monitors 读取每个会话最近一次推送结果。
func (s *Store) MonitorTokenStatuses() (map[string][2]string, error) {
	rows, err := s.db.Query(`SELECT conversation_id, CAST(last_status AS TEXT), last_status_at FROM monitor_tokens`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]string{}
	for rows.Next() {
		var cid, status, at string
		if err := rows.Scan(&cid, &status, &at); err != nil {
			return nil, err
		}
		out[cid] = [2]string{status, at}
	}
	return out, rows.Err()
}

// LeaseTargets 供续租：按团队/会话聚合出 (target_type, id) 与候选 token。
type LeaseTarget struct {
	TargetType     string
	TeamID         string
	ConversationID string
	Tokens         []string
}

func (s *Store) LeaseTargets() ([]LeaseTarget, error) {
	rows, err := s.db.Query(`SELECT conversation_id, target_type, team_id, token FROM monitor_tokens ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byKey := map[string]*LeaseTarget{}
	order := []string{}
	for rows.Next() {
		var cid, tt, team, tok string
		if err := rows.Scan(&cid, &tt, &team, &tok); err != nil {
			return nil, err
		}
		key := tt + "|" + team
		if tt != "team" {
			key = "conv|" + cid
		}
		lt, ok := byKey[key]
		if !ok {
			lt = &LeaseTarget{TargetType: tt, TeamID: team, ConversationID: cid}
			byKey[key] = lt
			order = append(order, key)
		}
		if tok != "" {
			lt.Tokens = append(lt.Tokens, tok)
		}
	}
	out := make([]LeaseTarget, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out, rows.Err()
}

/* ---------------- pending ---------------- */

func (s *Store) EnqueueMonitorPending(subID int64, payload string) error {
	_, err := s.db.Exec(`INSERT INTO monitor_pending (sub_id, payload, created_at) VALUES (?, ?, ?)`,
		subID, payload, time.Now().Format(stampFmt))
	return err
}

func (s *Store) ListMonitorPending(subID int64) ([]MonitorPending, error) {
	rows, err := s.db.Query(`SELECT id, sub_id, payload, created_at FROM monitor_pending WHERE sub_id = ? ORDER BY id`, subID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MonitorPending{}
	for rows.Next() {
		var p MonitorPending
		if err := rows.Scan(&p.ID, &p.SubID, &p.Payload, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ClearMonitorPending(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	parts := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		parts[i] = "?"
		args[i] = id
	}
	_, err := s.db.Exec(`DELETE FROM monitor_pending WHERE id IN (`+strings.Join(parts, ",")+`)`, args...)
	return err
}

/* ---------------- 看板快照（每 tick 只查一次库） ---------------- */

// BoardSnapshot 用一条 JOIN SQL 读出全部分区与任务，供内核分发给各区线程算 diff。
func (s *Store) BoardSnapshot() ([]Section, []Task, error) {
	rows, err := s.db.Query(`
SELECT s.id, s.name, s.auth_key, s.sort_order, s.bark_enabled,
       t.id, t.section_id, t.title, t.description, t.due_at, t.status, t.bark_override, t.created_at, t.updated_at
FROM sections s
LEFT JOIN tasks t ON t.section_id = s.id
ORDER BY s.sort_order, s.id,
         CASE t.status WHEN 'open' THEN 0 WHEN 'doing' THEN 1 ELSE 2 END,
         (t.due_at IS NULL) DESC, t.due_at, t.id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	sections := []Section{}
	byID := map[int64]int{}
	tasks := []Task{}
	for rows.Next() {
		var sec Section
		var key string
		var tid sql.NullInt64
		var tSection sql.NullInt64
		var title, desc, status, barkOv, created, updated sql.NullString
		var due sql.NullString
		if err := rows.Scan(&sec.ID, &sec.Name, &key, &sec.SortOrder, &sec.BarkEnabled,
			&tid, &tSection, &title, &desc, &due, &status, &barkOv, &created, &updated); err != nil {
			return nil, nil, err
		}
		if _, ok := byID[sec.ID]; !ok {
			sec.KeyMasked = maskKey(key)
			byID[sec.ID] = len(sections)
			sections = append(sections, sec)
		}
		if !tid.Valid {
			continue
		}
		var duePtr *string
		if due.Valid {
			v := due.String
			duePtr = &v
		}
		tasks = append(tasks, Task{
			ID:           tid.Int64,
			SectionID:    tSection.Int64,
			Title:        title.String,
			Description:  desc.String,
			DueAt:        duePtr,
			Status:       status.String,
			Done:         status.String == "done",
			BarkOverride: barkOv.String,
			CreatedAt:    created.String,
			UpdatedAt:    updated.String,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return sections, tasks, nil
}

/* ---------------- 快照与 diff ---------------- */

type taskSnap struct {
	Title       string  `json:"t"`
	Description string  `json:"d"`
	DueAt       *string `json:"due"`
	Status      string  `json:"s"`
}

type sectionSnap struct {
	Name        string              `json:"name"`
	SortOrder   int                 `json:"sort"`
	BarkEnabled bool                `json:"bark"`
	Tasks       map[string]taskSnap `json:"tasks"`
}

// buildSectionSnap 生成分区快照（JSON）。字段精简，避免帮助文本/大描述造成基线膨胀。
func buildSectionSnap(sec Section, tasks []Task) sectionSnap {
	snap := sectionSnap{
		Name:        sec.Name,
		SortOrder:   sec.SortOrder,
		BarkEnabled: sec.BarkEnabled,
		Tasks:       map[string]taskSnap{},
	}
	for _, t := range tasks {
		snap.Tasks[fmt.Sprintf("%d", t.ID)] = taskSnap{
			Title:       t.Title,
			Description: t.Description,
			DueAt:       t.DueAt,
			Status:      t.Status,
		}
	}
	return snap
}

type snapChange struct {
	Kind   string   `json:"kind"` // new | changed | deleted | section
	TaskID int64    `json:"task_id,omitempty"`
	Title  string   `json:"title"`
	Fields []string `json:"fields,omitempty"`
	Note   string   `json:"note,omitempty"`
}

// diffSectionSnap 对比基线，返回变化列表（新增 / 变更 / 删除 / 分区字段）。
func diffSectionSnap(prev, cur sectionSnap) []snapChange {
	out := []snapChange{}
	if prev.Name != cur.Name {
		out = append(out, snapChange{Kind: "section", Note: "分区名 " + prev.Name + " → " + cur.Name})
	}
	if prev.BarkEnabled != cur.BarkEnabled {
		out = append(out, snapChange{Kind: "section", Note: fmt.Sprintf("完成推送开关 %v → %v", prev.BarkEnabled, cur.BarkEnabled)})
	}
	if prev.SortOrder != cur.SortOrder {
		out = append(out, snapChange{Kind: "section", Note: fmt.Sprintf("分区排序 %d → %d", prev.SortOrder, cur.SortOrder)})
	}
	ids := map[string]bool{}
	for id := range cur.Tasks {
		ids[id] = true
	}
	for id := range prev.Tasks {
		ids[id] = true
	}
	for id, t := range cur.Tasks {
		p, ok := prev.Tasks[id]
		if !ok {
			var taskID int64
			fmt.Sscanf(id, "%d", &taskID)
			out = append(out, snapChange{Kind: "new", TaskID: taskID, Title: t.Title})
			continue
		}
		fields := []string{}
		if p.Title != t.Title {
			fields = append(fields, "标题")
		}
		if p.Description != t.Description {
			fields = append(fields, "描述")
		}
		if p.Status != t.Status {
			fields = append(fields, "状态")
		}
		if derefStr(p.DueAt) != derefStr(t.DueAt) {
			fields = append(fields, "截止")
		}
		if len(fields) > 0 {
			var taskID int64
			fmt.Sscanf(id, "%d", &taskID)
			out = append(out, snapChange{Kind: "changed", TaskID: taskID, Title: t.Title, Fields: fields})
		}
	}
	for id, p := range prev.Tasks {
		if _, ok := cur.Tasks[id]; ok {
			continue
		}
		var taskID int64
		fmt.Sscanf(id, "%d", &taskID)
		out = append(out, snapChange{Kind: "deleted", TaskID: taskID, Title: p.Title})
	}
	return out
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func encodeSnap(s sectionSnap) string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeSnap(raw string) (sectionSnap, bool) {
	if strings.TrimSpace(raw) == "" {
		return sectionSnap{}, false
	}
	var s sectionSnap
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return sectionSnap{}, false
	}
	if s.Tasks == nil {
		s.Tasks = map[string]taskSnap{}
	}
	return s, true
}

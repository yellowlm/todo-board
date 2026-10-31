package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 盯盘管理 API（T123）。鉴权沿用分区 key：X-Auth-Key 决定分区，
// ?section= 只用于选择分区（与 key 不一致按 403 处理）。

type monitorBody struct {
	Section        *string `json:"section"`
	SectionID      *int64  `json:"section_id"`
	TargetType     string  `json:"target_type"`
	TeamID         string  `json:"team_id"`
	SlotID         string  `json:"slot_id"`
	ConversationID string  `json:"conversation_id"`
	IntervalSec    *int    `json:"interval_sec"`
	Token          string  `json:"token"`
}

// monitorView 对外结构：绝不含 token / baseline。
type monitorView struct {
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
	LastStatus     int    `json:"last_status"`
	LastStatusAt   string `json:"last_status_at"`
	TeamName       string `json:"team_name"`
	SlotName       string `json:"slot_name"`
}

func (s *Server) handleMonitorCreate(w http.ResponseWriter, r *http.Request) {
	sec, ok := sectionFrom(r)
	if !ok {
		writeErr(w, ErrBadAuthKey)
		return
	}
	var in monitorBody
	if !decodeBody(w, r, &in) {
		return
	}
	interval := defaultMonitorInterval
	if in.IntervalSec != nil {
		interval = *in.IntervalSec
	}
	if interval < minMonitorInterval || interval > maxMonitorInterval {
		writeErr(w, ErrBadBody)
		return
	}
	sub, created, err := s.store.UpsertMonitor(sec.ID, MonitorInput{
		TargetType:     in.TargetType,
		TeamID:         in.TeamID,
		SlotID:         in.SlotID,
		ConversationID: in.ConversationID,
		IntervalSec:    interval,
		Token:          in.Token,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	views := s.monitorViews([]MonitorSub{sub})
	writeJSON(w, code, map[string]any{"monitor": views[0], "created": created})
}

func (s *Server) handleMonitorHeartbeat(w http.ResponseWriter, r *http.Request) {
	sec, ok := sectionFrom(r)
	if !ok {
		writeErr(w, ErrBadAuthKey)
		return
	}
	var in struct {
		ConversationID string `json:"conversation_id"`
		Token          string `json:"token"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.ConversationID) == "" {
		writeErr(w, ErrBadBody)
		return
	}
	sub, err := s.store.MonitorHeartbeat(sec.ID, in.ConversationID, in.Token)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// 语义＝盯盘已被清除（pager 收到就彻底停轮询）
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "盯盘已被清除"})
			return
		}
		writeErr(w, err)
		return
	}
	views := s.monitorViews([]MonitorSub{sub})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "monitor": views[0], "push_by": "kernel"})
}

// handleMonitorDelete 管理面单条删除：与「删除分区」同级，不要求分区 key。
func (s *Server) handleMonitorDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"), ErrNotFound)
	if !ok {
		return
	}
	if err := s.store.DeleteMonitor(id); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": 1, "id": id})
}

// handleMonitorDeleteSelf pager 面注销：必须带本分区 key，且只能删自己分区的行。
func (s *Server) handleMonitorDeleteSelf(w http.ResponseWriter, r *http.Request) {
	sec, ok := sectionFrom(r)
	if !ok {
		writeErr(w, ErrBadAuthKey)
		return
	}
	id, ok := parseID(w, r.PathValue("id"), ErrNotFound)
	if !ok {
		return
	}
	sub, err := s.store.MonitorByID(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if sub.SectionID != sec.ID {
		writeErr(w, ErrNotFound)
		return
	}
	if err := s.store.DeleteMonitor(id); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": 1, "id": id})
}

// handleMonitorDeleteMany 管理面批量删除：?section=N 只清该区；全清必须显式 ?all=1。
func (s *Server) handleMonitorDeleteMany(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	raw := strings.TrimSpace(q.Get("section"))
	if raw != "" {
		sid, err := s.store.ResolveSection(raw)
		if err != nil {
			writeErr(w, err)
			return
		}
		n, err := s.store.DeleteMonitorsBySection(sid.ID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": n, "section": sid.ID})
		return
	}
	if v := strings.TrimSpace(q.Get("all")); v != "1" {
		// 不带 section 又不显式 all=1：拒绝，避免误清全部
		writeErr(w, ErrBadBody)
		return
	}
	n, err := s.store.DeleteAllMonitors()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": n})
}

// handleMonitorList 管理面列表：?section=N 过滤该区，不带则列全部（不含 token）。
func (s *Server) handleMonitorList(w http.ResponseWriter, r *http.Request) {
	var filter *int64
	resp := map[string]any{}
	if raw := strings.TrimSpace(r.URL.Query().Get("section")); raw != "" {
		sid, err := s.store.ResolveSection(raw)
		if err != nil {
			writeErr(w, err)
			return
		}
		filter = &sid.ID
		resp["section"] = map[string]any{"id": sid.ID, "name": sid.Name}
	}
	subs, err := s.store.ListMonitors(filter)
	if err != nil {
		writeErr(w, err)
		return
	}
	resp["count"] = len(subs)
	resp["monitors"] = s.monitorViews(subs)
	writeJSON(w, http.StatusOK, resp)
}

// monitorViews 补 last_status / 友好名；不含 token。
func (s *Server) monitorViews(subs []MonitorSub) []monitorView {
	out := make([]monitorView, 0, len(subs))
	if len(subs) == 0 {
		return out
	}
	statuses, _ := s.store.MonitorTokenStatuses()
	cfg := loadAppSettings()
	nameCache := map[string][2]string{}
	for _, sub := range subs {
		v := monitorView{
			ID:             sub.ID,
			SectionID:      sub.SectionID,
			SectionName:    sub.SectionName,
			TargetType:     sub.TargetType,
			TeamID:         sub.TeamID,
			SlotID:         sub.SlotID,
			ConversationID: sub.ConversationID,
			IntervalSec:    sub.IntervalSec,
			LastSeen:       sub.LastSeen,
			CreatedAt:      sub.CreatedAt,
			UpdatedAt:      sub.UpdatedAt,
		}
		if st, ok := statuses[sub.ConversationID]; ok {
			if n, err := strconv.Atoi(st[0]); err == nil {
				v.LastStatus = n
			}
			v.LastStatusAt = st[1]
		}
		if sub.TeamID != "" {
			names, ok := nameCache[sub.TeamID]
			if !ok {
				names = lookupTeamNames(cfg, sub.TeamID, s.store.MonitorToken(sub.ConversationID), sub.SlotID)
				nameCache[sub.TeamID] = names
			}
			v.TeamName, v.SlotName = names[0], names[1]
		}
		out = append(out, v)
	}
	return out
}

// lookupTeamNames 尽力查团队/成员友好名；查不到就留空，绝不因为查询失败影响接口。
func lookupTeamNames(cfg appSettings, teamID, token, slotID string) [2]string {
	var out [2]string
	if cfg.BaseURL == "" || teamID == "" {
		return out
	}
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/teams/%s", cfg.BaseURL, teamID), nil)
	if err != nil {
		return out
	}
	if token != "" {
		req.Header.Set("x-aionui-runtime-token", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	type agent struct {
		SlotID  string `json:"slot_id"`
		SlotID2 string `json:"slotId"`
		Name    string `json:"name"`
	}
	var doc struct {
		Name   string  `json:"name"`
		Agents []agent `json:"agents"`
		Data   struct {
			Name   string  `json:"name"`
			Agents []agent `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return out
	}
	out[0] = strings.TrimSpace(doc.Name)
	if out[0] == "" {
		out[0] = strings.TrimSpace(doc.Data.Name)
	}
	agents := doc.Agents
	if len(agents) == 0 {
		agents = doc.Data.Agents
	}
	if slotID != "" {
		for _, a := range agents {
			id := strings.TrimSpace(a.SlotID)
			if id == "" {
				id = strings.TrimSpace(a.SlotID2)
			}
			if id == slotID {
				out[1] = strings.TrimSpace(a.Name)
				break
			}
		}
	}
	return out
}

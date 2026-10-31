package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	UserID         string  `json:"user_id"`  // 可选：AionUI 用户 id（查友好名要三个头）
	BaseURL        string  `json:"base_url"` // 可选：该连接的 AionUI 宿主地址（Pager 注册时上报）
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
	ConvName       string `json:"conv_name"`
	DisplayName    string `json:"display_name"`
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
		BaseURL:        in.BaseURL,
		UserID:         in.UserID,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	if uid := strings.TrimSpace(in.UserID); uid != "" {
		saveAionUserID(sub.ConversationID, uid)
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
		BaseURL        string `json:"base_url"`
		UserID         string `json:"user_id"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.ConversationID) == "" {
		writeErr(w, ErrBadBody)
		return
	}
	sub, err := s.store.MonitorHeartbeat(sec.ID, in.ConversationID, in.Token, in.BaseURL, in.UserID)
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
	sub, err := s.store.MonitorByID(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.store.DeleteMonitor(id); err != nil {
		writeErr(w, err)
		return
	}
	removePagerState(sub.ConversationID)
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
	// pager 自己注销（/self）时保留本地参数文件，便于下次启动自动恢复
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
		convs, _ := s.store.MonitorConversations(&sid.ID)
		n, err := s.store.DeleteMonitorsBySection(sid.ID)
		if err != nil {
			writeErr(w, err)
			return
		}
		for _, c := range convs {
			removePagerState(c)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": n, "section": sid.ID})
		return
	}
	if v := strings.TrimSpace(q.Get("all")); v != "1" {
		// 不带 section 又不显式 all=1：拒绝，避免误清全部
		writeErr(w, ErrBadBody)
		return
	}
	convs, _ := s.store.MonitorConversations(nil)
	n, err := s.store.DeleteAllMonitors()
	if err != nil {
		writeErr(w, err)
		return
	}
	for _, c := range convs {
		removePagerState(c)
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
	cfg := loadAppSettings()

	// 先把要查的目标去重，并发查（每个自带 2s 超时），避免 20 个分区串行等待
	type nameJob struct{ key, convID, token, base, userID string }
	seen := map[string]bool{}
	jobs := []nameJob{}
	for _, sub := range subs {
		k := "conv:" + sub.ConversationID
		if sub.TargetType == "team" && sub.TeamID != "" {
			k = "team:" + sub.TeamID
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		jobs = append(jobs, nameJob{key: k, convID: sub.ConversationID, token: sub.Token, base: sub.BaseURL, userID: sub.UserID})
	}
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j nameJob) {
			defer wg.Done()
			resolveNames(cfg, j.key, j.token, j.convID, j.base, j.userID)
		}(j)
	}
	wg.Wait()

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
		v.LastStatus = sub.LastStatus
		v.LastStatusAt = sub.LastStatusAt
		if sub.TargetType == "team" && sub.TeamID != "" {
			e := getNames("team:" + sub.TeamID)
			v.TeamName = e.team
			if sub.SlotID != "" {
				v.SlotName = e.slots[sub.SlotID]
			}
			v.DisplayName = joinName(v.TeamName, v.SlotName)
		} else {
			e := getNames("conv:" + sub.ConversationID)
			v.ConvName = e.team
			v.DisplayName = v.ConvName
		}
		out = append(out, v)
	}
	return out
}

func joinName(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, " - ")
}

/* ---------------- 友好名查询（60s 内存缓存） ---------------- */

type nameInfo struct {
	team  string
	slots map[string]string
	exp   time.Time
}

var (
	nameCacheMu sync.Mutex
	nameCache   = map[string]nameInfo{}
	nameLoading = map[string]bool{}
)

const nameCacheTTL = 60 * time.Second

func getNames(key string) nameInfo {
	nameCacheMu.Lock()
	defer nameCacheMu.Unlock()
	if e, ok := nameCache[key]; ok && time.Now().Before(e.exp) {
		return e
	}
	return nameInfo{}
}

// resolveNames 查一次并写缓存；查询中（并发）或失败都不阻塞调用方，直接回退。
func resolveNames(cfg appSettings, key, token, convID, connBaseURL, rowUserID string) {
	base := strings.TrimRight(strings.TrimSpace(connBaseURL), "/")
	if base == "" || token == "" {
		return // 连接没上报地址 / 没有 token：直接回退（不再用设置页默认）
	}
	cfg.BaseURL = base
	userID := rowUserID
	if userID == "" {
		userID = loadAionUserID(convID) // 老数据回退到侧车
	}
	if userID == "" {
		return // 不知道用户 id 就查不了（aioncore 要 runtime-token + user-id + conversation-id 三个头）
	}
	nameCacheMu.Lock()
	if e, ok := nameCache[key]; ok && time.Now().Before(e.exp) {
		nameCacheMu.Unlock()
		return
	}
	if nameLoading[key] {
		nameCacheMu.Unlock()
		return // 同目标正在查，本次先用回退值
	}
	nameLoading[key] = true
	nameCacheMu.Unlock()

	info := nameInfo{slots: map[string]string{}, exp: time.Now().Add(nameCacheTTL)}
	ok := false
	if strings.HasPrefix(key, "team:") {
		info.team, info.slots, ok = fetchTeamNames(cfg, strings.TrimPrefix(key, "team:"), token, userID, convID)
	} else {
		info.team, ok = fetchConversationName(cfg, strings.TrimPrefix(key, "conv:"), token, userID, convID)
	}

	nameCacheMu.Lock()
	delete(nameLoading, key)
	if ok {
		nameCache[key] = info
	} else {
		// 失败也缓存一小段（10s），避免失败时每次刷新都打 aioncore
		info.exp = time.Now().Add(10 * time.Second)
		nameCache[key] = info
	}
	nameCacheMu.Unlock()
}

func fetchTeamNames(cfg appSettings, teamID, token, userID, convID string) (string, map[string]string, bool) {
	body, ok := aionGet(cfg, "/api/teams/"+teamID, token, userID, convID)
	if !ok {
		return "", nil, false
	}
	type assistant struct {
		SlotID  string `json:"slot_id"`
		SlotID2 string `json:"slotId"`
		Name    string `json:"name"`
	}
	var doc struct {
		Name       string      `json:"name"`
		Assistants []assistant `json:"assistants"`
		Agents     []assistant `json:"agents"`
		Data       struct {
			Name       string      `json:"name"`
			Assistants []assistant `json:"assistants"`
			Agents     []assistant `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", nil, false
	}
	name := strings.TrimSpace(doc.Data.Name)
	if name == "" {
		name = strings.TrimSpace(doc.Name)
	}
	list := doc.Data.Assistants
	if len(list) == 0 {
		list = doc.Data.Agents
	}
	if len(list) == 0 {
		list = doc.Assistants
	}
	if len(list) == 0 {
		list = doc.Agents
	}
	slots := map[string]string{}
	for _, a := range list {
		id := strings.TrimSpace(a.SlotID)
		if id == "" {
			id = strings.TrimSpace(a.SlotID2)
		}
		if id != "" {
			slots[id] = strings.TrimSpace(a.Name)
		}
	}
	if name == "" && len(slots) == 0 {
		return "", nil, false
	}
	return name, slots, true
}

func fetchConversationName(cfg appSettings, convID, token, userID, convHint string) (string, bool) {
	body, ok := aionGet(cfg, "/api/conversations/"+convID, token, userID, convHint)
	if !ok {
		return "", false
	}
	var doc struct {
		Name string `json:"name"`
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", false
	}
	name := strings.TrimSpace(doc.Data.Name)
	if name == "" {
		name = strings.TrimSpace(doc.Name)
	}
	if name == "" {
		return "", false
	}
	return name, true
}

// aionGet 查 aioncore；2 秒超时、任何失败都只返回 false（绝不阻塞或 500）。
func aionGet(cfg appSettings, path, token, userID, convID string) ([]byte, bool) {
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodGet, cfg.BaseURL+path, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("x-aionui-runtime-token", token)
	req.Header.Set("x-aionui-user-id", userID)
	if convID != "" {
		req.Header.Set("x-aionui-conversation-id", convID)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false
	}
	return body, true
}

/* ---------------- AionUI user id（查友好名要三个头） ---------------- */

// aionUsersPath 存 %LOCALAPPDATA%\AgentTodoBoard\aion_users.json（conversation_id -> user_id）。
// 只用于查团队/会话名，不进 API 响应、不进日志。
func aionUsersPath() string { return filepath.Join(appDataDir(), "aion_users.json") }

func loadAionUserID(convID string) string {
	if convID == "" {
		return ""
	}
	raw, err := os.ReadFile(aionUsersPath())
	if err != nil {
		return ""
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	return strings.TrimSpace(m[convID])
}

func saveAionUserID(convID, userID string) {
	if convID == "" || userID == "" {
		return
	}
	path := aionUsersPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	m := map[string]string{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &m)
	}
	if m[convID] == userID {
		return
	}
	m[convID] = userID
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

/* ---------------- pager 盯盘参数目录（T156） ---------------- */

// pagerStateDir 与 pager 侧同源：<exe 目录>\<FQDN>\（参数文件与 pager 日志都在这里）。
func pagerStateDir() string {
	return filepath.Join(exeDirPath(), sanitizeFileName(computerFQDN()))
}

func pagerStateFile(convID string) string {
	return filepath.Join(pagerStateDir(), sanitizeFileName(convID)+".json")
}

// ensurePagerStateDir 启动时建目录（不存在就建）。
func ensurePagerStateDir() string {
	dir := pagerStateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("创建 pager 参数目录失败: %v", err)
		return dir
	}
	log.Printf("pager 参数目录: %s", dir)
	return dir
}

// removePagerState 删掉某会话的盯盘参数文件；失败只记日志。
func removePagerState(convID string) {
	if strings.TrimSpace(convID) == "" {
		return
	}
	if err := os.Remove(pagerStateFile(convID)); err != nil && !os.IsNotExist(err) {
		log.Printf("删除 pager 参数文件失败（%s）: %v", convID, err)
	}
}
func exeDirPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

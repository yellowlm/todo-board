// todoboard-pager：把「待办看板」接进 AionUI 的常驻盯盘 MCP（stdio JSON-RPC）。
//
// 设计口径（T72）：
//   - 不写任何本地文件（无快照、无 state.json），状态全在内存；重启/团队重建后由 Agent 重新 start_monitor
//   - 日志默认关闭；仅当环境变量 TODOBOARD_DEBUGE 有值时写日志到该路径（文件或目录）
//   - 通知正文逐条列出全部新增任务标题，不限条数
//
// 环境变量（缺一即退出，stderr 写明缺哪个）：
//
//	AIONUI_BASE_URL / AIONUI_RUNTIME_TOKEN / AIONUI_USER_ID / AIONUI_CONVERSATION_ID
//
// 可选：TODO_BOARD_URL（默认 http://127.0.0.1:17321）、TODOBOARD_DEBUGE（日志路径）
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	serverName      = "todoboard-pager"
	serverVersion   = "1.0.0"
	protocolVersion = "2024-11-05"
)

var (
	boardDefaultURL = "http://127.0.0.1:17321"

	envBaseURL string
	envToken   string
	envUserID  string
	envConvID  string
	boardBase  string

	targetKind string // "team" | "conversation"
	targetTeam string
	targetSlot string

	httpClient = &http.Client{Timeout: 10 * time.Second}
	logger     *log.Logger
	logCloser  io.Closer

	stateMu sync.Mutex
	state   monitorState
)

// monitorState 全部驻留内存，进程结束即失效。
type monitorState struct {
	running   bool
	section   string
	sectionID int64
	key       string
	interval  int
	target    string
	hash      string
	prev      []task
	pending   string // 待重发的通知（鉴权/网络失败时不丢，按间隔重试）
	subID     int64  // 主程序 /api/monitors 里本会话的注册 id（0 = 未注册）

}

type task struct {
	ID          int64   `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Status      string  `json:"status"`
	DueAt       *string `json:"due_at"`
	UpdatedAt   string  `json:"updated_at"`
}

type taskListResponse struct {
	Count   int `json:"count"`
	Section struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"section"`
	Tasks []task `json:"tasks"`
}

func main() {
	setupLogging()
	loadEnvValues()

	// 启动阶段绝不因缺少环境变量退出：AionUI 的「测试连接」不注入这些变量，
	// 只要 initialize / tools/list 能应答即可。真正发消息时再检查。
	if miss := missingEnv(); len(miss) > 0 {
		dbg("未注入环境变量 %s：initialize/tools/list/list_tasks/set_status 可用，盯盘注册不可用", strings.Join(miss, ", "))
	} else if err := precheckTarget(); err != nil {
		dbg("预检会话失败（不阻塞启动，start_monitor 时会重试）：%v", err)
	}

	dbg("启动：target=%s team=%s slot=%s board=%s", targetKind, targetTeam, targetSlot, boardBase)
	setupShutdownCleanup()
	serve(os.Stdin, os.Stdout)
	shutdownUnregister() // stdin EOF / 正常结束：注销自己注册的那条
}

// shutdownUnregister 正常退出时注销本进程注册的那条（只删自己的 subID）。
// 失败只记日志，不阻塞退出、不改退出码；被强杀时不会走到这里。
func shutdownUnregister() {
	st := currentMonitor()
	if st.subID == 0 || st.key == "" {
		return
	}
	unregisterMonitor(st.subID, st.key)
	dbg("已注销盯盘注册 #%d（退出清理）", st.subID)
}

// setupShutdownCleanup 监听 SIGINT / SIGTERM：收到就先注销再退出。
func setupShutdownCleanup() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		dbg("收到退出信号，开始清理")
		shutdownUnregister()
		if logCloser != nil {
			_ = logCloser.Close()
		}
		os.Exit(0)
	}()
}

// missingEnv 返回缺失的环境变量名（空切片表示齐全）。
func missingEnv() []string {
	var missing []string
	if strings.TrimSpace(os.Getenv("AIONUI_BASE_URL")) == "" {
		missing = append(missing, "AIONUI_BASE_URL")
	}
	if strings.TrimSpace(os.Getenv("AIONUI_RUNTIME_TOKEN")) == "" {
		missing = append(missing, "AIONUI_RUNTIME_TOKEN")
	}
	if strings.TrimSpace(os.Getenv("AIONUI_USER_ID")) == "" {
		missing = append(missing, "AIONUI_USER_ID")
	}
	if strings.TrimSpace(os.Getenv("AIONUI_CONVERSATION_ID")) == "" {
		missing = append(missing, "AIONUI_CONVERSATION_ID")
	}
	return missing
}

// ensureMessaging 在注册盯盘前调用：缺 env / 目标未判定时返回工具错误。
func ensureMessaging() error {
	if miss := missingEnv(); len(miss) > 0 {
		return fmt.Errorf("缺少环境变量 %s；这些变量由 AionUI 的 agent 宿主注入，缺失时无法把通知发回会话（list_tasks / set_status 不受影响）", strings.Join(miss, ", "))
	}
	if targetKind == "" {
		if err := precheckTarget(); err != nil {
			return err
		}
	}
	return nil
}

/* ---------------- 日志（默认关闭） ---------------- */

// logFileName 与数据库同名风格：<FQDN>.pager.log（本机即 work0.yellowlm.net.pager.log）。
func logFileName() string { return sanitizeFileName(computerFQDN()) + ".pager.log" }

// resolveLogPath 解析 TODOBOARD_DEBUGE：
//   - 空 → 返回 ""（不写日志，开关语义不变）
//   - 完整文件路径（带目录且以 .log 结尾）→ 原样使用
//   - 目录 / 以分隔符结尾 / 只有文件名 → 该目录（或 exe 同目录）下的 <FQDN>.pager.log
func resolveLogPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if fi, err := os.Stat(raw); err == nil && fi.IsDir() {
		return filepath.Join(raw, logFileName())
	}
	if strings.HasSuffix(raw, "/") || strings.HasSuffix(raw, string(os.PathSeparator)) {
		return filepath.Join(raw, logFileName())
	}
	dir, base := filepath.Dir(raw), filepath.Base(raw)
	if dir != "" && dir != "." && strings.HasSuffix(strings.ToLower(base), ".log") {
		return raw // 完整文件路径：照用，不改名
	}
	return filepath.Join(exeDirPath(), logFileName())
}

func setupLogging() {
	logger = log.New(io.Discard, "", 0)
	path := resolveLogPath(os.Getenv("TODOBOARD_DEBUGE"))
	if path == "" {
		return
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	logCloser = f
	logger = log.New(f, "", log.LstdFlags)
	dbg("日志文件: %s", path)
}

func dbg(format string, args ...any) { logger.Printf(format, args...) }

func fatal(code int, msg string) {
	dbg("退出(%d)：%s", code, msg)
	fmt.Fprintf(os.Stderr, "todoboard-pager: %s\n", msg)
	if logCloser != nil {
		_ = logCloser.Close()
	}
	os.Exit(code)
}

/* ---------------- 环境与目标预检 ---------------- */

func loadEnvValues() {
	envBaseURL = strings.TrimRight(strings.TrimSpace(os.Getenv("AIONUI_BASE_URL")), "/")
	envToken = strings.TrimSpace(os.Getenv("AIONUI_RUNTIME_TOKEN"))
	envUserID = strings.TrimSpace(os.Getenv("AIONUI_USER_ID"))
	envConvID = strings.TrimSpace(os.Getenv("AIONUI_CONVERSATION_ID"))

	boardBase = strings.TrimRight(strings.TrimSpace(os.Getenv("TODO_BOARD_URL")), "/")
	if boardBase == "" {
		boardBase = boardDefaultURL
	}
}

// precheckTarget 读会话 extra：teamId 非空 → 团队端点；否则单聊端点。
// slot_id 只从会话读取，不做额外配置项。
func precheckTarget() error {
	url := fmt.Sprintf("%s/api/conversations/%s", envBaseURL, envConvID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("构造预检请求失败: %w", err)
	}
	// 预检只为拿到 team/slot（注册要用），鉴权头直接用 env；不再读 TOML（configfile.go 里为注释保留的通用配置读写）
	req.Header.Set("x-aionui-runtime-token", envToken)
	req.Header.Set("x-aionui-user-id", envUserID)
	req.Header.Set("x-aionui-conversation-id", envConvID)
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("连接 AionUI 失败（%s）：%v；请检查 AIONUI_BASE_URL 是否可达", envBaseURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("预检会话失败：HTTP %d %s", resp.StatusCode, truncate(string(body), 300))
	}

	var conv struct {
		Extra map[string]any `json:"extra"`
		Data  struct {
			Extra map[string]any `json:"extra"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &conv); err != nil {
		return fmt.Errorf("解析会话信息失败: %w", err)
	}
	extra := conv.Data.Extra
	if len(extra) == 0 {
		extra = conv.Extra
	}
	teamID := pickString(extra, "teamId", "team_id")
	slotID := pickString(extra, "slot_id", "slotId")
	if teamID != "" {
		if slotID == "" {
			return fmt.Errorf("会话属于团队 %s，但 extra 里没有 slot_id；无法确定要发给哪个成员", teamID)
		}
		targetKind, targetTeam, targetSlot = "team", teamID, slotID
		return nil
	}
	targetKind = "conversation"
	return nil
}

func pickString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

/* ---------------- 与 AionUI 通信 ---------------- */

/* ---------------- 主程序盯盘注册（T123） ---------------- */

// monitorEndpoint 调主程序的 /api/monitors 系列接口，统一带分区 key。
func monitorEndpoint(method, path, key string, body any) (int, []byte, error) {
	url := strings.TrimRight(boardBase, "/") + path
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Auth-Key", key)
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("连接待办看板失败（%s）：%v", boardBase, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// registerMonitor 在主程序登记盯盘；成功返回注册 id。
func registerMonitor(sectionID int64, key string, interval int) (int64, error) {
	body := map[string]any{
		"section_id":      sectionID,
		"target_type":     targetTypeForRegister(),
		"team_id":         targetTeam,
		"slot_id":         targetSlot,
		"conversation_id": envConvID,
		"interval_sec":    interval,
		"token":           envToken,
		"user_id":         envUserID,  // 查团队/会话友好名要用
		"base_url":        envBaseURL, // 该连接的宿主地址，主程序推送/取名/续租都用它
	}
	code, raw, err := monitorEndpoint(http.MethodPost, "/api/monitors", key, body)
	if err != nil {
		return 0, err
	}
	if code < 200 || code >= 300 {
		return 0, fmt.Errorf("主程序拒绝注册：HTTP %d %s", code, truncate(string(raw), 160))
	}
	var out struct {
		Monitor struct {
			ID int64 `json:"id"`
		} `json:"monitor"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("解析注册响应失败: %w", err)
	}
	if out.Monitor.ID == 0 {
		return 0, errors.New("注册响应里没有 id")
	}
	return out.Monitor.ID, nil
}

func targetTypeForRegister() string {
	if targetKind == "team" {
		return "team"
	}
	return "conv"
}

// heartbeatMonitor 回报存活；status 为 HTTP 状态码（404 = 盯盘已被清除）。
func heartbeatMonitor(sectionID int64, key string) (int, error) {
	body := map[string]any{"section_id": sectionID, "conversation_id": envConvID, "token": envToken, "base_url": envBaseURL, "user_id": envUserID}
	code, _, err := monitorEndpoint(http.MethodPut, "/api/monitors/heartbeat", key, body)
	if err != nil {
		return 0, err
	}
	return code, nil
}

func unregisterMonitor(subID int64, key string) {
	if subID == 0 {
		return
	}
	if code, _, err := monitorEndpoint(http.MethodDelete, fmt.Sprintf("/api/monitors/%d/self", subID), key, nil); err != nil {
		dbg("注销盯盘失败：%v", err)
	} else if code >= 300 && code != http.StatusNotFound {
		dbg("注销盯盘返回 HTTP %d", code)
	}
}

/* ---------------- 待办看板 API ---------------- */

func boardTasks(section, key, status string) (taskListResponse, []byte, error) {
	url := strings.TrimRight(boardBase, "/") + "/api/tasks"
	q := []string{}
	if strings.TrimSpace(section) != "" {
		q = append(q, "section="+queryEscape(section))
	}
	if strings.TrimSpace(status) != "" && status != "all" {
		q = append(q, "status="+queryEscape(status))
	}
	if len(q) > 0 {
		url += "?" + strings.Join(q, "&")
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return taskListResponse{}, nil, err
	}
	req.Header.Set("X-Auth-Key", key)
	resp, err := httpClient.Do(req)
	if err != nil {
		return taskListResponse{}, nil, fmt.Errorf("连接待办看板失败（%s）：%v", boardBase, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return taskListResponse{}, body, fmt.Errorf("看板返回 HTTP %d：%s", resp.StatusCode, truncate(string(body), 200))
	}
	var out taskListResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return taskListResponse{}, body, fmt.Errorf("解析看板响应失败: %w", err)
	}
	return out, body, nil
}

func boardSetStatus(taskID int64, status, key string) (task, error) {
	url := fmt.Sprintf("%s/api/tasks/%d/status", strings.TrimRight(boardBase, "/"), taskID)
	payload, _ := json.Marshal(map[string]string{"status": status})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return task{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Auth-Key", key)
	resp, err := httpClient.Do(req)
	if err != nil {
		return task{}, fmt.Errorf("连接待办看板失败（%s）：%v", boardBase, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return task{}, fmt.Errorf("看板返回 HTTP %d：%s", resp.StatusCode, truncate(string(body), 200))
	}
	var out struct {
		Task task `json:"task"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return task{}, fmt.Errorf("解析看板响应失败: %w", err)
	}
	return out.Task, nil
}

func queryEscape(s string) string {
	r := strings.NewReplacer(" ", "%20", "&", "%26", "=", "%3D", "#", "%23", "+", "%2B", "?", "%3F", "/", "%2F")
	return r.Replace(s)
}

/* ---------------- 监控 ---------------- */

func stateOf(t task) string {
	switch t.Status {
	case "doing", "done":
		return t.Status
	}
	return "open"
}

func summarize(list taskListResponse) string {
	var open, doing, done int
	for _, t := range list.Tasks {
		switch stateOf(t) {
		case "doing":
			doing++
		case "done":
			done++
		default:
			open++
		}
	}
	name := list.Section.Name
	if name == "" {
		name = "当前分区"
	}
	return fmt.Sprintf("「%s」共 %d 条（未完成 %d / 处理中 %d / 已完成 %d）", name, len(list.Tasks), open, doing, done)
}

// diff 返回新增任务（保持接口顺序）、变更标题、删除标题。
func diff(prev, cur []task) (added []task, changed []string, deleted []string) {
	seen := map[int64]task{}
	for _, t := range prev {
		seen[t.ID] = t
	}
	curIDs := map[int64]bool{}
	for _, t := range cur {
		curIDs[t.ID] = true
		old, ok := seen[t.ID]
		if !ok {
			added = append(added, t)
			continue
		}
		if old.Status != t.Status || old.Title != t.Title || old.Description != t.Description ||
			old.UpdatedAt != t.UpdatedAt || deref(old.DueAt) != deref(t.DueAt) {
			changed = append(changed, t.Title)
		}
	}
	for _, t := range prev {
		if !curIDs[t.ID] {
			deleted = append(deleted, t.Title)
		}
	}
	return added, changed, deleted
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

var monitorStop chan struct{}

// normalizeSection 允许 "4" / "#4" / "  #4 " / 分区名 等写法。
func normalizeSection(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSpace(strings.TrimPrefix(s, "#"))
	return s
}

func currentTarget() string {
	return targetKind + "|" + targetTeam + "|" + targetSlot
}

func startMonitor(section string, sectionID int64, key string, interval int) {
	stopMonitor()
	stop := make(chan struct{})
	monitorStop = stop

	// 先在主程序登记（T123）：登记成功后推送由主程序内核负责，本进程只轮询 + 心跳
	subID, regErr := registerMonitor(sectionID, key, interval)
	if regErr != nil {
		dbg("主程序登记盯盘失败（本地继续轮询、由本进程推送）：%v", regErr)
	}

	stateMu.Lock()
	state = monitorState{
		running:   true,
		section:   section,
		sectionID: sectionID,
		key:       key,
		interval:  interval,
		target:    currentTarget(),
		subID:     subID,
	}
	stateMu.Unlock()

	go func() {
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()
		// 一律用启动时解析出的分区 ID 轮询：分区改名后依然能盯
		pollSection := strconv.FormatInt(sectionID, 10)
		poll := func() {
			// 心跳（T123）：404 = 用户已在别处清除盯盘 → 彻底停止轮询，不再注册、不再推送
			if code, err := heartbeatMonitor(sectionID, key); err == nil {
				if code == http.StatusNotFound {
					dbg("主程序里盯盘已被清除，停止本地轮询")
					stopMonitor()
					return
				}
			} else {
				dbg("心跳失败（不影响轮询）：%v", err)
			}

			list, raw, err := boardTasks(pollSection, key, "")
			if err != nil {
				dbg("轮询失败: %v", err)
				return
			}
			sum := sha256.Sum256(raw)
			hash := hex.EncodeToString(sum[:])

			stateMu.Lock()
			prevHash, prevTasks := state.hash, state.prev
			stateMu.Unlock()
			if hash == prevHash {
				return
			}

			added, changed, deleted := diff(prevTasks, list.Tasks)
			stateMu.Lock()
			state.hash = hash
			state.prev = list.Tasks
			stateMu.Unlock()

			if prevHash == "" {
				// 首轮只建立基线（推送由主程序负责，这里只记日志）
				dbg("基线建立：%s", summarize(list))
				return
			}
			// 推送已由主程序负责：本进程只更新基线，不再自己发通知
			dbg("看板有变化，已更新基线（新增 %d / 变更 %d / 删除 %d）；推送由主程序负责",
				len(added), len(changed), len(deleted))
		}
		poll()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				poll()
			}
		}
	}()
}

// stopMonitor 只停本地轮询；注销主程序注册请用 stopMonitorAndUnregister。
func stopMonitor() {
	stateMu.Lock()
	if monitorStop != nil {
		close(monitorStop)
		monitorStop = nil
	}
	state = monitorState{}
	stateMu.Unlock()
}

// stopMonitorAndUnregister 先注销主程序里的注册，再停本地轮询。
func stopMonitorAndUnregister() {
	st := currentMonitor()
	if st.subID != 0 && st.key != "" {
		unregisterMonitor(st.subID, st.key)
	}
	stopMonitor()
}

func currentMonitor() monitorState {
	stateMu.Lock()
	defer stateMu.Unlock()
	return state
}

/* ---------------- MCP（stdio JSON-RPC） ---------------- */

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func serve(in io.Reader, out io.Writer) {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	enc := json.NewEncoder(out)
	write := func(resp rpcResponse) {
		resp.JSONRPC = "2.0"
		if err := enc.Encode(resp); err != nil {
			dbg("写响应失败: %v", err)
		}
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			dbg("非法 JSON-RPC: %v", err)
			write(rpcResponse{Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		if req.ID == nil {
			continue // 通知类消息无需回应
		}
		switch req.Method {
		case "initialize":
			write(rpcResponse{ID: req.ID, Result: map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
			}})
		case "ping":
			write(rpcResponse{ID: req.ID, Result: map[string]any{}})
		case "tools/list":
			write(rpcResponse{ID: req.ID, Result: map[string]any{"tools": toolDefs()}})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				write(rpcResponse{ID: req.ID, Error: &rpcError{Code: -32602, Message: "invalid params"}})
				continue
			}
			text, isErr := callTool(p.Name, p.Arguments)
			write(rpcResponse{ID: req.ID, Result: map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
				"isError": isErr,
			}})
		default:
			write(rpcResponse{ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}})
		}
	}
	dbg("stdin 结束，退出")
}

func toolDefs() []map[string]any {
	return []map[string]any{
		{
			"name":        "start_monitor",
			"description": "开始盯一个分区（非阻塞）。看板有变化时向当前会话/团队成员发送[通知]，正文逐条列出全部新增任务标题。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"section":      map[string]any{"type": "string", "description": "分区名或分区 ID"},
					"key":          map[string]any{"type": "string", "description": "该分区的认证密钥（X-Auth-Key）"},
					"interval_sec": map[string]any{"type": "integer", "description": "刷新间隔秒数；默认 60，最小 1"},
				},
				"required": []string{"section", "key"},
			},
		},
		{
			"name":        "stop_monitor",
			"description": "停止当前盯盘。",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "list_tasks",
			"description": "读取当前分区的任务列表。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status":  map[string]any{"type": "string", "description": "open | doing | done | all（默认全部）"},
					"section": map[string]any{"type": "string", "description": "分区名或 ID（默认当前盯盘的分区）"},
					"key":     map[string]any{"type": "string", "description": "分区密钥（默认沿用 start_monitor 的 key）"},
				},
			},
		},
		{
			"name":        "set_status",
			"description": "修改任务状态：open（未完成）/ doing（处理中）/ done（已完成）。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{"type": "integer", "description": "任务 ID"},
					"status":  map[string]any{"type": "string", "description": "open | doing | done"},
					"key":     map[string]any{"type": "string", "description": "分区密钥（默认沿用 start_monitor 的 key）"},
				},
				"required": []string{"task_id", "status"},
			},
		},
	}
}

func callTool(name string, raw json.RawMessage) (string, bool) {
	switch name {
	case "start_monitor":
		var in struct {
			Section  string `json:"section"`
			Key      string `json:"key"`
			Interval *int   `json:"interval_sec"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return "参数解析失败: " + err.Error(), true
		}
		in.Section = normalizeSection(in.Section)
		in.Key = strings.TrimSpace(in.Key)
		if in.Section == "" || in.Key == "" {
			return "section 与 key 均必填（分区名或 ID、该分区密钥）", true
		}
		interval := 60
		if in.Interval != nil {
			if *in.Interval < 1 {
				return "interval_sec 必须 ≥ 1（秒）；不传则默认 60", true
			}
			interval = *in.Interval
		}
		if err := ensureMessaging(); err != nil {
			return err.Error(), true
		}
		list, _, err := boardTasks(in.Section, in.Key, "")
		if err != nil {
			return "启动失败（首次读取看板出错）：" + err.Error(), true
		}
		cur := currentMonitor()
		if cur.running && cur.sectionID == list.Section.ID && cur.interval == interval &&
			cur.key == in.Key && cur.target == currentTarget() {
			dbg("监控已在运行，保留基线（分区 #%d，间隔 %d）", cur.sectionID, cur.interval)
			return fmt.Sprintf("监控已在运行（分区「%s」#%d，间隔 %d 秒），保留原有基线、未重建。",
				list.Section.Name, list.Section.ID, cur.interval), false
		}
		if cur.running {
			dbg("因参数变更重建基线（分区 #%d 间隔 %d → 分区 #%d 间隔 %d）", cur.sectionID, cur.interval, list.Section.ID, interval)
		} else {
			dbg("建立基线（分区 #%d，间隔 %d 秒）", list.Section.ID, interval)
		}
		startMonitor(in.Section, list.Section.ID, in.Key, interval)
		return fmt.Sprintf("已开始盯盘 %s，间隔 %d 秒；后续有变化会向本会话发送[通知]。", summarize(list), interval), false

	case "stop_monitor":
		stopMonitorAndUnregister()
		return "已停止盯盘，并已清除主程序里的注册。", false

	case "list_tasks":
		var in struct {
			Status  string `json:"status"`
			Section string `json:"section"`
			Key     string `json:"key"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return "参数解析失败: " + err.Error(), true
		}
		st := currentMonitor()
		key := strings.TrimSpace(in.Key)
		if key == "" {
			key = st.key
		}
		if key == "" {
			return "缺少分区密钥：请先 start_monitor 提供 key，或在参数里传 key。", true
		}
		section := normalizeSection(in.Section)
		if section == "" {
			section = strconv.FormatInt(st.sectionID, 10)
			if st.sectionID == 0 {
				section = st.section
			}
		}
		list, _, err := boardTasks(section, key, in.Status)
		if err != nil {
			return "读取任务失败：" + err.Error(), true
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s\n", summarize(list))
		if len(list.Tasks) == 0 {
			b.WriteString("（没有任务）")
			return b.String(), false
		}
		for _, t := range list.Tasks {
			due := "未设截止"
			if t.DueAt != nil && *t.DueAt != "" {
				due = strings.Replace(*t.DueAt, "T", " ", 1)
			}
			fmt.Fprintf(&b, "- #%d [%s] %s · %s\n", t.ID, stateOf(t), t.Title, due)
		}
		return strings.TrimRight(b.String(), "\n"), false

	case "set_status":
		var in struct {
			TaskID int64  `json:"task_id"`
			Status string `json:"status"`
			Key    string `json:"key"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return "参数解析失败: " + err.Error(), true
		}
		switch in.Status {
		case "open", "doing", "done":
		default:
			return "status 仅支持 open / doing / done", true
		}
		if in.TaskID <= 0 {
			return "task_id 必须为正整数", true
		}
		st := currentMonitor()
		key := strings.TrimSpace(in.Key)
		if key == "" {
			key = st.key
		}
		if key == "" {
			return "缺少分区密钥：请先 start_monitor 提供 key，或在参数里传 key。", true
		}
		t, err := boardSetStatus(in.TaskID, in.Status, key)
		if err != nil {
			return "修改状态失败：" + err.Error(), true
		}
		return fmt.Sprintf("已把 #%d「%s」改为 %s。", t.ID, t.Title, stateOf(t)), false
	}
	return "未知工具: " + name, true
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

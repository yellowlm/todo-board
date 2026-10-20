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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
		dbg("未注入环境变量 %s：initialize/tools/list/list_tasks/set_status 可用，盯盘与发消息不可用", strings.Join(miss, ", "))
	} else if err := precheckTarget(); err != nil {
		dbg("预检会话失败（不阻塞启动，发消息时会重试）：%v", err)
	} else {
		startLeaseLoop()
	}

	dbg("启动：target=%s team=%s slot=%s board=%s", targetKind, targetTeam, targetSlot, boardBase)
	serve(os.Stdin, os.Stdout)
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

// ensureMessaging 在真正要发消息前调用：缺 env / 目标未判定时返回工具错误。
func ensureMessaging() error {
	if miss := missingEnv(); len(miss) > 0 {
		return fmt.Errorf("缺少环境变量 %s；这些变量由 AionUI 的 agent 宿主注入，缺失时无法把通知发回会话（list_tasks / set_status 不受影响）", strings.Join(miss, ", "))
	}
	if targetKind == "" {
		if err := precheckTarget(); err != nil {
			return err
		}
		startLeaseLoop()
	}
	return nil
}

/* ---------------- 日志（默认关闭） ---------------- */

func setupLogging() {
	logger = log.New(io.Discard, "", 0)
	path := strings.TrimSpace(os.Getenv("TODOBOARD_DEBUGE"))
	if path == "" {
		return
	}
	// 值可能是文件或目录：已存在的目录 → 里面建固定文件名
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		path = filepath.Join(path, "todoboard-pager.log")
	} else if strings.HasSuffix(path, string(os.PathSeparator)) || strings.HasSuffix(path, "/") {
		_ = os.MkdirAll(path, 0o755)
		path = filepath.Join(path, "todoboard-pager.log")
	} else if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	logCloser = f
	logger = log.New(f, "", log.LstdFlags)
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

func aionHeaders(req *http.Request) {
	req.Header.Set("x-aionui-runtime-token", envToken)
	req.Header.Set("x-aionui-user-id", envUserID)
	req.Header.Set("x-aionui-conversation-id", envConvID)
}

// precheckTarget 读会话 extra：teamId 非空 → 团队端点；否则单聊端点。
// slot_id 只从会话读取，不做额外配置项。
func precheckTarget() error {
	url := fmt.Sprintf("%s/api/conversations/%s", envBaseURL, envConvID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("构造预检请求失败: %w", err)
	}
	aionHeaders(req)
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

func targetURL() (string, error) {
	if targetKind == "team" {
		if targetTeam == "" || targetSlot == "" {
			return "", errors.New("团队目标缺少 teamId 或 slot_id")
		}
		return fmt.Sprintf("%s/api/teams/%s/agents/%s/messages", envBaseURL, targetTeam, targetSlot), nil
	}
	return fmt.Sprintf("%s/api/conversations/%s/messages", envBaseURL, envConvID), nil
}

func sendMessage(content string) error {
	if err := ensureMessaging(); err != nil {
		return err
	}
	url, err := targetURL()
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"content": content})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	aionHeaders(req)
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("发送消息失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	// 202 = 排队开新轮；200 = 投进正在跑的一轮；都算成功
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("发送消息失败：HTTP %d %s", resp.StatusCode, truncate(string(body), 200))
	}
	return nil
}

var leaseOnce sync.Once

func startLeaseLoop() {
	leaseOnce.Do(func() { go leaseLoop() })
}

func leaseLoop() {
	{
		lease := func() {
			var url string
			if targetKind == "team" {
				url = fmt.Sprintf("%s/api/teams/%s/active-lease", envBaseURL, targetTeam)
			} else {
				url = fmt.Sprintf("%s/api/conversations/%s/active-lease", envBaseURL, envConvID)
			}
			req, err := http.NewRequest(http.MethodPost, url, nil)
			if err != nil {
				return
			}
			aionHeaders(req)
			resp, err := httpClient.Do(req)
			if err != nil {
				dbg("续租失败: %v", err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				dbg("续租失败：HTTP %d", resp.StatusCode)
			}
		}
		lease()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			lease()
		}
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

func buildNotice(list taskListResponse, added []task, changed, deleted []string) string {
	// 三者都空但 hash 变了（例如只改了分区名/排序、或描述未触发 updated_at）也要发一条
	if len(added) == 0 && len(changed) == 0 && len(deleted) == 0 {
		return fmt.Sprintf("[通知] 看板有变化：共 %d 条（内容有更新）", len(list.Tasks))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[通知] 看板有变化：共 %d 条，新增 %d 条", len(list.Tasks), len(added))
	if len(added) > 0 {
		b.WriteString("\n")
		for _, t := range added {
			fmt.Fprintf(&b, "\n- %s", t.Title)
		}
	}
	if len(deleted) > 0 {
		fmt.Fprintf(&b, "\n\n删除 %d 条：", len(deleted))
		for _, title := range deleted {
			fmt.Fprintf(&b, "\n- [删除] %s", title)
		}
	}
	if len(changed) > 0 {
		fmt.Fprintf(&b, "\n\n另有 %d 条变更：", len(changed))
		for _, title := range changed {
			fmt.Fprintf(&b, "\n- %s", title)
		}
	}
	return b.String()
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

	stateMu.Lock()
	state = monitorState{
		running:   true,
		section:   section,
		sectionID: sectionID,
		key:       key,
		interval:  interval,
		target:    currentTarget(),
	}
	stateMu.Unlock()

	go func() {
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()
		// 一律用启动时解析出的分区 ID 轮询：分区改名后依然能盯
		pollSection := strconv.FormatInt(sectionID, 10)
		poll := func() {
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
				// 首轮只建立基线，不发通知
				dbg("基线建立：%s", summarize(list))
				return
			}
			msg := buildNotice(list, added, changed, deleted)
			if err := sendMessage(msg); err != nil {
				dbg("发通知失败: %v", err)
			} else {
				dbg("已发通知（新增 %d / 变更 %d / 删除 %d）", len(added), len(changed), len(deleted))
			}
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

func stopMonitor() {
	stateMu.Lock()
	if monitorStop != nil {
		close(monitorStop)
		monitorStop = nil
	}
	state = monitorState{}
	stateMu.Unlock()
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
		stopMonitor()
		return "已停止盯盘。", false

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

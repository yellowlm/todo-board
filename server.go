package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

const Version = "1.0.0"

//go:embed frontend
var frontendFS embed.FS

var (
	ErrBadBody     = errors.New("请求内容格式无效")
	ErrDesktopOnly = errors.New("该功能仅在桌面模式下可用")
	ErrNoAuthKey   = errors.New("缺少认证密钥，请在请求头携带 X-Auth-Key")
	ErrBadAuthKey  = errors.New("认证密钥无效")
	ErrInternal    = errors.New("服务器内部错误")
)

type counts struct {
	Total   int `json:"total"`
	Open    int `json:"open"`
	Doing   int `json:"doing"`
	Done    int `json:"done"`
	Overdue int `json:"overdue"`
}

type boardSection struct {
	Section
	counts
	Tasks []Task `json:"tasks"`
}

type boardPayload struct {
	Date     string         `json:"date"`
	Totals   counts         `json:"totals"`
	Sections []boardSection `json:"sections"`
}

type nullableString struct {
	Set   bool
	Value *string
}

func (n *nullableString) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(bytes.TrimSpace(b)) == "null" {
		n.Value = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	n.Value = &s
	return nil
}

type taskInput struct {
	Section      *string        `json:"section"`
	SectionID    *int64         `json:"section_id"`
	Title        *string        `json:"title"`
	Description  *string        `json:"description"`
	Due          nullableString `json:"due"`
	BarkOverride *string        `json:"bark_override"`
}

type doneInput struct {
	Done *bool `json:"done"`
}

type sectionInput struct {
	Name        *string `json:"name"`
	Key         *string `json:"key"`
	BarkEnabled *bool   `json:"bark_enabled"`
}

type hub struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

func newHub() *hub {
	return &hub{subs: map[chan string]struct{}{}}
}

func (h *hub) subscribe() chan string {
	ch := make(chan string, 8)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan string) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *hub) broadcast(kind string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- kind:
		default:
		}
	}
}

type Server struct {
	store     *Store
	dbPath    string
	hub       *hub
	httpSrv   *http.Server
	ln        net.Listener
	baseURL   string
	port      int
	handler   http.Handler
	binds     []string
	listening []string
	bounds    []boundListener

	OnHide          func()
	OnDrag          func()
	OnReady         func()
	AutostartStatus func() bool
	SetAutostart    func(bool) error

	OnOpenEditor  func(sectionID, taskID int64)
	OnEditorReady func(title string)
	OnHideEditor  func()
	OnDragEditor  func()
	OnResize      func(edge string, editor bool)
	OnResizeEnd   func()
	OnClose       func()

	readyOnce sync.Once
}

func NewServer(store *Store, dbPath string) *Server {
	return &Server{store: store, dbPath: dbPath, hub: newHub()}
}

// Start 在多个地址上监听（共用同一 handler）。返回逐条失败原因；全部失败才算错误。
func (s *Server) Start(port int, binds []string) (map[string]string, error) {
	static, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		return nil, fmt.Errorf("加载前端资源失败: %w", err)
	}
	mux := http.NewServeMux()
	s.routes(mux, static)
	s.handler = mux
	s.port = port
	return s.rebind(binds)
}

type boundListener struct {
	addr string
	ln   net.Listener
	srv  *http.Server
}

func (b boundListener) close() {
	_ = b.ln.Close()
}

// rebind 先开新增地址、全部成功后才关闭被移除的旧地址；任一新增失败则一条旧监听都不动。
func (s *Server) rebind(binds []string) (map[string]string, error) {
	binds = normalizeBinds(binds)
	if len(binds) == 0 {
		binds = []string{defaultBind}
	}

	current := map[string]boundListener{}
	for _, b := range s.bounds {
		current[b.addr] = b
	}
	want := map[string]bool{}
	for _, b := range binds {
		want[b] = true
	}

	// 1) 被移除、且与新通配地址抢同一端口的旧监听必须先关（0.0.0.0 vs 具体 IPv4、:: vs 具体 IPv6）
	toAdd := []string{}
	for _, b := range binds {
		if _, ok := current[b]; !ok {
			toAdd = append(toAdd, b)
		}
	}
	closedForConflict := []boundListener{}
	for _, b := range toAdd {
		for addr, bl := range current {
			if want[addr] || !addrConflicts(b, addr) {
				continue
			}
			bl.close()
			closedForConflict = append(closedForConflict, bl)
			delete(current, addr)
			log.Printf("为绑定 %s 先停止监听: %s", b, addr)
		}
	}

	// 2) 打开新集合里「当前没有」的地址
	added := map[string]boundListener{}
	failures := map[string]string{}
	for _, b := range toAdd {
		if _, ok := current[b]; ok {
			continue
		}
		bl, err := s.listenOne(b)
		if err != nil {
			failures[b] = err.Error()
			log.Printf("监听 %s 失败: %v", net.JoinHostPort(b, strconv.Itoa(s.port)), err)
			continue
		}
		added[b] = bl
	}

	// 3) 有任一失败：关闭本次新开的，尽力恢复刚为冲突关掉的旧监听
	if len(failures) > 0 {
		for _, bl := range added {
			bl.close()
		}
		restored := 0
		for _, bl := range closedForConflict {
			if nb, err := s.listenOne(bl.addr); err == nil {
				current[bl.addr] = nb
				restored++
			}
		}
		log.Printf("新配置失败，已恢复 %d/%d 个旧监听", restored, len(closedForConflict))
		return failures, fmt.Errorf("新配置里有 %d 个地址监听失败，已保持原有监听不变", len(failures))
	}

	// 4) 全部新地址就绪后，才关闭旧集合里被移除的监听
	for addr, bl := range current {
		if !want[addr] {
			bl.close()
			log.Printf("已停止监听: %s", addr)
		}
	}

	// 5) 组装新状态（保留项复用旧监听器）
	next := make([]boundListener, 0, len(binds))
	for _, b := range binds {
		if bl, ok := current[b]; ok {
			next = append(next, bl)
			continue
		}
		next = append(next, added[b])
	}
	s.applyBounds(next)
	return failures, nil
}

// listenOne 打开一个监听器并起 Serve。
// 语义：0.0.0.0 → 仅 IPv4（tcp4）；:: → 仅 IPv6（tcp6）；域名 → 先解析，绑到解析出的 IP。
func (s *Server) listenOne(bind string) (boundListener, error) {
	host := bind
	network := "tcp"
	if ip := net.ParseIP(bind); ip != nil {
		if ip.To4() != nil {
			network = "tcp4"
		} else {
			network = "tcp6"
		}
	} else {
		ips, err := net.LookupIP(bind)
		if err != nil {
			return boundListener{}, fmt.Errorf("解析域名 %s 失败: %w", bind, err)
		}
		if len(ips) == 0 {
			return boundListener{}, fmt.Errorf("解析域名 %s 没有得到任何地址", bind)
		}
		ip := ips[0]
		host = ip.String()
		if ip.To4() != nil {
			network = "tcp4"
		} else {
			network = "tcp6"
		}
		log.Printf("域名 %s 解析为 %s", bind, host)
	}
	addr := net.JoinHostPort(host, strconv.Itoa(s.port))
	ln, err := net.Listen(network, addr)
	if err != nil {
		return boundListener{}, err
	}
	srv := &http.Server{Handler: s.handler, ReadHeaderTimeout: 5 * time.Second}
	go func(srv *http.Server, ln net.Listener) {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) &&
			!strings.Contains(err.Error(), "use of closed network connection") {
			log.Printf("HTTP 服务异常退出: %v", err)
		}
	}(srv, ln)
	log.Printf("监听成功: %s（%s）", bind, ln.Addr().String())
	return boundListener{addr: bind, ln: ln, srv: srv}, nil
}

func (s *Server) applyBounds(bounds []boundListener) {
	s.bounds = bounds
	s.listening = make([]string, 0, len(bounds))
	for _, b := range bounds {
		s.listening = append(s.listening, b.addr)
	}
	// 配置与生效集合保持一致（失败已在上面提前返回）
	s.binds = append([]string{}, s.listening...)
	s.baseURL = shellURL(s.listening, s.port)
	if len(bounds) > 0 {
		s.httpSrv = bounds[0].srv
		s.ln = bounds[0].ln
	}
	if HasNonLoopback(s.listening) {
		log.Printf("注意：已在非环回地址上监听（%s），网页端无鉴权，同网段任何人可读写", strings.Join(s.listening, "、"))
	}
}

func (s *Server) closeListeners() {
	for _, b := range s.bounds {
		b.close()
	}
	s.bounds = nil
}

// shellURL 给桌面壳用的本机可达地址：优先环回项，通配地址映射为环回。
func shellURL(listening []string, port int) string {
	pick := ""
	for _, b := range listening {
		if isLoopbackBind(b) {
			pick = b
			break
		}
	}
	if pick == "" && len(listening) > 0 {
		pick = listening[0]
		switch pick {
		case "0.0.0.0":
			pick = "127.0.0.1"
		case "::":
			pick = "::1"
		}
	}
	if pick == "" {
		return ""
	}
	return "http://" + net.JoinHostPort(pick, strconv.Itoa(port))
}

// HasNonLoopback 判断是否存在对外可达的监听地址。
func HasNonLoopback(listening []string) bool {
	for _, b := range listening {
		if !isLoopbackBind(b) {
			return true
		}
	}
	return false
}

func (s *Server) BindConfig() []string { return append([]string{}, s.binds...) }
func (s *Server) Listening() []string  { return append([]string{}, s.listening...) }
func (s *Server) Port() int            { return s.port }
func (s *Server) Rebind(binds []string) (map[string]string, error) {
	return s.rebind(binds)
}
func (s *Server) BaseURL() string { return s.baseURL }

func (s *Server) Stop() {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, b := range s.bounds {
		if err := b.srv.Shutdown(ctx); err != nil {
			log.Printf("关闭 HTTP 服务: %v", err)
		}
	}
	s.closeListeners()
}

func (s *Server) notify(kind string) { s.hub.broadcast(kind) }

func (s *Server) routes(mux *http.ServeMux, static fs.FS) {
	mux.HandleFunc("GET /", s.handleStatic(static))

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/board", s.handleBoard)

	mux.HandleFunc("POST /api/board/sections/{sid}/tasks", s.handleWebTaskCreate)
	mux.HandleFunc("PUT /api/board/sections/{sid}/tasks/{tid}", s.handleWebTaskUpdate)
	mux.HandleFunc("DELETE /api/board/sections/{sid}/tasks/{tid}", s.handleWebTaskDelete)
	mux.HandleFunc("POST /api/board/sections/{sid}/tasks/{tid}/done", s.handleWebTaskDone)
	mux.HandleFunc("POST /api/board/sections/{sid}/tasks/{tid}/status", s.handleWebTaskStatus)

	mux.HandleFunc("GET /api/sections", s.handleSectionList)
	mux.HandleFunc("POST /api/sections", s.handleSectionCreate)
	mux.HandleFunc("PUT /api/sections/{id}", s.handleSectionRename)
	mux.HandleFunc("DELETE /api/sections/{id}", s.handleSectionDelete)
	mux.HandleFunc("GET /api/sections/{id}/key", s.handleSectionKeyGet)
	mux.HandleFunc("PUT /api/sections/{id}/key", s.handleSectionKeyPut)

	mux.HandleFunc("PUT /api/sections/order", s.handleSectionsOrder)
	mux.HandleFunc("GET /api/web/sections", s.handleWebSections)
	mux.HandleFunc("GET /api/web/tasks", s.handleWebTasks)
	mux.HandleFunc("GET /api/web/stats", s.handleWebStats)
	mux.HandleFunc("GET /api/bark", s.handleBarkGet)
	mux.HandleFunc("PUT /api/bark", s.handleBarkPut)
	mux.HandleFunc("POST /api/bark/test", s.handleBarkTest)

	mux.HandleFunc("GET /api/settings", s.handleSettings)
	mux.HandleFunc("POST /api/settings/autostart", s.handleAutostart)
	mux.HandleFunc("PUT /api/settings/bind", s.handleBindPut)
	mux.HandleFunc("POST /api/window/close", s.handleWindowClose)
	mux.HandleFunc("POST /api/window/hide", s.handleWindowHide)
	mux.HandleFunc("POST /api/window/drag", s.handleWindowDrag)
	mux.HandleFunc("POST /api/window/resize/start", s.handleWindowResize)
	mux.HandleFunc("POST /api/window/resize/end", s.handleWindowResizeEnd)

	mux.HandleFunc("POST /api/editor/open", s.handleEditorOpen)

	mux.HandleFunc("POST /api/editor/ready", s.handleEditorReady)
	mux.HandleFunc("GET /api/editor/context", s.handleEditorContext)

	mux.HandleFunc("GET /api/section", s.authExternal(s.handleExternalSection))
	mux.HandleFunc("GET /api/tasks", s.authExternal(s.handleExternalTaskList))
	mux.HandleFunc("POST /api/tasks", s.authExternal(s.handleExternalTaskCreate))
	mux.HandleFunc("PUT /api/tasks/{id}", s.authExternal(s.handleExternalTaskUpdate))
	mux.HandleFunc("POST /api/tasks/{id}/done", s.authExternal(s.handleExternalTaskDone))
	mux.HandleFunc("POST /api/tasks/{id}/status", s.authExternal(s.handleExternalTaskStatus))
	mux.HandleFunc("DELETE /api/tasks/{id}", s.authExternal(s.handleExternalTaskDelete))
}

func (s *Server) handleStatic(fsys fs.FS) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		// 根路径给浏览器版；桌面壳走 /app（相对资源仍解析到根目录）
		if clean == "" || clean == "." {
			clean = "web.html"
		}
		if clean == "app" {
			clean = "index.html"
		}
		if strings.HasPrefix(clean, "api/") {
			http.NotFound(w, r)
			return
		}
		data, err := fs.ReadFile(fsys, clean)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if ct := mimeFor(clean); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, clean, time.Time{}, bytes.NewReader(data))
	}
}

func mimeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".json"):
		return "application/json; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".ico"):
		return "image/x-icon"
	case strings.HasSuffix(name, ".woff2"):
		return "font/woff2"
	}
	return ""
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if v == nil {
		w.WriteHeader(code)
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("序列化响应失败: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(code)
	w.Write(b)
}

func statusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrNoAuthKey), errors.Is(err, ErrBadAuthKey):
		return http.StatusUnauthorized
	case errors.Is(err, ErrEmptyName), errors.Is(err, ErrNoTitle), errors.Is(err, ErrBadDue),
		errors.Is(err, ErrBadKey), errors.Is(err, ErrDupName), errors.Is(err, ErrDupKey),
		errors.Is(err, ErrBadDone), errors.Is(err, ErrBadStatus), errors.Is(err, ErrBadSection), errors.Is(err, ErrNoSection),
		errors.Is(err, ErrBadBody), errors.Is(err, ErrDesktopOnly):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func writeErr(w http.ResponseWriter, err error) {
	code := statusFor(err)
	if code == http.StatusInternalServerError {
		log.Printf("请求处理失败: %v", err)
		err = ErrInternal
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		writeErr(w, ErrBadBody)
		return false
	}
	return true
}

func parseID(w http.ResponseWriter, raw string, missing error) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, missing)
		return 0, false
	}
	return id, true
}

func parseDone(raw string) (*bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "all":
		return nil, nil
	case "true", "1", "yes", "done":
		v := true
		return &v, nil
	case "false", "0", "no", "open", "todo":
		v := false
		return &v, nil
	}
	return nil, ErrBadDone
}

func parseStatus(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "", "all":
		return "", nil
	case "open", "doing", "done":
		return s, nil
	}
	return "", ErrBadStatus
}

func tally(tasks []Task, now string) counts {
	var c counts
	for _, t := range tasks {
		c.Total++
		switch t.Status {
		case "done":
			c.Done++
			continue
		case "doing":
			c.Doing++
		default:
			c.Open++
		}
		if t.DueAt != nil && *t.DueAt < now {
			c.Overdue++
		}
	}
	return c
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": Version})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, errors.New("当前服务不支持流式推送"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 2000\n\n")
	flusher.Flush()

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case kind := <-ch:
			fmt.Fprintf(w, "data: {\"type\":%q}\n\n", kind)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	s.readyOnce.Do(func() {
		if s.OnReady != nil {
			s.OnReady()
		}
	})
	sections, err := s.store.ListSections()
	if err != nil {
		writeErr(w, err)
		return
	}
	tasks, err := s.store.ListTasks(nil, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	bySection := map[int64][]Task{}
	for _, t := range tasks {
		bySection[t.SectionID] = append(bySection[t.SectionID], t)
	}
	now := time.Now().Format(timeLayout)
	payload := boardPayload{
		Date:     time.Now().Format("2006-01-02"),
		Sections: []boardSection{},
	}
	for _, sec := range sections {
		bs := boardSection{Section: sec, Tasks: []Task{}}
		group := bySection[sec.ID]
		bs.counts = tally(group, now)
		bs.Tasks = append(bs.Tasks, group...)
		payload.Totals.Total += bs.Total
		payload.Totals.Open += bs.Open
		payload.Totals.Doing += bs.Doing
		payload.Totals.Done += bs.Done
		payload.Totals.Overdue += bs.Overdue
		payload.Sections = append(payload.Sections, bs)
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleWebTaskCreate(w http.ResponseWriter, r *http.Request) {
	sectionID, ok := parseID(w, r.PathValue("sid"), ErrNoSection)
	if !ok {
		return
	}
	var in taskInput
	if !decodeBody(w, r, &in) {
		return
	}
	title := ""
	if in.Title != nil {
		title = *in.Title
	}
	description := ""
	if in.Description != nil {
		description = *in.Description
	}
	var due *string
	if in.Due.Set {
		due = in.Due.Value
	}
	task, err := s.store.CreateTask(sectionID, title, description, due)
	if err != nil {
		writeErr(w, err)
		return
	}
	if in.BarkOverride != nil && strings.TrimSpace(*in.BarkOverride) != "" && *in.BarkOverride != "inherit" {
		if updated, err := s.store.SetBarkOverride(sectionID, task.ID, *in.BarkOverride); err == nil {
			task = updated
		} else {
			log.Printf("设置任务推送覆盖失败: %v", err)
		}
	}
	s.notify("board")
	writeJSON(w, http.StatusCreated, map[string]any{"task": task})
}

func (s *Server) handleWebTaskUpdate(w http.ResponseWriter, r *http.Request) {
	sectionID, ok := parseID(w, r.PathValue("sid"), ErrNoSection)
	if !ok {
		return
	}
	taskID, ok := parseID(w, r.PathValue("tid"), ErrNotFound)
	if !ok {
		return
	}
	var in taskInput
	if !decodeBody(w, r, &in) {
		return
	}
	patch := TaskPatch{Title: in.Title, Description: in.Description, BarkOverride: in.BarkOverride}
	if in.Due.Set {
		patch.Due = duePatch{Set: true, Value: in.Due.Value}
	}
	task, err := s.store.UpdateTask(sectionID, taskID, patch)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.notify("board")
	writeJSON(w, http.StatusOK, map[string]any{"task": task})
}

func (s *Server) handleWebTaskDelete(w http.ResponseWriter, r *http.Request) {
	sectionID, ok := parseID(w, r.PathValue("sid"), ErrNoSection)
	if !ok {
		return
	}
	taskID, ok := parseID(w, r.PathValue("tid"), ErrNotFound)
	if !ok {
		return
	}
	if err := s.store.DeleteTask(sectionID, taskID); err != nil {
		writeErr(w, err)
		return
	}
	s.notify("board")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleWebTaskDone(w http.ResponseWriter, r *http.Request) {
	sectionID, ok := parseID(w, r.PathValue("sid"), ErrNoSection)
	if !ok {
		return
	}
	taskID, ok := parseID(w, r.PathValue("tid"), ErrNotFound)
	if !ok {
		return
	}
	var in doneInput
	if !decodeBody(w, r, &in) {
		return
	}
	prev, err := s.store.TaskByID(sectionID, taskID)
	if err != nil {
		writeErr(w, err)
		return
	}
	task, err := s.store.SetDone(sectionID, taskID, in.Done)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.notify("board")
	resp := map[string]any{"task": task}
	if task.Status == "done" && prev.Status != "done" {
		if sec, err := s.store.SectionByID(sectionID); err == nil {
			resp["bark"] = s.pushBark(sec, task)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleWebTaskStatus(w http.ResponseWriter, r *http.Request) {
	sectionID, ok := parseID(w, r.PathValue("sid"), ErrNoSection)
	if !ok {
		return
	}
	taskID, ok := parseID(w, r.PathValue("tid"), ErrNotFound)
	if !ok {
		return
	}
	var in struct {
		Status *string `json:"status"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if in.Status == nil {
		writeErr(w, ErrBadStatus)
		return
	}
	prev, err := s.store.TaskByID(sectionID, taskID)
	if err != nil {
		writeErr(w, err)
		return
	}
	task, err := s.store.SetStatus(sectionID, taskID, *in.Status)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.notify("board")
	resp := map[string]any{"task": task}
	if task.Status == "done" && prev.Status != "done" {
		if sec, err := s.store.SectionByID(sectionID); err == nil {
			resp["bark"] = s.pushBark(sec, task)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSectionList(w http.ResponseWriter, r *http.Request) {
	sections, err := s.store.ListSections()
	if err != nil {
		writeErr(w, err)
		return
	}
	now := time.Now().Format(timeLayout)
	bySection, err := s.store.CountsBySection(now)
	if err != nil {
		writeErr(w, err)
		return
	}
	type sectionWithCounts struct {
		Section
		counts
	}
	out := make([]sectionWithCounts, 0, len(sections))
	for _, sec := range sections {
		c := bySection[sec.ID]
		out = append(out, sectionWithCounts{Section: sec, counts: counts{
			Total: c.Total, Open: c.Open, Doing: c.Doing, Done: c.Done, Overdue: c.Overdue,
		}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sections": out, "count": len(out)})
}

func (s *Server) handleSectionDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"), ErrNoSection)
	if !ok {
		return
	}
	deleted, err := s.store.DeleteSection(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.notify("sections")
	s.notify("board")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "deleted_tasks": deleted})
}

func (s *Server) handleSectionCreate(w http.ResponseWriter, r *http.Request) {
	var in sectionInput
	if !decodeBody(w, r, &in) {
		return
	}
	name := ""
	if in.Name != nil {
		name = *in.Name
	}
	key := ""
	if in.Key != nil {
		key = *in.Key
	}
	sec, fullKey, err := s.store.CreateSection(name, key)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.notify("sections")
	writeJSON(w, http.StatusCreated, map[string]any{"section": sec, "auth_key": fullKey})
}

func (s *Server) handleSectionRename(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"), ErrNoSection)
	if !ok {
		return
	}
	var in sectionInput
	if !decodeBody(w, r, &in) {
		return
	}
	if in.Name == nil && in.BarkEnabled == nil {
		writeErr(w, ErrBadBody)
		return
	}
	sec, err := s.store.SectionByID(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if in.Name != nil {
		sec, err = s.store.RenameSection(id, *in.Name)
		if err != nil {
			writeErr(w, err)
			return
		}
	}
	if in.BarkEnabled != nil {
		sec, err = s.store.SetSectionBark(id, *in.BarkEnabled)
		if err != nil {
			writeErr(w, err)
			return
		}
	}
	s.notify("sections")
	writeJSON(w, http.StatusOK, map[string]any{"section": sec})
}

func (s *Server) handleSectionKeyGet(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"), ErrNoSection)
	if !ok {
		return
	}
	key, err := s.store.SectionKey(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "auth_key": key})
}

func (s *Server) handleSectionKeyPut(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"), ErrNoSection)
	if !ok {
		return
	}
	var in sectionInput
	if !decodeBody(w, r, &in) {
		return
	}
	key := ""
	if in.Key != nil {
		key = *in.Key
	}
	sec, fullKey, err := s.store.SetSectionKey(id, key)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.notify("sections")
	writeJSON(w, http.StatusOK, map[string]any{"section": sec, "auth_key": fullKey})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	autostart := false
	if s.AutostartStatus != nil {
		autostart = s.AutostartStatus()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":      Version,
		"api_base":     s.BaseURL(),
		"db_path":      s.dbPath,
		"autostart":    autostart,
		"port":         s.Port(),
		"bind":         s.BindConfig(),
		"listening":    s.Listening(),
		"lan_open":     HasNonLoopback(s.Listening()),
		"default_bind": defaultBind,
	})
}

func (s *Server) handleBindPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Binds []string `json:"binds"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	binds := normalizeBinds(in.Binds)
	if len(binds) == 0 {
		binds = []string{defaultBind}
	}
	failures, err := s.Rebind(binds)
	if err == nil {
		if saveErr := saveBinds(s.BindConfig()); saveErr != nil {
			log.Printf("保存 listen.json 失败: %v", saveErr)
		}
	}
	resp := map[string]any{
		"ok":        err == nil,
		"bind":      s.BindConfig(),
		"listening": s.Listening(),
		"failures":  failures,
		"port":      s.Port(),
		"lan_open":  HasNonLoopback(s.Listening()),
	}
	if err != nil {
		resp["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAutostart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if in.Enabled == nil {
		writeErr(w, ErrBadBody)
		return
	}
	if s.SetAutostart == nil {
		writeErr(w, ErrDesktopOnly)
		return
	}
	if err := s.SetAutostart(*in.Enabled); err != nil {
		writeErr(w, err)
		return
	}
	state := *in.Enabled
	if s.AutostartStatus != nil {
		state = s.AutostartStatus()
	}
	writeJSON(w, http.StatusOK, map[string]any{"autostart": state})
}

func (s *Server) handleWindowHide(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("target") == "editor" {
		hidden := false
		if s.OnHideEditor != nil {
			s.OnHideEditor()
			hidden = true
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hidden": hidden})
		return
	}
	hidden := false
	if s.OnHide != nil {
		s.OnHide()
		hidden = true
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hidden": hidden})
}

func (s *Server) handleWindowDrag(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("target") == "editor" {
		if s.OnDragEditor != nil {
			s.OnDragEditor()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if s.OnDrag != nil {
		s.OnDrag()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleWindowResize(w http.ResponseWriter, r *http.Request) {
	edge := strings.TrimSpace(r.URL.Query().Get("edge"))
	if s.OnResize == nil {
		writeErr(w, ErrDesktopOnly)
		return
	}
	if edge == "" {
		writeErr(w, ErrBadBody)
		return
	}
	s.OnResize(edge, r.URL.Query().Get("target") == "editor")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleWindowClose(w http.ResponseWriter, r *http.Request) {
	if s.OnClose != nil {
		s.OnClose()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleWindowResizeEnd(w http.ResponseWriter, r *http.Request) {
	if s.OnResizeEnd != nil {
		s.OnResizeEnd()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleEditorOpen(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SectionID *int64 `json:"section_id"`
		TaskID    *int64 `json:"task_id"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if s.OnOpenEditor == nil {
		writeErr(w, ErrDesktopOnly)
		return
	}
	if in.SectionID == nil || *in.SectionID <= 0 {
		writeErr(w, ErrNoSection)
		return
	}
	var taskID int64
	if in.TaskID != nil {
		taskID = *in.TaskID
	}
	s.OnOpenEditor(*in.SectionID, taskID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleEditorReady(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title *string `json:"title"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in)
	if s.OnEditorReady != nil {
		title := ""
		if in.Title != nil {
			title = *in.Title
		}
		s.OnEditorReady(title)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleEditorContext(w http.ResponseWriter, r *http.Request) {
	sectionID, ok := parseID(w, r.URL.Query().Get("sid"), ErrNoSection)
	if !ok {
		return
	}
	section, err := s.store.SectionByID(sectionID)
	if err != nil {
		writeErr(w, err)
		return
	}
	var task *Task
	if raw := strings.TrimSpace(r.URL.Query().Get("tid")); raw != "" && raw != "0" {
		taskID, ok := parseID(w, raw, ErrNotFound)
		if !ok {
			return
		}
		found, err := s.store.TaskByID(sectionID, taskID)
		if err != nil {
			writeErr(w, err)
			return
		}
		task = &found
	}
	writeJSON(w, http.StatusOK, map[string]any{"section": section, "task": task})
}

type ctxKey string

const sectionCtxKey ctxKey = "section"

func (s *Server) authExternal(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("X-Auth-Key"))
		if key == "" {
			writeErr(w, ErrNoAuthKey)
			return
		}
		sec, err := s.store.SectionByKey(key)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				writeErr(w, ErrBadAuthKey)
				return
			}
			writeErr(w, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sectionCtxKey, sec)))
	}
}

func sectionFrom(r *http.Request) (Section, bool) {
	sec, ok := r.Context().Value(sectionCtxKey).(Section)
	return sec, ok
}

func (s *Server) resolveTarget(w http.ResponseWriter, keySec Section, ref string) (Section, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return keySec, true
	}
	sec, err := s.store.ResolveSection(ref)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, ErrNoSection)
			return Section{}, false
		}
		writeErr(w, err)
		return Section{}, false
	}
	if sec.ID != keySec.ID {
		writeErr(w, ErrForbidden)
		return Section{}, false
	}
	return sec, true
}

func (s *Server) handleExternalSection(w http.ResponseWriter, r *http.Request) {
	keySec, ok := sectionFrom(r)
	if !ok {
		writeErr(w, ErrBadAuthKey)
		return
	}
	sec, ok := s.resolveTarget(w, keySec, r.URL.Query().Get("section"))
	if !ok {
		return
	}
	tasks, err := s.store.ListTasks(&sec.ID, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"section": sec,
		"counts":  tally(tasks, time.Now().Format(timeLayout)),
	})
}

func (s *Server) handleExternalTaskList(w http.ResponseWriter, r *http.Request) {
	keySec, ok := sectionFrom(r)
	if !ok {
		writeErr(w, ErrBadAuthKey)
		return
	}
	sec, ok := s.resolveTarget(w, keySec, r.URL.Query().Get("section"))
	if !ok {
		return
	}
	q := r.URL.Query()
	var status string
	if raw := q.Get("status"); strings.TrimSpace(raw) != "" {
		parsed, err := parseStatus(raw)
		if err != nil {
			writeErr(w, err)
			return
		}
		status = parsed
	} else if done, err := parseDone(q.Get("done")); err != nil {
		writeErr(w, err)
		return
	} else if done != nil {
		if *done {
			status = "done"
		} else {
			status = "doing" // done=false：未完成（含处理中），下面单独处理
		}
	}
	var statusFilter *string
	if status == "doing" && strings.TrimSpace(q.Get("status")) == "" {
		// done=false 兼容：返回未完成 + 处理中
		tasks, err := s.store.ListTasks(&sec.ID, nil)
		if err != nil {
			writeErr(w, err)
			return
		}
		filtered := make([]Task, 0, len(tasks))
		for _, t := range tasks {
			if t.Status != "done" {
				filtered = append(filtered, t)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"section": sec, "tasks": filtered, "count": len(filtered)})
		return
	}
	if status != "" {
		statusFilter = &status
	}
	tasks, err := s.store.ListTasks(&sec.ID, statusFilter)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"section": sec, "tasks": tasks, "count": len(tasks)})
}

func (s *Server) handleExternalTaskCreate(w http.ResponseWriter, r *http.Request) {
	keySec, ok := sectionFrom(r)
	if !ok {
		writeErr(w, ErrBadAuthKey)
		return
	}
	var in taskInput
	if !decodeBody(w, r, &in) {
		return
	}
	ref := r.URL.Query().Get("section")
	if in.Section != nil && strings.TrimSpace(*in.Section) != "" {
		ref = *in.Section
	} else if in.SectionID != nil {
		ref = strconv.FormatInt(*in.SectionID, 10)
	}
	sec, ok := s.resolveTarget(w, keySec, ref)
	if !ok {
		return
	}
	title := ""
	if in.Title != nil {
		title = *in.Title
	}
	description := ""
	if in.Description != nil {
		description = *in.Description
	}
	var due *string
	if in.Due.Set {
		due = in.Due.Value
	}
	task, err := s.store.CreateTask(sec.ID, title, description, due)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.notify("board")
	writeJSON(w, http.StatusCreated, map[string]any{"task": task})
}

func (s *Server) handleExternalTaskUpdate(w http.ResponseWriter, r *http.Request) {
	keySec, ok := sectionFrom(r)
	if !ok {
		writeErr(w, ErrBadAuthKey)
		return
	}
	taskID, ok := parseID(w, r.PathValue("id"), ErrNotFound)
	if !ok {
		return
	}
	var in taskInput
	if !decodeBody(w, r, &in) {
		return
	}
	ref := r.URL.Query().Get("section")
	if in.Section != nil && strings.TrimSpace(*in.Section) != "" {
		ref = *in.Section
	} else if in.SectionID != nil {
		ref = strconv.FormatInt(*in.SectionID, 10)
	}
	sec, ok := s.resolveTarget(w, keySec, ref)
	if !ok {
		return
	}
	patch := TaskPatch{Title: in.Title, Description: in.Description, BarkOverride: in.BarkOverride}
	if in.Due.Set {
		patch.Due = duePatch{Set: true, Value: in.Due.Value}
	}
	task, err := s.store.UpdateTask(sec.ID, taskID, patch)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.notify("board")
	writeJSON(w, http.StatusOK, map[string]any{"task": task})
}

func (s *Server) handleExternalTaskDone(w http.ResponseWriter, r *http.Request) {
	keySec, ok := sectionFrom(r)
	if !ok {
		writeErr(w, ErrBadAuthKey)
		return
	}
	taskID, ok := parseID(w, r.PathValue("id"), ErrNotFound)
	if !ok {
		return
	}
	var in doneInput
	if !decodeBody(w, r, &in) {
		return
	}
	sec, ok := s.resolveTarget(w, keySec, r.URL.Query().Get("section"))
	if !ok {
		return
	}
	prev, err := s.store.TaskByID(sec.ID, taskID)
	if err != nil {
		writeErr(w, err)
		return
	}
	task, err := s.store.SetDone(sec.ID, taskID, in.Done)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.notify("board")
	resp := map[string]any{"task": task}
	if task.Status == "done" && prev.Status != "done" {
		resp["bark"] = s.pushBark(sec, task)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleExternalTaskStatus(w http.ResponseWriter, r *http.Request) {
	keySec, ok := sectionFrom(r)
	if !ok {
		writeErr(w, ErrBadAuthKey)
		return
	}
	taskID, ok := parseID(w, r.PathValue("id"), ErrNotFound)
	if !ok {
		return
	}
	var in struct {
		Status *string `json:"status"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if in.Status == nil {
		writeErr(w, ErrBadStatus)
		return
	}
	sec, ok := s.resolveTarget(w, keySec, r.URL.Query().Get("section"))
	if !ok {
		return
	}
	prev, err := s.store.TaskByID(sec.ID, taskID)
	if err != nil {
		writeErr(w, err)
		return
	}
	task, err := s.store.SetStatus(sec.ID, taskID, *in.Status)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.notify("board")
	resp := map[string]any{"task": task}
	if task.Status == "done" && prev.Status != "done" {
		resp["bark"] = s.pushBark(sec, task)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleExternalTaskDelete(w http.ResponseWriter, r *http.Request) {
	keySec, ok := sectionFrom(r)
	if !ok {
		writeErr(w, ErrBadAuthKey)
		return
	}
	taskID, ok := parseID(w, r.PathValue("id"), ErrNotFound)
	if !ok {
		return
	}
	sec, ok := s.resolveTarget(w, keySec, r.URL.Query().Get("section"))
	if !ok {
		return
	}
	if err := s.store.DeleteTask(sec.ID, taskID); err != nil {
		writeErr(w, err)
		return
	}
	s.notify("board")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* ---------------- 排序 / 分页 / Bark ---------------- */

func pageParams(r *http.Request) (int, int) {
	limit := 20
	offset := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			if v > 200 {
				v = 200
			}
			limit = v
		}
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v >= 0 {
			offset = v
		}
	}
	return limit, offset
}

func (s *Server) handleSectionsOrder(w http.ResponseWriter, r *http.Request) {
	var in []SectionOrder
	if !decodeBody(w, r, &in) {
		return
	}
	if len(in) == 0 {
		writeErr(w, ErrBadBody)
		return
	}
	// 请求顺序即最终顺序，sort_order 由服务端归一化
	if err := s.store.ReorderSections(in); err != nil {
		writeErr(w, err)
		return
	}
	s.notify("sections")
	sections, err := s.store.ListSections()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sections": sections, "count": len(sections)})
}

func (s *Server) handleWebSections(w http.ResponseWriter, r *http.Request) {
	limit, offset := pageParams(r)
	sections, total, err := s.store.ListSectionsPaged(limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	now := time.Now().Format(timeLayout)
	counts, err := s.store.CountsBySection(now)
	if err != nil {
		writeErr(w, err)
		return
	}
	type webSection struct {
		Section
		SectionCounts
	}
	out := make([]webSection, 0, len(sections))
	for _, sec := range sections {
		out = append(out, webSection{Section: sec, SectionCounts: counts[sec.ID]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sections": out, "count": len(out), "total": total})
}

func (s *Server) handleWebTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sectionID, ok := parseID(w, q.Get("section"), ErrNoSection)
	if !ok {
		return
	}
	sec, err := s.store.SectionByID(sectionID)
	if err != nil {
		writeErr(w, err)
		return
	}
	status, err := parseStatus(q.Get("status"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var filter *string
	if status != "" {
		filter = &status
	}
	limit, offset := pageParams(r)
	tasks, total, err := s.store.ListTasksPaged(&sec.ID, filter, limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"section": sec, "tasks": tasks, "count": len(tasks), "total": total,
		"limit": limit, "offset": offset,
	})
}

func barkPublicView(cfg barkConfig) map[string]any {
	return map[string]any{
		"server":         cfg.Server,
		"icon":           cfg.Icon,
		"enabled":        cfg.Enabled,
		"key_set":        strings.TrimSpace(cfg.Key) != "",
		"key_masked":     maskKey(cfg.Key),
		"default_server": defaultBarkServer,
	}
}

func (s *Server) handleBarkGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, barkPublicView(loadBarkConfig()))
}

func (s *Server) handleBarkPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Server  *string `json:"server"`
		Key     *string `json:"key"`
		Icon    *string `json:"icon"`
		Enabled *bool   `json:"enabled"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	cfg := loadBarkConfig()
	if in.Server != nil {
		if v := strings.TrimSpace(*in.Server); v != "" {
			cfg.Server = v
		} else {
			cfg.Server = defaultBarkServer
		}
	}
	if in.Key != nil {
		cfg.Key = strings.TrimSpace(*in.Key)
	}
	if in.Icon != nil {
		cfg.Icon = strings.TrimSpace(*in.Icon)
	}
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
	}
	if err := saveBarkConfig(cfg); err != nil {
		log.Printf("保存 Bark 配置失败: %v", err)
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, barkPublicView(cfg))
}

func (s *Server) handleBarkTest(w http.ResponseWriter, r *http.Request) {
	cfg := loadBarkConfig()
	payload := barkPayload{Title: "待办看板", Body: "这是一条测试推送", Group: "待办看板", Icon: cfg.Icon}
	if err := sendBark(cfg, payload); err != nil {
		log.Printf("Bark 测试推送失败: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleWebStats(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Format(timeLayout)
	bySection, err := s.store.CountsBySection(now)
	if err != nil {
		writeErr(w, err)
		return
	}
	var t counts
	for _, c := range bySection {
		t.Total += c.Total
		t.Open += c.Open
		t.Doing += c.Doing
		t.Done += c.Done
		t.Overdue += c.Overdue
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"date":    time.Now().Format("2006-01-02"),
		"total":   t.Total,
		"open":    t.Open,
		"doing":   t.Doing,
		"done":    t.Done,
		"overdue": t.Overdue,
	})
}

// addrConflicts 判断两个监听地址是否会抢同一端口（通配地址与同类具体地址冲突）。
func addrConflicts(a, b string) bool {
	if a == b {
		return false
	}
	ipA, ipB := net.ParseIP(a), net.ParseIP(b)
	isV4 := func(ip net.IP) bool { return ip != nil && ip.To4() != nil }
	isV6 := func(ip net.IP) bool { return ip != nil && ip.To4() == nil }
	if a == "0.0.0.0" {
		return isV4(ipB)
	}
	if a == "::" {
		return isV6(ipB)
	}
	if b == "0.0.0.0" {
		return isV4(ipA)
	}
	if b == "::" {
		return isV6(ipA)
	}
	return false
}

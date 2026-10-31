package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// 盯盘内核（T123）：主进程内跑，一区一个 goroutine。
//   - 每 tick 只全量读一次看板（BoardSnapshot 一条 JOIN SQL），再分发给各区线程算 diff
//   - 区内多个盯盘目标各自的 pending 队列独立，一个目标 401 不阻塞同区其它目标
//   - 重启后按库里 baseline 继续算 diff，绝不重建基线（否则停机期间的变更会被吞）
//   - 推送一律走 AionUI helper 子进程，token 只在子进程环境里出现，绝不落日志

const (
	defaultMonitorInterval = 60
	minMonitorInterval     = 1
	maxMonitorInterval     = 3600

	// 推送结果分类（写进 monitor_tokens.last_status）
	statusPushOK            = 200
	statusHelperNotConfiged = -1 // 未配置 helper 路径或文件不存在
	statusHelperFailed      = -2 // helper 退出码非 0 且看不出鉴权码
	statusHelperTimeout     = -3 // helper 超时
	statusHelperBadOutput   = -4 // 输出无法判定（视为失败）
)

type sectionJob struct {
	section Section
	tasks   []Task
	subs    []MonitorSub
}

type monitorKernel struct {
	store *Store
	stop  chan struct{}
	once  sync.Once
	wg    sync.WaitGroup

	mu        sync.Mutex
	workers   map[int64]chan sectionJob
	lastPush  map[int64]time.Time
	skipUntil map[int64]time.Time
}

func StartMonitorKernel(st *Store) *monitorKernel {
	k := &monitorKernel{
		store:     st,
		stop:      make(chan struct{}),
		workers:   map[int64]chan sectionJob{},
		lastPush:  map[int64]time.Time{},
		skipUntil: map[int64]time.Time{},
	}
	k.wg.Add(1)
	go k.loop()
	k.wg.Add(1)
	go k.leaseLoop()
	return k
}

func (k *monitorKernel) Stop() {
	k.once.Do(func() { close(k.stop) })
	k.wg.Wait()
}

func (k *monitorKernel) loop() {
	defer k.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-k.stop:
			return
		case <-ticker.C:
			k.tick()
		}
	}
}

// tick 每个子决定「哪些 sub 到点了」，只读一次看板，再丢给各区 goroutine。
func (k *monitorKernel) tick() {
	subs, err := k.store.ListMonitors(nil)
	if err != nil {
		log.Printf("盯盘：读取注册表失败: %v", err)
		return
	}
	if len(subs) == 0 {
		return
	}
	now := time.Now()
	due := map[int64][]MonitorSub{}
	for _, sub := range subs {
		k.mu.Lock()
		lp, su := k.lastPush[sub.ID], k.skipUntil[sub.ID]
		k.mu.Unlock()
		if now.Before(su) {
			continue
		}
		interval := sub.IntervalSec
		if interval < minMonitorInterval {
			interval = defaultMonitorInterval
		}
		if now.Sub(lp) < time.Duration(interval)*time.Second {
			continue
		}
		due[sub.SectionID] = append(due[sub.SectionID], sub)
	}
	if len(due) == 0 {
		return
	}

	sections, tasks, err := k.store.BoardSnapshot()
	if err != nil {
		log.Printf("盯盘：读取看板失败: %v", err)
		return
	}
	secByID := map[int64]Section{}
	tasksBySection := map[int64][]Task{}
	for _, s := range sections {
		secByID[s.ID] = s
	}
	for _, t := range tasks {
		tasksBySection[t.SectionID] = append(tasksBySection[t.SectionID], t)
	}

	for sid, list := range due {
		sec, ok := secByID[sid]
		if !ok {
			continue // 分区已被删除，等 cleanup 清掉注册行
		}
		ch := k.worker(sid)
		select {
		case ch <- sectionJob{section: sec, tasks: tasksBySection[sid], subs: list}:
		default: // 上一批还在跑（推送慢）：本 tick 跳过，next tick 再来
		}
	}
}

func (k *monitorKernel) worker(sectionID int64) chan sectionJob {
	k.mu.Lock()
	defer k.mu.Unlock()
	if ch, ok := k.workers[sectionID]; ok {
		return ch
	}
	ch := make(chan sectionJob, 1)
	k.workers[sectionID] = ch
	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		for {
			select {
			case <-k.stop:
				return
			case job := <-ch:
				for _, sub := range job.subs {
					k.processSub(job.section, job.tasks, sub)
				}
			}
		}
	}()
	return ch
}

func (k *monitorKernel) markPushed(subID int64) {
	k.mu.Lock()
	k.lastPush[subID] = time.Now()
	k.mu.Unlock()
}

func (k *monitorKernel) processSub(sec Section, tasks []Task, sub MonitorSub) {
	defer k.markPushed(sub.ID)

	cur := buildSectionSnap(sec, tasks)
	curRaw := encodeSnap(cur)
	prev, hadBaseline := decodeSnap(sub.Baseline)

	if !hadBaseline {
		// 新注册 / 库里基线为空：只建立基线，不推送（与 pager 语义一致）
		if err := k.store.SetMonitorBaseline(sub.ID, curRaw); err != nil {
			log.Printf("盯盘：保存基线失败（分区 #%d）: %v", sub.SectionID, err)
		}
		return
	}

	changes := diffSectionSnap(prev, cur)
	for _, c := range changes {
		b, err := json.Marshal(c)
		if err != nil {
			continue
		}
		if err := k.store.EnqueueMonitorPending(sub.ID, string(b)); err != nil {
			log.Printf("盯盘：写 pending 失败（分区 #%d）: %v", sub.SectionID, err)
		}
	}
	// 基线始终前移：同一变化只报一次
	if err := k.store.SetMonitorBaseline(sub.ID, curRaw); err != nil {
		log.Printf("盯盘：保存基线失败（分区 #%d）: %v", sub.SectionID, err)
	}

	pending, err := k.store.ListMonitorPending(sub.ID)
	if err != nil {
		log.Printf("盯盘：读取 pending 失败（分区 #%d）: %v", sub.SectionID, err)
		return
	}
	if len(pending) == 0 {
		return
	}

	cfg := loadAppSettings()
	msg := buildMergedNotice(sec, pending)
	status := pushNotice(cfg, sub, k.store.MonitorToken(sub.ConversationID), msg)
	now := time.Now()
	if err := k.store.SetMonitorTokenStatus(sub.ConversationID, status, now.Format(stampFmt)); err != nil {
		log.Printf("盯盘：写推送状态失败（分区 #%d）: %v", sub.SectionID, err)
	}
	ids := make([]int64, 0, len(pending))
	for _, p := range pending {
		ids = append(ids, p.ID)
	}
	if status == statusPushOK {
		if err := k.store.ClearMonitorPending(ids); err != nil {
			log.Printf("盯盘：清理 pending 失败（分区 #%d）: %v", sub.SectionID, err)
			return
		}
		k.mu.Lock()
		delete(k.skipUntil, sub.ID)
		k.mu.Unlock()
		return
	}
	// 失败：按 skip_interval 重试，不设最大次数；pending 继续累积
	skip := time.Duration(cfg.SkipInterval) * time.Second
	k.mu.Lock()
	k.skipUntil[sub.ID] = now.Add(skip)
	k.mu.Unlock()
	log.Printf("盯盘：推送失败（分区 #%d，目标 %s，结果码 %d），%d 秒后重试，pending %d 条",
		sub.SectionID, sub.TargetType, status, cfg.SkipInterval, len(pending))
}

// buildMergedNotice 把多条 pending 合并成一条消息。
func buildMergedNotice(sec Section, pending []MonitorPending) string {
	var newT, chgT, delT []string
	var notes []string
	for _, p := range pending {
		var c snapChange
		if err := json.Unmarshal([]byte(p.Payload), &c); err != nil {
			continue
		}
		switch c.Kind {
		case "new":
			newT = append(newT, c.Title)
		case "changed":
			line := c.Title
			if len(c.Fields) > 0 {
				line += "（" + strings.Join(c.Fields, "、") + "）"
			}
			chgT = append(chgT, line)
		case "deleted":
			delT = append(delT, c.Title)
		case "section":
			notes = append(notes, c.Note)
		}
	}
	total := len(newT) + len(chgT) + len(delT) + len(notes)
	var b strings.Builder
	fmt.Fprintf(&b, "[通知] 分区「%s」#%d 有 %d 条变化：新增 %d、变更 %d、删除 %d\n",
		sec.Name, sec.ID, total, len(newT), len(chgT), len(delT))
	for _, t := range newT {
		fmt.Fprintf(&b, "+ [新增] %s\n", t)
	}
	for _, t := range chgT {
		fmt.Fprintf(&b, "~ [变更] %s\n", t)
	}
	for _, t := range delT {
		fmt.Fprintf(&b, "- [删除] %s\n", t)
	}
	for _, n := range notes {
		fmt.Fprintf(&b, "· %s\n", n)
	}
	msg := b.String()
	if len(msg) > 3500 {
		msg = msg[:3500] + "\n…（内容过长已截断）"
	}
	return msg
}

// pushNotice 调 AionUI helper 子进程发一条消息。返回状态码（见 status* 常量）。
func pushNotice(cfg appSettings, sub MonitorSub, token, msg string) int {
	helper := strings.TrimSpace(cfg.HelperPath)
	if helper == "" {
		return statusHelperNotConfiged
	}
	if st, err := os.Stat(helper); err != nil || st.IsDir() {
		return statusHelperNotConfiged
	}

	// helper 的 send-message 不接旗标，正文与目标都走 stdin 的 JSON：{to, message}
	// 团队用 slot_id 作 to（团队本身由 helper 从 AIONUI_* 环境推导），单聊用会话 id。
	group := "team"
	payload := map[string]string{"to": sub.SlotID, "message": msg}
	if sub.TargetType != "team" {
		group = "session"
		payload = map[string]string{"to": sub.ConversationID, "message": msg}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return statusHelperBadOutput
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, helper, group, "send-message")
	cmd.Stdin = bytes.NewReader(raw)
	env := os.Environ()
	if token != "" {
		env = append(env, "AIONUI_RUNTIME_TOKEN="+token)
	}
	if cfg.BaseURL != "" {
		env = append(env, "AIONUI_BASE_URL="+cfg.BaseURL)
	}
	cmd.Env = env
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return statusHelperTimeout
	}
	if err != nil {
		// 只把输出当作「分类线索」，日志里也只记状态码，不打印输出（可能带 token 回显）
		text := out.String()
		switch {
		case strings.Contains(text, "401"):
			return http.StatusUnauthorized
		case strings.Contains(text, "403"):
			return http.StatusForbidden
		case strings.Contains(text, "404"):
			return http.StatusNotFound
		}
		return statusHelperFailed
	}
	return statusPushOK
}

/* ---------------- 团队/会话保活 ---------------- */

func (k *monitorKernel) leaseLoop() {
	defer k.wg.Done()
	// 启动后先等一下，避免和首轮注册抢锁
	select {
	case <-k.stop:
		return
	case <-time.After(5 * time.Second):
	}
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		k.leaseOnce()
		select {
		case <-k.stop:
			return
		case <-ticker.C:
		}
	}
}

func (k *monitorKernel) leaseOnce() {
	cfg := loadAppSettings()
	if cfg.BaseURL == "" {
		return // 没配宿主地址就不续租
	}
	targets, err := k.store.LeaseTargets()
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 8 * time.Second}
	for _, t := range targets {
		var url string
		if t.TargetType == "team" && t.TeamID != "" {
			url = fmt.Sprintf("%s/api/teams/%s/active-lease", cfg.BaseURL, t.TeamID)
		} else {
			url = fmt.Sprintf("%s/api/conversations/%s/active-lease", cfg.BaseURL, t.ConversationID)
		}
		if len(t.Tokens) == 0 {
			continue
		}
		// 同目标的多个 token 轮试，任一 200 即算成功
		for _, tok := range t.Tokens {
			req, err := http.NewRequest(http.MethodPost, url, nil)
			if err != nil {
				break
			}
			req.Header.Set("x-aionui-runtime-token", tok)
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			code := resp.StatusCode
			resp.Body.Close()
			if code == http.StatusOK {
				break
			}
		}
	}
}

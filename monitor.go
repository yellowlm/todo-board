package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 盯盘内核（T123）：主进程内跑，一区一个 goroutine。
//   - 每 tick 只全量读一次看板（BoardSnapshot 一条 JOIN SQL），再分发给各区线程算 diff
//   - 区内多个盯盘目标各自的 pending 队列独立，一个目标 401 不阻塞同区其它目标
//   - 重启后按库里 baseline 继续算 diff，绝不重建基线（否则停机期间的变更会被吞）
//   - 推送直连 AionUI 消息端点（HTTP POST），token 只出现在请求头里，绝不落日志

const (
	defaultMonitorInterval = 60
	minMonitorInterval     = 1
	maxMonitorInterval     = 3600

	// 推送结果分类（写进 monitor_tokens.last_status）
	statusPushOK        = 200
	statusNotConfigured = -1 // 连接配置不完整（保留位，当前未使用）
	statusPushFailed    = -2 // 请求失败（网络/非预期错误）
	statusPushTimeout   = -3 // 请求超时
	statusBadPayload    = -4 // 请求体构造失败
	statusNoBaseURL     = -5 // 连接没有上报 AionUI 地址，无法推送
	statusNoCreds       = -6 // 连接没有 token / user_id，无法推送
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
	token, baseURL := sub.Token, sub.BaseURL
	msg := buildMergedNotice(sec, pending)
	status := pushNotice(cfg, sub, token, baseURL, msg)
	now := time.Now()
	if err := k.store.SetMonitorTokenStatus(sub.SectionID, sub.ConversationID, status, now.Format(stampFmt)); err != nil {
		log.Printf("盯盘：写推送状态失败（分区 #%d）: %v", sub.SectionID, err)
	}
	ids := make([]int64, 0, len(pending))
	for _, p := range pending {
		ids = append(ids, p.ID)
	}
	if status >= 200 && status < 300 {
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

// pushNotice 直连 AionUI 的消息端点发一条通知。
// 团队：POST {base}/api/teams/{team}/agents/{slot}/messages
// 单聊：POST {base}/api/conversations/{conv}/messages
// 2xx 即成功（200 / 202 都算）；401/403/404 原样写回 last_status。
func pushNotice(cfg appSettings, sub MonitorSub, token, baseURL, msg string) int {
	// 宿主地址只认该连接注册/心跳时上报的 base_url；没有就跳过（设置页不再提供默认地址）
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return statusNoBaseURL
	}
	if strings.TrimSpace(token) == "" {
		return statusNoCreds
	}
	userID := sub.UserID
	if userID == "" {
		userID = loadAionUserID(sub.ConversationID) // 老数据回退到侧车
	}
	if userID == "" {
		return statusNoCreds
	}

	var url string
	if sub.TargetType == "team" {
		url = fmt.Sprintf("%s/api/teams/%s/agents/%s/messages", base, sub.TeamID, sub.SlotID)
	} else {
		url = fmt.Sprintf("%s/api/conversations/%s/messages", base, sub.ConversationID)
	}
	payload, err := json.Marshal(map[string]string{"content": msg})
	if err != nil {
		return statusBadPayload
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return statusPushFailed
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-aionui-runtime-token", token)
	req.Header.Set("x-aionui-user-id", userID)
	req.Header.Set("x-aionui-conversation-id", sub.ConversationID)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if ctx.Err() == context.DeadlineExceeded {
		return statusPushTimeout
	}
	if err != nil {
		return statusPushFailed
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode
	}
	return resp.StatusCode
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
	targets, err := k.store.LeaseTargets()
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 8 * time.Second}
	for _, t := range targets {
		base := strings.TrimRight(strings.TrimSpace(t.BaseURL), "/")
		if base == "" {
			continue // 该连接没上报地址：跳过（不再回退设置页）
		}
		var url string
		if t.TargetType == "team" && t.TeamID != "" {
			url = fmt.Sprintf("%s/api/teams/%s/active-lease", base, t.TeamID)
		} else {
			url = fmt.Sprintf("%s/api/conversations/%s/active-lease", base, t.ConversationID)
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

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 不内置任何私有服务地址：留空要求用户在设置页自行填写
const defaultBarkServer = ""

// barkConfig 存在 %LOCALAPPDATA%\AgentTodoBoard\bark.json，不进交付目录。
type barkConfig struct {
	Server  string `json:"server"`
	Key     string `json:"key"`
	Icon    string `json:"icon"`
	Enabled bool   `json:"enabled"`
}

func barkConfigPath() string { return filepath.Join(appDataDir(), "bark.json") }

func loadBarkConfig() barkConfig {
	cfg := barkConfig{Server: defaultBarkServer}
	raw, err := os.ReadFile(barkConfigPath())
	if err != nil {
		return cfg
	}
	var stored barkConfig
	if err := json.Unmarshal(raw, &stored); err != nil {
		return cfg
	}
	if s := strings.TrimSpace(stored.Server); s != "" {
		cfg.Server = s
	}
	cfg.Key = strings.TrimSpace(stored.Key)
	cfg.Icon = strings.TrimSpace(stored.Icon)
	cfg.Enabled = stored.Enabled
	return cfg
}

func saveBarkConfig(cfg barkConfig) error {
	path := barkConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type barkPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Group string `json:"group,omitempty"`
	Icon  string `json:"icon,omitempty"`
}

// sendBark POST <server>/<key>，5 秒超时。
func sendBark(cfg barkConfig, p barkPayload) error {
	server := strings.TrimRight(strings.TrimSpace(cfg.Server), "/")
	key := strings.TrimSpace(cfg.Key)
	if server == "" || key == "" {
		return errors.New("未配置 Bark 服务器或设备 Key")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, server+"/"+key, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求 Bark 失败: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Bark 返回 HTTP %d", resp.StatusCode)
	}
	return nil
}

// barkEffective 判定是否推送：任务级覆盖优先，其次分区开关，再受总开关约束。
func barkEffective(cfg barkConfig, sec Section, task Task) bool {
	if !cfg.Enabled {
		return false
	}
	switch task.BarkOverride {
	case "on":
		return true
	case "off":
		return false
	}
	return sec.BarkEnabled
}

func barkBody(sec Section, task Task) string {
	due := "未设截止"
	if task.DueAt != nil && strings.TrimSpace(*task.DueAt) != "" {
		due = strings.Replace(*task.DueAt, "T", " ", 1)
	}
	return sec.Name + " · " + due
}

type barkResult struct {
	Pushed  bool   `json:"pushed"`
	Skipped bool   `json:"skipped"`
	Error   string `json:"error,omitempty"`
}

// pushBark 在任务完成时推送，失败只记日志不阻断状态变更。
func (s *Server) pushBark(sec Section, task Task) *barkResult {
	cfg := loadBarkConfig()
	if !barkEffective(cfg, sec, task) {
		return &barkResult{Skipped: true}
	}
	payload := barkPayload{Title: task.Title, Body: barkBody(sec, task), Group: sec.Name, Icon: cfg.Icon}
	if err := sendBark(cfg, payload); err != nil {
		log.Printf("Bark 推送失败（任务 %d）：%v", task.ID, err)
		return &barkResult{Error: err.Error()}
	}
	log.Printf("Bark 推送成功（任务 %d，分区 %s）", task.ID, sec.Name)
	return &barkResult{Pushed: true}
}

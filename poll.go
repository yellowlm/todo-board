package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// 应用设置（%LOCALAPPDATA%\AgentTodoBoard\settings.json）：
//   - poll_interval_sec   前端回退轮询间隔（秒）
//   - aionui_helper_path  AionUI helper 可执行文件路径（盯盘推送用，空＝未配置）
//   - aionui_base_url     AionUI 宿主地址（团队/会话保活用，空＝不续租）
//   - skip_interval_sec   推送失败后的重试间隔（秒）
const (
	defaultPollInterval = 5
	minPollInterval     = 1
	maxPollInterval     = 300

	defaultSkipInterval = 60
	minSkipInterval     = 5
	maxSkipInterval     = 3600
)

type appSettings struct {
	IntervalSec  int    `json:"poll_interval_sec"`
	HelperPath   string `json:"aionui_helper_path"`
	BaseURL      string `json:"aionui_base_url"`
	SkipInterval int    `json:"skip_interval_sec"`
}

// pollConfigPath：应用设置文件，与 bark.json / listen.json 同级
func pollConfigPath() string { return filepath.Join(appDataDir(), "settings.json") }

// loadAppSettings 读取全部设置；缺失/损坏/越界逐项回退默认值（不因为一个坏字段丢其它字段）。
func loadAppSettings() appSettings {
	cfg := appSettings{IntervalSec: defaultPollInterval, SkipInterval: defaultSkipInterval}
	raw, err := os.ReadFile(pollConfigPath())
	if err != nil {
		return cfg
	}
	var stored appSettings
	if err := json.Unmarshal(raw, &stored); err != nil {
		log.Printf("settings.json 无法解析，使用默认值: %v", err)
		return cfg
	}
	if stored.IntervalSec >= minPollInterval && stored.IntervalSec <= maxPollInterval {
		cfg.IntervalSec = stored.IntervalSec
	}
	if stored.SkipInterval >= minSkipInterval && stored.SkipInterval <= maxSkipInterval {
		cfg.SkipInterval = stored.SkipInterval
	}
	cfg.HelperPath = strings.TrimSpace(stored.HelperPath)
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(stored.BaseURL), "/")
	return cfg
}

func saveAppSettings(cfg appSettings) error {
	path := pollConfigPath()
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

func loadPollInterval() int { return loadAppSettings().IntervalSec }

// savePollInterval 只改轮询间隔，其余设置原样保留（读-改-写）。
func savePollInterval(n int) error {
	cfg := loadAppSettings()
	cfg.IntervalSec = n
	return saveAppSettings(cfg)
}

func (s *Server) handlePollGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"interval_sec": loadPollInterval()})
}

// settingsPayload 返回保存后的完整设置（不含任何密钥）。
func settingsPayload() map[string]any {
	cfg := loadAppSettings()
	return map[string]any{
		"poll_interval_sec":  cfg.IntervalSec,
		"aionui_helper_path": cfg.HelperPath,
		"aionui_base_url":    cfg.BaseURL,
		"skip_interval_sec":  cfg.SkipInterval,
		"skip_min":           minSkipInterval,
		"skip_max":           maxSkipInterval,
	}
}

// handleAionPut 保存盯盘推送相关的三项设置（读-改-写，不动其它字段）。
func (s *Server) handleAionPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		HelperPath   *string `json:"aionui_helper_path"`
		BaseURL      *string `json:"aionui_base_url"`
		SkipInterval *int    `json:"skip_interval_sec"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	cfg := loadAppSettings()
	if in.HelperPath != nil {
		cfg.HelperPath = strings.TrimSpace(*in.HelperPath)
	}
	if in.BaseURL != nil {
		cfg.BaseURL = strings.TrimRight(strings.TrimSpace(*in.BaseURL), "/")
	}
	if in.SkipInterval != nil {
		if *in.SkipInterval < minSkipInterval || *in.SkipInterval > maxSkipInterval {
			writeErr(w, ErrBadBody)
			return
		}
		cfg.SkipInterval = *in.SkipInterval
	}
	if err := saveAppSettings(cfg); err != nil {
		log.Printf("保存 settings.json 失败: %v", err)
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsPayload())
}

func (s *Server) handlePollPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IntervalSec *int `json:"interval_sec"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if in.IntervalSec == nil || *in.IntervalSec < minPollInterval || *in.IntervalSec > maxPollInterval {
		writeErr(w, ErrBadBody)
		return
	}
	if err := savePollInterval(*in.IntervalSec); err != nil {
		log.Printf("保存 settings.json 失败: %v", err)
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "interval_sec": *in.IntervalSec})
}

package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

// 前端自动刷新间隔（秒）：存 %LOCALAPPDATA%\AgentTodoBoard\poll.json
const (
	defaultPollInterval = 5
	minPollInterval     = 1
	maxPollInterval     = 300
)

type pollConfig struct {
	IntervalSec int `json:"poll_interval_sec"`
}

// settingsPath：应用设置文件，与 bark.json / listen.json 同级
func pollConfigPath() string { return filepath.Join(appDataDir(), "settings.json") }

// loadPollInterval 读取间隔；文件缺失/损坏/越界都回退默认值。
func loadPollInterval() int {
	raw, err := os.ReadFile(pollConfigPath())
	if err != nil {
		return defaultPollInterval
	}
	var cfg pollConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Printf("settings.json 无法解析，使用默认 %d 秒: %v", defaultPollInterval, err)
		return defaultPollInterval
	}
	if cfg.IntervalSec < minPollInterval || cfg.IntervalSec > maxPollInterval {
		return defaultPollInterval
	}
	return cfg.IntervalSec
}

func savePollInterval(n int) error {
	path := pollConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(pollConfig{IntervalSec: n}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Server) handlePollGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"interval_sec": loadPollInterval()})
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

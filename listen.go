package main

import (
	"encoding/json"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const defaultBind = "127.0.0.1"

// listenConfig 存在 %LOCALAPPDATA%\AgentTodoBoard\listen.json。
type listenConfig struct {
	Binds []string `json:"binds"`
}

func listenConfigPath() string { return filepath.Join(appDataDir(), "listen.json") }

// loadBinds 读取监听地址；文件不存在/损坏时回退到仅本机（损坏文件备份为 .bad）。
func loadBinds() []string {
	path := listenConfigPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return []string{defaultBind}
	}
	var cfg listenConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Printf("listen.json 无法解析（已备份为 %s.bad）: %v", path, err)
		if renameErr := os.Rename(path, path+".bad"); renameErr != nil {
			log.Printf("备份 listen.json 失败: %v", renameErr)
		}
		return []string{defaultBind}
	}
	out := normalizeBinds(cfg.Binds)
	if len(out) == 0 {
		return []string{defaultBind}
	}
	return out
}

func saveBinds(binds []string) error {
	path := listenConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(listenConfig{Binds: binds}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// normalizeBinds 去空白、去掉 IPv6 方括号、去重（保持顺序）。
func normalizeBinds(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, b := range in {
		b = strings.TrimSpace(b)
		b = strings.TrimSuffix(strings.TrimPrefix(b, "["), "]")
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		key := strings.ToLower(b)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, b)
	}
	return out
}

// isLoopbackBind 判断是否为环回地址（127.0.0.0/8、::1、localhost）。
func isLoopbackBind(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// isWildcardBind 判断是否为「全部接口」的通配地址。
func isWildcardBind(host string) bool {
	switch strings.TrimSpace(host) {
	case "0.0.0.0", "::":
		return true
	}
	return false
}

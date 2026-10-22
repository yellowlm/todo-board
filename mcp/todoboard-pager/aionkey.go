package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// aioncore 实时 KEY 的来源：exe 同目录的 <FQDN>.toml
//
//	[aioncore]
//	system_key = "…\aion_key.txt"   # 本机 aioncore 自己的 toml 里写在 [aionmod] 段，两段都认
//
// 约定：
//   - 每次发起 aioncore 请求前都重新读一次 key 文件（不缓存）
//   - 读不到 / 为空 → 回退环境变量 AIONUI_RUNTIME_TOKEN，并只在日志里提示一次
//   - 任何日志与错误文本都不得包含 key 内容，最多出现路径

var (
	cfgMu     sync.Mutex
	cfgWarned bool
)

// sanitizeFileName 去掉文件名里的非法字符（与桌面端口径一致）。
func sanitizeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 32 || strings.ContainsRune(`\/:*?"<>|`, r) {
			b.WriteRune('-')
			continue
		}
		b.WriteRune(r)
	}
	out := strings.Trim(b.String(), " .-")
	if out == "" {
		out = "todo"
	}
	return out
}

func exeDirPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// configFilePath：exe 同目录下的 <FQDN>.toml（与数据库命名口径一致）。
func configFilePath() string {
	return filepath.Join(exeDirPath(), sanitizeFileName(computerFQDN())+".toml")
}

// systemKeyPathFromToml 解析配置文件里的 system_key 路径。
// 先找 [aioncore]，没有再找 [aionmod]（aioncore 自身的配置用后者）。
func systemKeyPathFromToml() (path, keyPath, section string, err error) {
	path = configFilePath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return path, "", "", fmt.Errorf("读取配置文件失败：%s", path)
	}
	section = ""
	fallback := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSuffix(line, "\r")
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
			section = strings.TrimSpace(l[1 : len(l)-1])
			continue
		}
		if !strings.HasPrefix(l, "system_key") {
			continue
		}
		eq := strings.Index(l, "=")
		if eq < 0 {
			continue
		}
		v := strings.TrimSpace(l[eq+1:])
		v = strings.Trim(v, `"'`)
		if i := strings.IndexAny(v, `"'`); i >= 0 {
			v = v[:i]
		}
		switch section {
		case "aioncore":
			return path, v, section, nil
		case "aionmod":
			if fallback == "" {
				fallback = v
			}
		}
	}
	if fallback != "" {
		return path, fallback, "aionmod", nil
	}
	return path, "", "", fmt.Errorf("配置文件里没有 [aioncore] 或 [aionmod] 的 system_key：%s", path)
}

// runtimeAionKey 每次调用都重读 key 文件（供「每次请求重读」用）。
func runtimeAionKey() (string, string, error) {
	cfgPath, keyPath, section, err := systemKeyPathFromToml()
	if err != nil {
		return "", cfgPath, err
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return "", cfgPath, fmt.Errorf("[%s] system_key 指向的文件读取失败：%s", section, keyPath)
	}
	k := strings.TrimSpace(string(raw))
	if k == "" {
		return "", cfgPath, fmt.Errorf("[%s] system_key 指向的文件为空：%s", section, keyPath)
	}
	return k, cfgPath, nil
}

// applyAionAuth 给 aioncore 请求加鉴权头：运行时 key 优先，失败回退 env。
func applyAionAuth(req *http.Request) {
	key, _, err := runtimeAionKey()
	if err == nil && key != "" {
		req.Header.Set("x-aionui-runtime-token", key)
	} else {
		req.Header.Set("x-aionui-runtime-token", envToken)
		warnFallback(err)
	}
	req.Header.Set("x-aionui-user-id", envUserID)
	req.Header.Set("x-aionui-conversation-id", envConvID)
}

// warnFallback 只在第一次回退时写一条日志，避免刷屏；不含 key 内容。
func warnFallback(err error) {
	if err == nil {
		return
	}
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if cfgWarned {
		return
	}
	cfgWarned = true
	dbg("未能使用 TOML 里的 aioncore system_key，回退到环境变量 AIONUI_RUNTIME_TOKEN（原因：%v）", err)
}

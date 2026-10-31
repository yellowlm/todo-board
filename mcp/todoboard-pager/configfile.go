package main

import (
	"os"
	"path/filepath"
	"strings"
)

// 已停用（2026-10-31）：Pager 不再直接访问 aioncore；以下为通用的配置文件读写能力，
// 日后需要时取消注释即可。
//
// 示例（读 exe 同目录 <FQDN>.toml 里 [aioncore] 的 system_key）：
//
//	p := configFilePath()
//	keyPath, err := readConfigValue("aioncore", "system_key")
//
// 约定（恢复时注意）：
//   - 只支持 `key = "value"` 的单行写法，同段重复出现取第一次
//   - 任何日志与错误文本都不得包含读到的值（最多出现路径）
//
// // configFilePath：exe 同目录下的 <FQDN>.toml（与数据库命名口径一致）。
// func configFilePath() string {
// 	return filepath.Join(exeDirPath(), sanitizeFileName(computerFQDN())+".toml")
// }
//
// // readConfigValue 读取 TOML 里 [section] 下 key 的值；找不到返回错误。
// func readConfigValue(section, key string) (string, error) {
// 	path := configFilePath()
// 	raw, err := os.ReadFile(path)
// 	if err != nil {
// 		return "", fmt.Errorf("读取配置文件失败：%s", path)
// 	}
// 	cur := ""
// 	for _, line := range strings.Split(string(raw), "\n") {
// 		l := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
// 		if l == "" || strings.HasPrefix(l, "#") {
// 			continue
// 		}
// 		if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
// 			cur = strings.TrimSpace(l[1 : len(l)-1])
// 			continue
// 		}
// 		if cur != section || !strings.HasPrefix(l, key) {
// 			continue
// 		}
// 		eq := strings.Index(l, "=")
// 		if eq < 0 {
// 			continue
// 		}
// 		if strings.TrimSpace(l[:eq]) != key {
// 			continue
// 		}
// 		v := strings.TrimSpace(l[eq+1:])
// 		v = strings.Trim(v, `"'`)
// 		if i := strings.IndexAny(v, `"'`); i >= 0 {
// 			v = v[:i]
// 		}
// 		return v, nil
// 	}
// 	return "", fmt.Errorf("配置文件里没有 [%s] %s：%s", section, key, path)
// }
//
// // writeConfigValue 写回 [section] key = "value"（与 readConfigValue 对称的占位实现）。
// // 目前未启用：需要时再补原子写（tmp + rename）。
// func writeConfigValue(section, key, value string) error {
// 	return errors.New("writeConfigValue 尚未实现")
// }

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

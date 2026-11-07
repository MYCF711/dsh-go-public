package v4

import (
	"fmt"
	"strings"
	"unicode/utf16"
)

// NormalizeCwd 去掉 Windows 的扩展长度前缀（`\\?\` 与 `\\.\`）。
//
// Node 侧客户端（Martty 等）会把 workspace 原样传过来，常常带着 `\\?\` 前缀。
// 原样落盘的话 projectKey 会变成一个真实 DSH 永远不会产生的目录名
// （形如 `--~003F-D-...`），两边就互相看不见对方的会话了。
// 真实语料里 116 个会话目录**零个**带 `~003F`。
func NormalizeCwd(p string) string {
	const (
		extended   = `\\?\` // \\?\  —— 扩展长度路径
		devicePath = `\\.\` // \\.\ —— 设备命名空间
	)
	if strings.HasPrefix(p, extended) {
		return p[len(extended):]
	}
	if strings.HasPrefix(p, devicePath) {
		return p[len(devicePath):]
	}
	return p
}

// ProjectKey 把 cwd 变成项目目录名。规则逐字对齐（research/21 §1）：
//
//  1. `/` `\` `:` 各变一个 `-`，连续 `-` 折叠；
//  2. ASCII [A-Za-z0-9._-] 且非 `~` 原样，其余每个 **UTF-16 码元** → `~XXXX`（大写 4 位 hex）；
//  3. 去前导 `-`；空 → `root`；截 251；
//  4. 包成 `--<key>--`。
func ProjectKey(cwd string) string {
	cwd = NormalizeCwd(cwd)
	var b strings.Builder
	for _, r := range cwd {
		if r == '/' || r == '\\' || r == ':' {
			b.WriteByte('-')
			continue
		}
		b.WriteRune(r)
	}

	var collapsed strings.Builder
	for i, r := range b.String() {
		if r == '-' && i > 0 && b.String()[i-1] == '-' {
			continue
		}
		collapsed.WriteRune(r)
	}

	key := escapeUTF16(collapsed.String())
	key = strings.TrimLeft(key, "-")
	if key == "" {
		key = "root"
	}
	if len(key) > 251 {
		key = key[:251]
	}
	return "--" + key + "--"
}

// EncodeSegment 转义会话 id 之类的单段文件名。
//
// `.` 与 `..` 必须特判，否则会变成目录穿越。
func EncodeSegment(id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("会话 id 为空")
	}
	switch id {
	case ".":
		return "~002E", nil
	case "..":
		return "~002E~002E", nil
	}
	return escapeUTF16(id), nil
}

// escapeUTF16 逐 UTF-16 码元转义：ASCII 安全集原样，其余（含 `~`）→ `~XXXX`。
func escapeUTF16(s string) string {
	var out strings.Builder
	for _, u := range utf16.Encode([]rune(s)) {
		c := rune(u)
		safe := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-'
		if safe {
			out.WriteRune(c)
			continue
		}
		out.WriteString(fmt.Sprintf("~%04X", u))
	}
	return out.String()
}

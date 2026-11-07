package session

import (
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// HistoryGuard 判断"这个文件能不能被我们改写"。
//
// 背景：`dshd session compact` 是**原地追加**。指错路径就会往 DSH 本体的历史里写东西，
// 而那条历史是别人（正在运行的 DSH）正在读的。
//
// 规则：
//  1. 在 `$DSH_HOME` 下面 → 一律拒绝（除非显式 Force）；
//  2. 路径里带 `sessions` 段 → 要求 Force 并在返回的 reason 里说清为什么。
func HistoryGuard(path, dshHome string, force bool) (ok bool, reason string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	clean := filepath.ToSlash(abs)

	if dshHome != "" {
		home, err := filepath.Abs(dshHome)
		if err == nil {
			hc := strings.TrimSuffix(filepath.ToSlash(home), "/")
			if clean == hc || strings.HasPrefix(clean, hc+"/") {
				if !force {
					return false, "目标是 DSH_HOME 下面（" + hc + "），这是 DSH 本体正在用的历史，换了它会污染别人的会话"
				}
				return true, ""
			}
		}
	}

	for _, seg := range strings.Split(clean, "/") {
		if seg == "sessions" {
			if !force {
				return false, "路径里有 sessions 目录，看着像别人的会话库"
			}
			return true, ""
		}
	}
	return true, ""
}

// EstimateRunes 是诊断用：估算一条字符串的 token（与 rawEstimate 同口径）。
func EstimateRunes(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	return ascii/4 + other
}

// UnderDSHHome 只判断"这个路径是不是在 $DSH_HOME 下面"——不做启发式。
//
// 给内部操作用（比如 ACP 的 session/delete 删的是**我们自己的 home**，
// 路径里当然有 sessions，不能因此就拦）。
func UnderDSHHome(path, dshHome string) bool {
	if dshHome == "" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	home, err := filepath.Abs(dshHome)
	if err != nil {
		return false
	}
	clean := strings.TrimSuffix(filepath.ToSlash(abs), "/")
	hc := strings.TrimSuffix(filepath.ToSlash(home), "/")
	return clean == hc || strings.HasPrefix(clean, hc+"/")
}

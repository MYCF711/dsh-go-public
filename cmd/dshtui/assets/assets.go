// Package assets 存放从上游 dsh-TUI（MIT）**原样搬来**的视觉素材。
//
// 为什么单独成一个包：这里的字符串是"像素"，不是代码。它们唯一的正确性判据是
// 「与上游逐字节一致」，所以单独放、单独钉 sha256（见 whale_test.go），
// 而不是散在渲染代码里——散进去的话，任何一次"顺手调一下"都不会有人发现。
//
// 来源：
//
//	https://github.com/ccch1mneyyy/dsh-TUI
//	src/components/whaleFrames.ts   （standard 帧，25 行 × 40 列）
//	src/components/Whale.tsx        （调色板 PALETTE，半块编码）
//
// Copyright (c) 2026 chimney (ccch1mneyyy) —— MIT
package assets

import (
	_ "embed"
	"strings"
)

// WhaleStandard 是上游 `whaleFrames.ts` 里 `standard` 帧的原文：
// 25 行 × 40 列，字符是调色板字母（D/B/L/W/H/Z），`.` 表示透明。
//
// 它就是 settled header 上那只静止的鲸鱼（STANDARD_FRAME_INDEX = 0）。
// 动画帧（眨眼/喷水/甩尾/爱心/睡觉）**没有**搬过来：第一阶段只做静止首屏。
//
//go:embed whale_standard.txt
var WhaleStandard string

// WhaleStandardSHA256 是 WhaleStandard 的 sha256。
//
// 它钉的不是"这个文件没被改过"（那由 git 保证），而是"文件内容仍然等于
// 从上游抽出来的那 25 行"——上游换了立绘、或有人手改了某一格，
// 这条就会红，而红的时候人知道该去看 docs/tui-visual.md 里的对照表。
const WhaleStandardSHA256 = "4b5c9ca7c886a331160d4b5bd584b55fe5c49ea294799132db1610961a6e5b4c"

// WhaleWidth / WhaleHeight 是精灵的格子数（上游源码注释：40×25 cells）。
const (
	WhaleWidth  = 40
	WhaleHeight = 25
)

// WhaleRows 返回按行切开的精灵（25 行）。
func WhaleRows() []string {
	rows := strings.Split(WhaleStandard, "\n")
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r != "" {
			out = append(out, r)
		}
	}
	return out
}

// RGB 是一个真彩色。
type RGB struct{ R, G, B uint8 }

// WhalePalette 是上游 `Whale.tsx` 的 PALETTE：
//
//	D 深蓝轮廓 · B DeepSeek 蓝身体 · L 冰蓝肚皮 · W 白嘴 · H 粉爱心 · Z 灰睡眠 Z
//
// 看着像"六种颜色随便挑的"，其实每一个都对应立绘的一层；
// 改了某一个，鲸鱼看起来还是鲸鱼，只是"不像原来那只"——所以照抄。
var WhalePalette = map[rune]RGB{
	'D': {20, 38, 96},
	'B': {78, 111, 255},
	'L': {190, 225, 255},
	'W': {255, 255, 255},
	'H': {204, 51, 153},
	'Z': {128, 128, 128},
}

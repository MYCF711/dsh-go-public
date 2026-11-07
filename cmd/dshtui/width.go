package main

// 终端**显示列宽**与"按列裁剪"。
//
// 为什么不能数 rune：一个中文字在终端上占 2 列，`len([]rune(s))` 会把
// 「探索未至之境！」当成 8 列，而它实际占 16 列。以前 fitWidth 就是这么数的，
// 症状是：中文正文行比终端宽、被终端自己折行，把整屏往下推一格；而单测断言
// 的恰好也是 rune 数，所以测试一直是绿的。
//
// 判定只认 East Asian Width 的 W（宽）与 F（全宽）两档，**Ambiguous 一律按 1 列**：
// 本机主用 Windows Terminal / conhost + 等宽字体（Cascadia Mono），
// `❯ ⚓ · … ●` 在里面都是 1 列。宁可偶尔少补一格空格（无害），
// 也不能多算（多算会把下一列的内容盖掉）。
//
// 反过来说：这套宽度只服务**布局**（居中缩进、裁到终端宽），不服务"像素级还原"。

import (
	"strings"
	"unicode/utf8"
)

// runeRange 是 [lo, hi] 闭区间。
type runeRange [2]rune

// wideRanges 是 East Asian Width = W/F 的常用区间（东亚文字、全角符号、emoji），
// 按 lo 升序排列（inRanges 走二分）。
//
// 不追求 Unicode 全集：这里是终端界面，真正会出现的"宽字符"就是中日韩文字、
// 全角标点和少量 emoji。漏一个的后果是多算/少算一列，肉眼不可见；
// 而把 Ambiguous 全算成 2 列的后果是整屏文字互相盖住。
var wideRanges = []runeRange{
	{0x1100, 0x115F},   // 谚文字母
	{0x2E80, 0x303E},   // 中日韩部首 … 中日韩符号和标点
	{0x3041, 0x33FF},   // 平假名/片假名/注音/中日韩兼容
	{0x3400, 0x4DBF},   // 中日韩统一表意文字扩展 A
	{0x4E00, 0x9FFF},   // 中日韩统一表意文字
	{0xA000, 0xA4CF},   // 彝文
	{0xA960, 0xA97F},   // 谚文字母扩展 A
	{0xAC00, 0xD7A3},   // 谚文音节
	{0xF900, 0xFAFF},   // 中日韩兼容表意文字
	{0xFE10, 0xFE19},   // 竖排标点
	{0xFE30, 0xFE6F},   // 中日韩兼容形式
	{0xFF00, 0xFF60},   // 全角 ASCII / 全角标点
	{0xFFE0, 0xFFE6},   // 全角符号
	{0x1F300, 0x1F64F}, // emoji 杂项符号与表情
	{0x1F900, 0x1F9FF}, // 补充符号与表意符号
	{0x20000, 0x2FFFD}, // 扩展 B 及以后
	{0x30000, 0x3FFFD},
}

// zeroRanges 是不可见/不占列的组合区（本端基本用不到；写了是为了不被算成 1 列）。
var zeroRanges = []runeRange{
	{0x0300, 0x036F}, // 组合用附加符号
	{0x200B, 0x200F}, // 零宽空格/方向标记
	{0x20D0, 0x20FF}, // 组合用符号
}

func inRanges(r rune, ranges []runeRange) bool {
	lo, hi := 0, len(ranges)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		switch {
		case r < ranges[mid][0]:
			hi = mid - 1
		case r > ranges[mid][1]:
			lo = mid + 1
		default:
			return true
		}
	}
	return false
}

// cellWidth 返回一个 rune 在终端上占几列。
func cellWidth(r rune) int {
	switch {
	case r == 0, r < 0x20, r == 0x7F:
		return 0 // 控制字符不可见
	case r < 0x7F:
		return 1
	case inRanges(r, zeroRanges):
		return 0
	case inRanges(r, wideRanges):
		return 2
	}
	return 1
}

// ansiLen 返回 s[i] 处一条 ANSI 转义序列的长度；不是转义序列返回 0。
//
// 只认 CSI（`ESC [ … 终止字节`）与 OSC（`ESC ] … BEL|ST`）——
// 这两类就是本端会输出的全部。
func ansiLen(s string, i int) int {
	if i >= len(s) || s[i] != 0x1b || i+1 >= len(s) {
		return 0
	}
	switch s[i+1] {
	case '[':
		for j := i + 2; j < len(s); j++ {
			if s[j] >= 0x40 && s[j] <= 0x7e { // 终止字节
				return j - i + 1
			}
		}
		return 0
	case ']':
		for j := i + 2; j < len(s); j++ {
			if s[j] == 0x07 {
				return j - i + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j - i + 2
			}
		}
		return 0
	}
	return 0
}

// displayWidth 是 s 在终端上占的列数（ANSI 转义序列不计）。
func displayWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if n := ansiLen(s, i); n > 0 {
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w += cellWidth(r)
		i += size
	}
	return w
}

// padRight 把 s 补到 w 列（只补不裁）。
func padRight(s string, w int) string {
	if gap := w - displayWidth(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// clipWidth 把一行裁/补到**恰好** w 个显示列。
//
// 这是全屏渲染唯一的"截断"入口（screen.Render 用它）。裁的时候整段 ANSI
// 照抄、不占列；裁断处补 `…`，并在有颜色时补一个 SGR 复位——不然最后一段的
// 颜色会漏到右边的空白上。
func clipWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if displayWidth(s) <= w {
		return s + strings.Repeat(" ", w-displayWidth(s))
	}
	return clipPlusEllipsis(s, w)
}

// clipPlusEllipsis 把内容裁到 w-1 列再补一个 `…`（宽 1 列），
// 于是结果恰好 w 列。宽字符放不进最后那一格时先补空格再落 `…`。
func clipPlusEllipsis(s string, w int) string {
	budget := w - 1
	used := 0
	styled := false
	var b strings.Builder
	for i := 0; i < len(s); {
		if n := ansiLen(s, i); n > 0 {
			b.WriteString(s[i : i+n])
			styled = true
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		cw := cellWidth(r)
		if used+cw > budget {
			break
		}
		b.WriteString(s[i : i+size])
		used += cw
		i += size
	}
	if used+1 < w {
		b.WriteString(strings.Repeat(" ", w-used-1))
	}
	b.WriteString("…")
	if styled {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

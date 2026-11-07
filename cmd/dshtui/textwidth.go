package main

// 显示宽度与折行：终端里"一格"不等于"一个 rune"。
//
// 三件事必须一次算对，否则后面每个排版函数都得各错一遍：
//   - CJK/全角字占 **2** 列。按 rune 算会让中文正文"看着窄、实际顶出右边界"，
//     一超宽就被 Screen.Render 硬切，切掉的正是句尾那几个字；
//   - ANSI 转义序列占 **0** 列。按字节算会把一行彩色字当成超宽行，
//     裁出半截转义序列——屏幕上剩下的是 `[38;2;…` 这样的垃圾，比少几个字更难认；
//   - 组合符、零宽字符占 **0** 列。
//
// 为什么单独成文件、单独可测：行宽决定了边框画多长、在哪里折行。
// 框画歪了在屏幕上只是"右边差一格"，却是几万行里最难归因的一类错。

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// ansiSeqLen 返回从 ESC 开始的一段转义序列的字节数。
//
// 认不出来就**只吞 ESC 一个字节**：多吞会把后面的正文吃掉，
// 少吞最坏只是多一列——两种错的代价不对称。
func ansiSeqLen(s string) int {
	if len(s) < 2 {
		return len(s)
	}
	switch s[1] {
	case '[': // CSI：参数 + 终止字节 0x40..0x7e
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
		return len(s)
	case ']': // OSC：以 BEL 或 ST 收尾
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return len(s)
	}
	return 1
}

// stripANSI 去掉转义序列。样式标记（0x01..0x0c）不是转义序列，
// 它们由 paint 处理——这里的职责只有"字节层面的转义"。
func stripANSI(s string) string {
	if strings.IndexByte(s, 0x1b) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i += ansiSeqLen(s[i:])
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// runeDispWidth 是一个可见字符占几列。
//
// 判据用 Unicode 的 East Asian Width（W/F 两档算 2），再加一条：
// 组合符（Mn/Me）不占位——不排掉的话，带声调的拉丁字母会被算成两格。
func runeDispWidth(r rune) int {
	if r < 0x20 || (r >= 0x7f && r < 0xa0) {
		return 0 // 控制字符（含样式标记）
	}
	if r == utf8.RuneError {
		return 1
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
	if inRanges(r, wideRanges) {
		return 2
	}
	return 1
}

// dispWidth 是字符串在屏幕上的列数：转义序列与样式标记都记 0。
func dispWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i += ansiSeqLen(s[i:])
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w += runeDispWidth(r)
		i += size
	}
	return w
}

// cell 是折行/截断的最小单位：一段**不可分**的字节 + 它占的列数。
//
// 转义序列与样式标记各自成一个 cell（宽度 0）：它们可以落在任何位置，
// 但不能被拦腰切开——切开的就是半截转义，屏幕上直接变乱码。
type cell struct {
	s string
	w int
}

func cellsOf(s string) []cell {
	out := make([]cell, 0, len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			n := ansiSeqLen(s[i:])
			out = append(out, cell{s: s[i : i+n]})
			i += n
			continue
		}
		if s[i] < 0x20 {
			out = append(out, cell{s: s[i : i+1]})
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		out = append(out, cell{s: s[i : i+size], w: runeDispWidth(r)})
		i += size
	}
	return out
}

func cellsWidth(cs []cell) int {
	w := 0
	for _, c := range cs {
		w += c.w
	}
	return w
}

func cellsString(cs []cell) string {
	var b strings.Builder
	for _, c := range cs {
		b.WriteString(c.s)
	}
	return b.String()
}

// truncDisp 把一行截到 w 列，被截时补一个 `…`（它自己也占一列）。
//
// 与 Screen.Render 的 fitWidth 同源：两处都按**显示列**裁，
// 否则一处在折行里裁、另一处按 rune 裁，中文行就会在最后一步被切坏。
func truncDisp(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if dispWidth(s) <= w {
		return s
	}
	limit := w - 1 // 给 `…` 留一列
	if limit < 0 {
		limit = 0
	}
	var kept []cell
	used := 0
	for _, c := range cellsOf(s) {
		if c.w == 0 {
			kept = append(kept, c)
			continue
		}
		if used+c.w > limit {
			break
		}
		kept = append(kept, c)
		used += c.w
	}
	var b strings.Builder
	b.WriteString(cellsString(kept))
	// 截断发生在行内样式中间时，把已经打开的样式逐个收掉；
	// 否则省略号之后的角色文本会继承上一段的 bold/颜色。
	open := openStyles(kept)
	for i := len(open) - 1; i >= 0; i-- {
		b.WriteString(styleEnd[open[i]])
	}
	if strings.Contains(cellsString(kept), "\x1b") {
		b.WriteString(ansiReset)
	}
	b.WriteString("…")
	return b.String()
}

// padDisp 补到恰好 w 列（超了不裁——调用方该先 truncDisp）。
func padDisp(s string, w int) string {
	if gap := w - dispWidth(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// styleEnd / styleStart 是样式标记的配对表（见 style.go 的常量）。
var (
	styleEnd   = map[string]string{stBold: stBoldEnd, stDim: stDimEnd, stAccent: stAccentEnd, stSubtle: stSubtleEnd, stOK: stOKEnd, stErr: stErrEnd}
	styleStart = map[string]string{stBoldEnd: stBold, stDimEnd: stDim, stAccentEnd: stAccent, stSubtleEnd: stSubtle, stOKEnd: stOK, stErrEnd: stErr}
)

// openStyles 返回 cells 里"开着还没关"的样式标记（按开启顺序）。
func openStyles(cs []cell) []string {
	var open []string
	for _, c := range cs {
		if _, ok := styleEnd[c.s]; ok {
			open = append(open, c.s)
			continue
		}
		if start, ok := styleStart[c.s]; ok && len(open) > 0 && styleEnd[open[len(open)-1]] == start {
			open = open[:len(open)-1]
		}
	}
	return open
}

// wrapDisp 按显示宽度折行。
//
// 规则只有两条，但要一起满足：
//   - 有空格就断在空格处（英文单词不被劈开）；
//   - 没有空格就按字符断（CJK 本来就可以在任意字之间折；长 URL/长 token
//     也只能这么断——**断在半路比溢出屏幕好**）。
//
// 折出来的每一行都会把跨行的样式在行尾收掉、在下一行重开：
// 不收的话，第二行起样式全丢（颜色只覆盖第一行，看起来像"掉色"）；
// 不重开的话，收掉之后就再也回不来。
func wrapDisp(s string, w int) []string {
	if w < 1 {
		w = 1
	}
	out := make([]string, 0, 4)
	for _, seg := range strings.Split(s, "\n") {
		out = append(out, wrapSegment(seg, w)...)
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

func wrapSegment(seg string, w int) []string {
	var lines []string
	var cur []cell
	curW := 0
	lastSpace := -1
	// carry 是"上一行结尾收掉、这一行要重开"的样式。
	carry := []string{}

	emit := func() {
		for len(cur) > 0 && cur[len(cur)-1].s == " " {
			cur = cur[:len(cur)-1]
		}
		var b strings.Builder
		for _, m := range carry {
			b.WriteString(m)
		}
		b.WriteString(cellsString(cur))
		// carry 已经在行首重开，必须参与"这一行结束时还开着什么"的计算；
		// 只看 cur 会让第二行忘收、第三行忘开，长 bold/code 从这里开始掉色。
		styledCells := make([]cell, 0, len(carry)+len(cur))
		for _, mark := range carry {
			styledCells = append(styledCells, cell{s: mark})
		}
		styledCells = append(styledCells, cur...)
		open := openStyles(styledCells)
		carry = open
		for i := len(open) - 1; i >= 0; i-- {
			b.WriteString(styleEnd[open[i]])
		}
		lines = append(lines, b.String())
		cur = nil
		curW = 0
		lastSpace = -1
	}

	for _, c := range cellsOf(seg) {
		if c.w == 0 {
			cur = append(cur, c)
			continue
		}
		if curW+c.w > w && curW > 0 {
			if lastSpace >= 0 {
				tail := append([]cell(nil), cur[lastSpace+1:]...)
				cur = cur[:lastSpace]
				emit()
				cur = tail
				curW = cellsWidth(tail)
				lastSpace = -1
			} else {
				emit()
			}
		}
		cur = append(cur, c)
		curW += c.w
		if c.s == " " {
			lastSpace = len(cur) - 1
		}
	}
	if len(cur) > 0 {
		emit()
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	return lines
}

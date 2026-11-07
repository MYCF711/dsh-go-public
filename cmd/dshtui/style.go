package main

// 一行屏幕文字的样式模型，以及消息正文里的轻量样式标记。
//
// 首屏/角色颜色由 span 表达；Markdown 的行内 code/bold/strike、代码框和工具状态
// 由 0x01..0x0c 与 0x0e..0x0f 标记表达。两者在 styledLine 这一层汇合：plain 用于快照，
// ansi 用于真屏。布局因此不需要猜文本前缀，也不会把控制字节写进转储。

import (
	"fmt"
	"os"
	"strings"
)

// color 是一个真彩色；零值表示"未设"（不输出转义，用终端默认色）。
type color struct {
	r, g, b uint8
	set     bool
}

func (c color) fgSeq() string {
	if !c.set {
		return ""
	}
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.r, c.g, c.b)
}

func (c color) bgSeq() string {
	if !c.set {
		return ""
	}
	return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", c.r, c.g, c.b)
}

// span 是一段同一角色样式的文字。
type span struct {
	text    string
	fg      color
	bg      color
	bold    bool
	dim     bool
	italic  bool
	reverse bool
}

type styledLine []span

func textLine(s string) styledLine { return styledLine{{text: s}} }
func blankLine(n int) styledLine   { return textLine(strings.Repeat(" ", n)) }

// 消息层的行内标记。控制字符占零显示列；它们永远不会进入纯文本快照。
// 0x0d 是回车，不能拿来做行内标记，因此 strike 从 0x0e 开始。
const (
	stBold      = "\x01"
	stBoldEnd   = "\x02"
	stDim       = "\x03"
	stDimEnd    = "\x04"
	stAccent    = "\x05"
	stAccentEnd = "\x06"
	stSubtle    = "\x07"
	stSubtleEnd = "\x08"
	stOK        = "\x09"
	stOKEnd     = "\x0a"
	stErr       = "\x0b"
	stErrEnd    = "\x0c"
	stStrike    = "\x0e"
	stStrikeEnd = "\x0f"
)

const ansiReset = "\x1b[0m"

func isStyleMark(c byte) bool {
	return c >= 1 && c <= 12 || c >= 14 && c <= 15
}

func hasStyleMark(s string) bool {
	for i := 0; i < len(s); i++ {
		if isStyleMark(s[i]) {
			return true
		}
	}
	return false
}

// textwidth.go 的折行/截断表是包级共享注册表；在这里登记新样式，
// 长删除线才会在每行重开/收掉，截断时也不会把 SGR 9 泄漏到后续正文。
func init() {
	styleEnd[stStrike] = stStrikeEnd
	styleStart[stStrikeEnd] = stStrike
}

// terminalStrikeSupported 只声明有依据的能力。SGR 9 是默认能力；
// TERM=dumb、NO_COLOR（本端因此走纯文本）以及显式关闭都走可见降级。
// DSH_TUI_STRIKETHROUGH=1/0 可供终端实测后强制覆盖自动判断。
func terminalStrikeSupported() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DSH_TUI_STRIKETHROUGH"))) {
	case "1", "true", "on", "yes":
		return true
	case "0", "false", "off", "no":
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb")
}

// stripStyleMarks 是快照、搜索与断言共用的纯文本口径。
func stripStyleMarks(s string) string {
	if !hasStyleMark(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if isStyleMark(s[i]) {
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// plainMessageLine 是快照与搜索的统一纯文本口径：内部样式标记与输入里
// 原有的 ANSI 都不占字、也不该漏进无颜色转储。
func plainMessageLine(s string) string {
	return stripANSI(stripStyleMarks(s))
}

// plain 去掉角色样式与消息标记，返回这一行的文字。
func (l styledLine) plain() string {
	if len(l) == 1 {
		return plainMessageLine(l[0].text)
	}
	var b strings.Builder
	for _, s := range l {
		b.WriteString(plainMessageLine(s.text))
	}
	return b.String()
}

func (l styledLine) width() int { return displayWidth(l.plain()) }

func (l styledLine) styled() int {
	n := 0
	for _, s := range l {
		if s.fg.set || s.bg.set || s.bold || s.dim || s.italic || s.reverse || hasStyleMark(s.text) {
			n++
		}
	}
	return n
}

// spanStyleSeq 是角色样式的 SGR（不含复位）。
func spanStyleSeq(s span) string {
	seq := s.fg.fgSeq() + s.bg.bgSeq()
	if s.bold {
		seq += "\x1b[1m"
	}
	if s.dim {
		seq += "\x1b[2m"
	}
	if s.italic {
		seq += "\x1b[3m"
	}
	if s.reverse {
		seq += "\x1b[7m"
	}
	return seq
}

// messageMarkSeq 把消息开始标记映射到当前主题。结束标记由活动栈统一重算，
// 因而行内 code 结束后会回到角色的正文色，而不是硬编码的深色主题色。
func messageMarkSeq(mark byte, theme themeTokens) string {
	switch mark {
	case 1:
		return "\x1b[1m"
	case 3:
		return "\x1b[2m"
	case 5:
		return theme.accent.fgSeq()
	case 7:
		return theme.subtle.fgSeq()
	case 9:
		return theme.success.fgSeq()
	case 11:
		return theme.error.fgSeq()
	case 14:
		return "\x1b[9m"
	}
	return ""
}

func updateMarkStack(active []byte, mark byte) []byte {
	markText := string(mark)
	if _, opens := styleEnd[markText]; opens {
		return append(active, mark)
	}
	start, closes := styleStart[markText]
	if !closes {
		return active
	}
	for i := len(active) - 1; i >= 0; i-- {
		if active[i] == start[0] {
			return append(active[:i], active[i+1:]...)
		}
	}
	return active
}

// renderMarkedSpan 在同一段角色样式里解释嵌套的消息标记。
func renderMarkedSpan(s span) string {
	base := spanStyleSeq(s)
	theme := currentTheme()
	var b strings.Builder
	b.Grow(len(s.text) + 32)
	b.WriteString(ansiReset)
	b.WriteString(base)
	active := make([]byte, 0, 4)
	for i := 0; i < len(s.text); i++ {
		c := s.text[i]
		if isStyleMark(c) {
			active = updateMarkStack(active, c)
			b.WriteString(ansiReset)
			b.WriteString(base)
			for _, mark := range active {
				b.WriteString(messageMarkSeq(mark, theme))
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// ansi 把这一行拼成带转义的字符串，并在行尾复位，防止颜色泄漏。
func (l styledLine) ansi() string {
	var b strings.Builder
	styled := false
	for _, s := range l {
		if spanStyleSeq(s) == "" && !hasStyleMark(s.text) {
			b.WriteString(s.text)
			continue
		}
		b.WriteString(renderMarkedSpan(s))
		styled = true
	}
	if styled {
		b.WriteString(ansiReset)
	}
	return b.String()
}

func (l styledLine) render(withColor bool) string {
	if withColor {
		return l.ansi()
	}
	return l.plain()
}

func (l styledLine) appendSpan(text string, fg color, bold bool) styledLine {
	return append(l, span{text: text, fg: fg, bold: bold})
}

// truncStyledLine 按终端显示列截断，同时保留每个 span 的渐变/角色颜色。
// 首屏文字列与鲸鱼拼完后才决定剩余宽度，因此必须在组合行出口处理，
// 不能只截 cwd 字符串（model/tip 或下一个真实字段同样可能超长）。
func truncStyledLine(l styledLine, w int) styledLine {
	if w <= 0 {
		return styledLine{}
	}
	if l.width() <= w {
		return l
	}
	out := styledLine{}
	remaining := w
	for _, s := range l {
		if remaining <= 0 {
			break
		}
		sw := dispWidth(s.text)
		if sw == 0 {
			out = append(out, s)
			continue
		}
		if sw <= remaining {
			out = append(out, s)
			remaining -= sw
			continue
		}
		s.text = truncDisp(s.text, remaining)
		out = append(out, s)
		break
	}
	return out
}

// paint 保留消息层原有的单行入口；颜色同样走 styledLine，避免两套映射。
func paint(s string, withColor bool) string {
	return styledLine{{text: s}}.render(withColor)
}

func paintLines(lines []string, withColor bool) []string {
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = paint(ln, withColor)
	}
	return out
}

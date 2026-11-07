package main

// 全屏渲染：进 alt-screen、按"行数组"整屏重画、输出时把光标放回去。
//
// 为什么先整屏重画而不是差分：差分要维护"上一帧每一格是什么"，
// 只要有一处算错就会留下残影，而残影比闪一下难查得多。
// 先整屏画（够快：一屏 30 行），把正确性钉住，diff 是后面的事。

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Screen 是一块可重画的全屏区域。
type Screen struct {
	// out 是 io.Writer 而不是 *os.File：终端层（alt-screen 进/出、整行清屏、裁宽、
	// 差分重画）是唯一没有测试覆盖的一层，而它坏了的症状是"界面花掉/键没反应"——
	// 单测看不见。抽成 Writer 之后这些 ANSI 字节可以在测试里逐字节断言。
	out    io.Writer
	width  int
	height int
	inAlt  bool
	// prev 是上一帧的每一行。差分渲染靠它：**只重画变了的行**。
	// 整屏重画在 30 行时够快，但流式输出每 100ms 一帧就会看出闪，
	// 而且终端越慢（Windows 控制台）越明显。
	prev []string
	// prevTitle 是进 alt-screen 之前的窗口标题。退出时必须还回去：
	// 否则关掉 TUI 后那个控制台窗口永远顶着产品名，人以为它还活着。
	prevTitle string
}

func NewScreen() *Screen {
	w, h := termSize()
	return &Screen{out: os.Stdout, width: w, height: h}
}

// newScreenTo 给测试用：把输出接到任意 Writer，尺寸自己定。
func newScreenTo(out io.Writer, width, height int) *Screen {
	return &Screen{out: out, width: width, height: height}
}

// Enter 进 alt-screen、隐藏光标，并把窗口标题换成产品名。
//
// 标题走两条路，是故意的：
//   - `\x1b]0;…\x07`（OSC 0）是终端世界的标准做法，Windows Terminal / 其他终端都认；
//   - `setConsoleTitle` 是 conhost 的 API，`tasklist /v` 与任务管理器读的就是它。
//
// 只做前者，某些终端/控制台组合下标题不变；只做后者，非 Windows 上等于没做。
func (s *Screen) Enter() {
	if !enableVT() {
		// 打不开 VT 时不要硬来：直接告诉人原因，比满屏乱码有用。
		fmt.Fprintln(os.Stderr, "这个终端不支持 ANSI 转义（Win10 以前的控制台）；请用 Windows Terminal")
		os.Exit(1)
	}
	s.enterAlt()
}

// enterAlt 是 Enter 的字节部分，单独一件是因为它可测：
// Enter 会先探测真终端（探测失败直接 os.Exit），测试不能碰那条路。
func (s *Screen) enterAlt() {
	s.inAlt = true
	s.prevTitle = currentTitle()
	// 1000=按钮事件，1003=任意移动（hover），1006=SGR 坐标；Leave 成对关闭。
	fmt.Fprint(s.out, "\x1b[?1049h\x1b[?25l\x1b[2J\x1b[?1000h\x1b[?1003h\x1b[?1006h"+oscTitle(productName))
	setConsoleTitle(productName)
}

// Leave 恢复原屏幕、光标与窗口标题。**必须幂等**：异常退出路径也会调它。
func (s *Screen) Leave() {
	if !s.inAlt {
		return
	}
	s.inAlt = false
	// prevTitle 为空（非 Windows，或终端的标题读不出来）时只吐退出序列；
	// 吐一个空的 OSC 会把人家的标题清掉，那比不恢复更糟。
	out := "\x1b[?1006l\x1b[?1003l\x1b[?1000l\x1b[?25h\x1b[?1049l"
	if s.prevTitle != "" {
		out += oscTitle(s.prevTitle)
	}
	fmt.Fprint(s.out, out)
	setConsoleTitle(s.prevTitle)
}

// Resize 更新尺寸（每帧开始前调用，处理窗口变化）。
func (s *Screen) Resize() {
	s.width, s.height = termSize()
}

// Render 把若干行画满整屏：第 0 行 = 顶部。
//
// 超宽的行按显示宽度硬切（点阵与正文都是等宽假设），
// 少行就补空行——**不清屏不重画**会让上一帧的尾巴留在下面。
//
// 差分：与上一帧**逐字节相同**的行不重画，只用 `\x1b[<行>;1H` 把光标挪过去。
// 为什么值得：流式输出每 ~100ms 一帧，整屏重画在慢终端（Windows 控制台）上看得见闪。
//
// 以前这个字段（prev）只是**声明了却没人用**，而 `TestRenderSkipsUnchangedRows`
// 在测试里把"哪些行变了"自己算了一遍——测的是测试自己的算术，不是渲染器。
// 现在差分真的在这里，测试也调这个函数。
func (s *Screen) Render(lines []string) {
	next := make([]string, s.height)
	for i := 0; i < s.height; i++ {
		if i < len(lines) {
			if i == s.height-1 {
				// 最后一行固定是输入编辑器：ZWJ emoji 要按一个 grapheme
				// 计宽，不能被通用 rune 宽度裁成省略号。
				next[i] = fitInputWidth(lines[i], s.width)
			} else {
				next[i] = fitWidth(lines[i], s.width)
			}
		}
	}
	var b strings.Builder
	for i := 0; i < s.height; i++ {
		if i < len(s.prev) && s.prev[i] == next[i] {
			// 这一行没变：只定位，不清不写。
			b.WriteString("\x1b[")
			b.WriteString(strconv.Itoa(i + 1))
			b.WriteString(";1H")
			continue
		}
		if i == 0 {
			b.WriteString("\x1b[H")
		} else {
			b.WriteString("\x1b[")
			b.WriteString(strconv.Itoa(i + 1))
			b.WriteString(";1H")
		}
		b.WriteString("\x1b[2K") // 清整行
		b.WriteString(next[i])
	}
	s.prev = next
	fmt.Fprint(s.out, b.String())
}

// fitWidth 把一行裁/补到恰好 w 个**显示列**（见 width.go）。
// CJK、组合符与 ANSI 都由同一套列宽逻辑处理，避免终端自行折行。
func fitWidth(s string, w int) string {
	return clipWidth(s, w)
}

package main

// 单行草稿编辑器：文本、grapheme 光标与水平视口。
//
// 上游 PromptInput 用 Intl.Segmenter 保证方向键/删除/渲染都站在同一个
// grapheme 边界上。Go 标准库没有同等的 segmenter，这里覆盖终端草稿需要的
// 规则：组合附加符、variation selector、emoji modifier、区域旗、keycap、
// tag 序列与 ZWJ 序列。光标存储为 rune 下标，但永远落在这些边界上。

import (
	"strings"
	"unicode"
)

// inputCluster 是 [start,end) 的一段不可分文本及其真实终端列宽。
type inputCluster struct {
	start int
	end   int
	width int
}

const inputZWJ = '\u200d'

func inputIsExtend(r rune) bool {
	return unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) || unicode.Is(unicode.Me, r) ||
		r >= 0xfe00 && r <= 0xfe0f || // variation selectors
		r >= 0xe0100 && r <= 0xe01ef || // supplementary variation selectors
		r >= 0x1f3fb && r <= 0x1f3ff || // emoji skin-tone modifiers
		r >= 0xe0020 && r <= 0xe07f // emoji tag sequences + cancel tag
}

func inputIsRegionalIndicator(r rune) bool { return r >= 0x1f1e6 && r <= 0x1f1ff }

func inputHasRune(rs []rune, want rune) bool {
	for _, r := range rs {
		if r == want {
			return true
		}
	}
	return false
}

// inputClusterWidth 是“一个 grapheme 占几列”，不是把 ZWJ 两侧的 emoji
// 各算一次。普通组合字符取最大字符宽；emoji/ZWJ/旗/keycap 按 2 列。
func inputClusterWidth(rs []rune) int {
	if len(rs) == 0 {
		return 0
	}
	if len(rs) >= 2 && inputIsRegionalIndicator(rs[0]) && inputIsRegionalIndicator(rs[1]) {
		return 2
	}
	emoji := inputHasRune(rs, 0xfe0f) || inputHasRune(rs, 0x20e3)
	maxWidth := 0
	for _, r := range rs {
		if r >= 0x1f000 && r <= 0x1faff {
			emoji = true
		}
		if w := runeDispWidth(r); w > maxWidth {
			maxWidth = w
		}
	}
	if emoji {
		return 2
	}
	return maxWidth
}

// inputClusters 返回升序 grapheme 边界。空输入返回空切片；光标末尾由
// len(clusters) 表示，因此不需要额外虚构一个文本 cell。
func inputClusters(rs []rune) []inputCluster {
	out := make([]inputCluster, 0, len(rs))
	for i := 0; i < len(rs); {
		start := i
		i++
		// 一面区域旗是两个 regional indicator，而不是两个独立光标站。
		if inputIsRegionalIndicator(rs[start]) && i < len(rs) && inputIsRegionalIndicator(rs[i]) {
			i++
		}
		for i < len(rs) {
			if inputIsExtend(rs[i]) {
				i++
				continue
			}
			if rs[i] == inputZWJ && i+1 < len(rs) {
				// ZWJ 与下一个基础字符同属当前 grapheme；下一轮会吸收它的
				// combining/VS/modifier，并继续处理后续 ZWJ。
				i += 2
				continue
			}
			break
		}
		out = append(out, inputCluster{start: start, end: i, width: inputClusterWidth(rs[start:i])})
	}
	return out
}

func inputClustersWidth(cs []inputCluster) int {
	w := 0
	for _, c := range cs {
		w += c.width
	}
	return w
}

// inputStringWidth 给输入行和 Screen.Render 使用的 grapheme 显示列宽。
// ANSI/内部样式标记先剥离；它们不占终端格。
func inputStringWidth(s string) int {
	return inputClustersWidth(inputClusters([]rune(plainMessageLine(s))))
}

// fitInputWidth 只用于屏幕最后的输入行。普通正文仍走 fitWidth；输入行
// 必须按 grapheme 宽度补齐，否则一个 ZWJ 家庭 emoji 会被旧 rune 宽度误判
// 为超宽并切成省略号。
func fitInputWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	width := inputStringWidth(s)
	if width > w {
		// inputRow 自身保证不发生这条分支；保留硬裁作为最后一道终端防折行。
		return clipWidth(s, w)
	}
	return s + strings.Repeat(" ", w-width)
}

// inputCaretPosition 返回当前光标（rune 下标）。旧测试和外部构造直接写
// a.input 时 inputCaretSet=false，保持历史上“光标在末尾”的零值语义。
func (a *app) inputCaretPosition() int {
	if !a.inputCaretSet {
		return len(a.input)
	}
	caret := a.inputCaret
	if caret < 0 {
		caret = 0
	}
	if caret > len(a.input) {
		caret = len(a.input)
	}
	for _, c := range inputClusters(a.input) {
		if caret < c.end {
			return c.start
		}
	}
	return len(a.input)
}

// inputCaretCluster 返回光标所在的边界序号：[0,len(clusters)]。
func inputCaretCluster(cs []inputCluster, caret int) int {
	for i, c := range cs {
		if caret < c.end {
			return i
		}
	}
	return len(cs)
}

// inputViewportCapacity 是按键后同步视口使用的近似容量；真正渲染时
// inputRow 会针对 1/2 列终端重新计算前缀，因此窄屏仍由纯函数兜底。
func (a *app) inputViewportCapacity() int {
	if a.scr == nil {
		return 1
	}
	capacity := a.scr.width - displayWidth(markerUser+" ")
	if capacity < 1 {
		return 1
	}
	return capacity
}

// inputWindow 计算首个可见 grapheme 与光标边界。它不改 app：layout/view
// 仍是纯函数，终端缩到一半时也不会在渲染里偷偷改状态。
func (a *app) inputWindow(capacity int) (start, caret int, clusters []inputCluster) {
	if capacity < 1 {
		capacity = 1
	}
	clusters = inputClusters(a.input)
	caret = inputCaretCluster(clusters, a.inputCaretPosition())
	start = a.inputScroll
	if start < 0 {
		start = 0
	}
	if start > len(clusters) {
		start = len(clusters)
	}
	if caret < start {
		start = caret
	}
	if caret == len(clusters) {
		// 末尾要有一个反显空格，故文本容量少一列。
		for start < caret && inputClustersWidth(clusters[start:caret])+1 > capacity {
			start++
		}
		return start, caret, clusters
	}
	// 光标在文本上时，它自己的完整 grapheme 必须入窗。若终端连这个
	// grapheme 都放不下，保留 start=caret，由 inputRow 画一列反显光标。
	if clusters[caret].width > capacity {
		return caret, caret, clusters
	}
	for start < caret && inputClustersWidth(clusters[start:caret+1]) > capacity {
		start++
	}
	return start, caret, clusters
}

func (a *app) syncInputViewport() {
	start, _, _ := a.inputWindow(a.inputViewportCapacity())
	a.inputScroll = start
}

func (a *app) setInputCaret(caret int) {
	if caret < 0 {
		caret = 0
	}
	if caret > len(a.input) {
		caret = len(a.input)
	}
	// 吸附到 <=caret 的 grapheme 边界，任何调用方都不能落进组合字符。
	a.inputCaret = caret
	a.inputCaretSet = true
	a.inputCaret = a.inputCaretPosition()
	a.syncInputViewport()
}

func (a *app) clearInput() {
	a.input = nil
	a.inputCaret = 0
	a.inputCaretSet = false
	a.inputScroll = 0
}

func (a *app) insertInputRune(r rune) {
	caret := a.inputCaretPosition()
	a.input = append(a.input, 0)
	copy(a.input[caret+1:], a.input[caret:len(a.input)-1])
	a.input[caret] = r
	a.setInputCaret(caret + 1)
}

func (a *app) deleteInputRange(start, end int) bool {
	if start < 0 || end > len(a.input) || start >= end {
		return false
	}
	copy(a.input[start:], a.input[end:])
	a.input = a.input[:len(a.input)-(end-start)]
	a.setInputCaret(start)
	return true
}

func (a *app) deleteInputBackward() bool {
	caret := a.inputCaretPosition()
	if caret == 0 {
		return false
	}
	clusters := inputClusters(a.input)
	at := inputCaretCluster(clusters, caret)
	if at == 0 {
		return false
	}
	return a.deleteInputRange(clusters[at-1].start, caret)
}

func (a *app) deleteInputForward() bool {
	caret := a.inputCaretPosition()
	clusters := inputClusters(a.input)
	at := inputCaretCluster(clusters, caret)
	if at >= len(clusters) {
		return false
	}
	return a.deleteInputRange(caret, clusters[at].end)
}

func inputWordBoundaryLeft(rs []rune, caret int) int {
	for caret > 0 && unicode.IsSpace(rs[caret-1]) {
		caret--
	}
	for caret > 0 && !unicode.IsSpace(rs[caret-1]) {
		caret--
	}
	return caret
}

func inputWordBoundaryRight(rs []rune, caret int) int {
	for caret < len(rs) && !unicode.IsSpace(rs[caret]) {
		caret++
	}
	for caret < len(rs) && unicode.IsSpace(rs[caret]) {
		caret++
	}
	return caret
}

package main

// 代码块边框：移植上游 `src/components/CodeBlockFrame.tsx` 的**轻边框**。
//
// 上游的形状（typed 引擎，默认样式）：
//
//	┌─ ts ─────────────────────
//	│   const answer = await agent.run()
//	│   return answer
//
// 三条约定原样搬过来：
//   - 只有**上边**和**左边**：没有右边墙、没有下边。理由写在源码注释里——
//     "a block costs no extra rows"，正文区一行都别浪费；
//   - 头部只有角、语言标签、和一条分隔线；正文每行带竖线 + 一列内边距；
//   - 太窄（净宽 < 8）就退回普通 ANSI 围栏，硬画框只会把代码挤死。
//
// 与上游不同的一处（在 docs/tui-message.md 里标成 derived）：超过 codeFoldRows
// 行就折起来，提示写明还有多少行。上游的代码块不折（它靠整页滚动），本端沿用
// 既有 Ctrl+O 长内容开关；没有复制行为，因此不画 `copy` 控件或提示。

import "strings"

// 与上游 CodeBlockFrame 同值的常量。
const (
	// codeFrameMinBody 是"还画得下框"的净正文宽度（上游 MIN_NET_BODY_WIDTH）。
	codeFrameMinBody = 8
	// codeFrameWideHeader 是"头部拉满分隔线"的最小内容宽度（上游 WIDE_HEADER_MIN_COLUMNS）。
	codeFrameWideHeader = 56
	// codeFrameOverhead 是竖线 + 一列内边距。
	codeFrameOverhead = 2
)

// renderCodeFrameLines 把一个围栏块排成若干行。
//
// lang 为空时按上游用 `code` 兜底：没有标签的框右边会空一截，
// 而"这里是个代码块"本身就是要说的话。
func renderCodeFrameLines(lang, body string, width int) []mdLine {
	body = strings.TrimRight(body, "\n")
	label := lang
	if label == "" {
		label = "code"
	}
	net := width - codeFrameOverhead
	if net < codeFrameMinBody {
		return plainFenceLines(lang, body, width)
	}

	out := make([]mdLine, 0, 8)
	out = append(out, mdLine{text: codeHeader((label), width)})
	if body == "" {
		return out // 空围栏只有头（上游同样处理）
	}

	// 正文：源文每一行按净宽再折（超长行不折就会顶穿右边）。
	rank := 0
	for _, srcLine := range strings.Split(body, "\n") {
		for _, ln := range wrapDisp(srcLine, net) {
			out = append(out, mdLine{
				text: codeRail() + ln,
				meta: lineMeta{capRank: rank, capLimit: codeFoldRows},
			})
			rank++
		}
	}
	if rank > codeFoldRows {
		out = append(out, mdLine{
			text: codeRail() + stDim + foldHint(rank-codeFoldRows) + stDimEnd,
			meta: lineMeta{capHint: rank - codeFoldRows},
		})
	}
	return out
}

// codeHeader 画上游的 `┌─ 语言`；内容宽度达到 56 列后再拉满分隔线。
// 语言标签本身也按显示列截断：长标签再稀罕，也不能把右边顶穿。
func codeHeader(label string, width int) string {
	if width <= 0 {
		return ""
	}
	labelWidth := width - len("┌─ ")
	if labelWidth < 0 {
		labelWidth = 0
	}
	label = truncDisp(label, labelWidth)
	left := stSubtle + "┌─ " + label + stSubtleEnd
	if width < codeFrameWideHeader {
		return truncDisp(left, width)
	}
	left += stSubtle + " "
	fill := width - dispWidth(left)
	if fill < 0 {
		fill = 0
	}
	return truncDisp(left+stSubtle+strings.Repeat("─", fill)+stSubtleEnd, width)
}

func codeRail() string { return stSubtle + "│" + stSubtleEnd + " " }

// plainFenceLines 是窄终端的退路：上游在净宽不够时退回普通围栏，
// 不画框（框会把代码挤成每行四五个字）。
func plainFenceLines(lang, body string, width int) []mdLine {
	open := stSubtle + "```" + lang + stSubtleEnd
	out := []mdLine{{text: open}}
	if body == "" {
		return out
	}
	rank := 0
	for _, srcLine := range strings.Split(body, "\n") {
		for _, ln := range wrapDisp(srcLine, width-2) {
			out = append(out, mdLine{
				text: "  " + ln,
				meta: lineMeta{capRank: rank, capLimit: codeFoldRows},
			})
			rank++
		}
	}
	if rank > codeFoldRows {
		out = append(out, mdLine{
			text: "  " + stDim + foldHint(rank-codeFoldRows) + stDimEnd,
			meta: lineMeta{capHint: rank - codeFoldRows},
		})
	}
	return out
}

// foldHint 是折叠提示的文字，与上游 i18n 的 `lines-folded-expand` 同形：
// `… +{{n}} 行（{{key}} 展开）`。
func foldHint(hidden int) string {
	return "… +" + foldNumber(hidden) + " 行（Ctrl+O 展开）"
}

// foldNumber 是给小整数用的：整个文件里只出现在提示语里，用 strconv 更啰嗦。
func foldNumber(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

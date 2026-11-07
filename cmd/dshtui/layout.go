package main

// 全屏排版：这一屏长什么样，只在这里决定。
//
// 为什么把 `layout()` 抽出来而不是留在 view() 里：布局要同时产出**纯文字**
// （测试断言、`--dump-screen` 快照）和**带样式**（真屏）。以前只有纯文字，
// 上色就只能事后按前缀猜"这行是谁说的"——而"猜"在模型正文恰好以 `❯ ` 开头时
// 会误伤。现在角色住在 lineMeta 里，布局按角色一次画好两种形态。
//
// 行数契约（唯一一条，别的地方都不许破）：layout() 返回**恰好** scr.height 行，
// 顺序是：正文 … / 分隔横线 / 上下文条 / 状态行 / 输入行。
// 少一行 → 底部残留上一帧；多一行 → 终端滚屏、整屏错位。

import (
	"fmt"
	"math"
	"strings"
)

func (a *app) render() {
	a.scr.Resize()
	rows := a.layout()
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = r.render(a.color)
	}
	a.scr.Render(lines)
}

// view 是**纯函数**：给定状态，返回这一屏每一行的**纯文字**。
//
// 快照与真屏同源：view() 只是 layout() 去掉样式，
// 所以"快照里有、屏幕上没有"这种分叉在结构上不存在。
func (a *app) view() []string {
	rows := a.layout()
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.plain()
	}
	return out
}

// layout 是"这一屏长什么样"的**唯一**实现。
//
// 折叠与搜索只影响"取哪几行"和状态行的文字，不改变行数。
func (a *app) layout() []styledLine {
	theme := currentTheme()
	body := a.bodyHeight()
	rows := make([]styledLine, 0, a.scr.height)

	// 正文：先按折叠状态过滤，再按滚动位置取一屏。
	// 过滤在取窗口**之前**做：否则折叠的行会占掉本该给其它内容的格子，
	// 也会让"离底部多远"按看不见的行来算。
	idx := a.visibleIndex()
	total := len(idx)

	// max 是能往回看的上限：内容比屏幕短就是 0（滚不动）。
	maxScroll := 0
	if total > body {
		maxScroll = total - body
	}
	// 这里按 maxScroll 夹一次而不是直接信任 a.scroll：layout 不假设状态干净
	// （测试、以及 maxLines 裁剪都可能让 scroll 暂时越界）。
	scroll := a.scroll
	if scroll < 0 {
		scroll = 0
	}
	if scroll > maxScroll {
		scroll = maxScroll
	}

	switch {
	case body <= 0:
		// 终端矮到没有正文区：只画 footer（多画一行就会整屏滚一格）。
	case total == 0:
		// 空会话：正文区就是首屏（鲸鱼 + 大字 + 版本/模型/目录/提示 + 欢迎语）。
		// 上游把它叫 header；本端在"还一句话都没有"时把它铺在输入框上方。
		rows = append(rows, splashFirstScreen(a.splashInfo(), body)...)
	default:
		end := total - scroll
		start := end - body
		if start < 0 {
			start = 0
		}
		for k := start; k < end; k++ {
			rows = append(rows, a.bodyRow(idx[k], theme))
		}
	}
	for len(rows) < body {
		rows = append(rows, textLine(""))
	}

	// footer 四行：分隔横线 / 上下文条 / 状态行 / 输入行。
	//
	// 顺序是上游 footer 的顺序（条在上、状态在下），只是本端把输入行放在**最后**
	// ——那是这个界面从第一版起的约定（人往最下面看输入）。
	rule := styledLine{{text: strings.Repeat("─", a.scr.width), fg: theme.subtle}}
	bar := a.contextBarRow(theme)
	status := a.statusRow(theme, scroll, total)
	input := a.inputRow(theme)
	switch h := a.scr.height; {
	case h <= 0:
		return rows
	case h == 1:
		// 只剩一行：留输入行（能打字是底线）。
		rows = append(rows, input)
	case h == 2:
		rows = append(rows, status, input)
	case h == 3:
		// 先丢上下文条：它是装饰，横线是"正文与输入的分界"。
		rows = append(rows, rule, status, input)
	default:
		rows = append(rows, rule, bar, status, input)
	}
	// 弹层只覆盖正文/装饰区，状态行与输入行始终保留；关闭后恢复同一布局。
	return a.statusPopupLayout(rows)
}

// bodyRow 把第 i 行正文按**角色**排版：加前缀、上色。
//
// 前缀的意义与上游一致：`❯` 用户、`●` 助手、`⚓` 思考。续行只缩进不重复前缀——
// 每行都画一遍 `●` 会让一段话看起来像好几段。
func (a *app) bodyRow(i int, theme themeTokens) styledLine {
	text := a.lines[i]
	var meta lineMeta
	if i < len(a.meta) {
		meta = a.meta[i]
	}
	const contIndent = "  "
	indented := truncDisp(text, a.scr.width-len(contIndent))
	full := truncDisp(text, a.scr.width)
	switch meta.role {
	case roleUser:
		return styledLine{
			{text: markerUser + " ", fg: theme.userLabel, bold: true},
			{text: indented, fg: theme.text, bold: true},
		}
	case roleUserCont:
		return styledLine{{text: contIndent}, {text: indented, fg: theme.text}}
	case roleAssistant:
		return styledLine{
			{text: markerAssistant + " ", fg: theme.accent, bold: true},
			{text: indented, fg: theme.text},
		}
	case roleAssistantCont:
		return styledLine{{text: contIndent}, {text: indented, fg: theme.text}}
	case roleThinking:
		hint := "（Ctrl+O 展开）"
		if a.expandAll {
			hint = "（Ctrl+O 收起）"
		}
		prefix := markerThinking + " "
		labelWidth := a.scr.width - displayWidth(prefix) - displayWidth(hint)
		return styledLine{
			{text: prefix, fg: theme.inactive},
			{text: truncDisp(text, labelWidth), fg: theme.inactive, italic: true},
			{text: hint, fg: theme.inactive, dim: true},
		}
	case roleThinkingBody:
		return styledLine{{text: contIndent}, {text: indented, fg: theme.inactive, italic: true, dim: true}}
	case roleTool:
		return styledLine{{text: full, fg: theme.toolName}}
	case roleToolResult:
		fg := theme.inactive
		if meta.isError {
			fg = theme.error
		}
		return styledLine{{text: full, fg: fg}}
	case roleError:
		return styledLine{{text: full, fg: theme.error}}
	}
	return textLine(full)
}

// splashInfo 把首屏要的真实字段收成一个值。
func (a *app) splashInfo() splashInfo {
	// 每次渲染都取当时的本地日期，跨过午夜无需重启。兼容缝同时刷新给
	// animation_app.go 的可见性/命中测试；字体只改变文字列，不改鲸鱼状态机。
	face := currentSplashFace(a.clock())
	splashFace = face
	return splashInfo{
		columns: a.scr.width,
		model:   a.model,
		effort:  a.effort,
		cwd:     a.cwd,
		tip:     a.tipText(),
		face:    face,
		sprite:  a.currentWhaleSprite(),
	}
}

// tipText 是提示行那一条（空表时给空串，不编一句话）。
func (a *app) tipText() string {
	if len(splashTips) == 0 {
		return ""
	}
	i := a.tipIdx % len(splashTips)
	if i < 0 {
		i = 0
	}
	return splashTips[i]
}

// contextBarRow 是上下文条：已用段 + 空余段，读数右对齐在空余段里。
//
// 几何照上游 StatusMetrics 的 contextBarColumns：**占用率决定已用/空余的分界**，
// 分段明细只决定已用段内部怎么分。本端没有 content 明细
// （Transport 的 `session.load` 不提供），所以整段已用都是同一个颜色——
// 这正是上游"只有占用率、没有构成"的那条分支（contextBarUsedColor）。
//
// 两种"不知道"分开处理，因为含义不同：
//   - 用量未知（还没跑过一轮）→ 空条 + 读数 `?`
//   - 终端太窄（< 14 列）→ 整条不画，但**行数不变**（留空行，版面不跳）
func (a *app) contextBarRow(theme themeTokens) styledLine {
	width := a.scr.width
	if width < 14 {
		return textLine("")
	}

	usedWidth := 0
	pct := 0.0
	known := a.ctxTokens > 0 && a.ctxWindow > 0
	if known {
		ratio := float64(a.ctxTokens) / float64(a.ctxWindow)
		if ratio > 1 {
			ratio = 1
		}
		pct = ratio * 100
		usedWidth = int(math.Round(float64(width) * ratio))
		if usedWidth > width {
			usedWidth = width
		}
	}

	row := styledLine{}
	if usedWidth > 0 {
		// 一段纯色块：没有文字，颜色就是它的含义（上游同样只有底色）。
		row = append(row, span{text: strings.Repeat(" ", usedWidth), bg: theme.barAssistant})
	}

	free := width - usedWidth
	if free <= 0 {
		return row
	}
	// 读数按压力换色：≥80% 琥珀、≥95% 红，其余用空余段的文字色。
	fg := theme.barFreeText
	switch {
	case pct >= 95:
		fg = theme.error
	case pct >= 80:
		fg = theme.warning
	}
	row = append(row, span{text: rightAlignBarText(barReadings(a.ctxTokens, a.ctxWindow), free), fg: fg, bg: theme.barFreeFill})
	return row
}

// barReadings 是上下文条里那段读数的候选，从长到短。
//
// 上游 contextBarReadout 给的是一份"数字+百分比"和一份"只剩百分比"：
// 空余段窄到放不下长的，就退到短的——而不是让读数溢出到已用段上。
func barReadings(used, window int) []string {
	if used <= 0 || window <= 0 {
		return []string{"?"}
	}
	pct := float64(used) / float64(window) * 100
	return []string{
		fmt.Sprintf("%s/%s %.1f%%", fmtTokens(used), fmtTokens(window), pct),
		fmt.Sprintf("%.1f%%", pct),
	}
}

// rightAlignBarText 把第一条放得下的候选右对齐到 width 列，结果**恰好** width 列。
func rightAlignBarText(readings []string, width int) string {
	if width <= 0 {
		return ""
	}
	for _, r := range readings {
		if w := displayWidth(r); w <= width {
			return strings.Repeat(" ", width-w) + r
		}
	}
	return strings.Repeat(" ", width)
}

// statusRow 是状态行：左边是"这一轮在用什么、花了多少"，右边是会话身份。
//
// 顺序照上游 footer 的左组：模型 · effort · 用量 · 工具数 · 速率 · 状态。
// 产品名不在这行里——窗口标题（OSC 0）与 `--version` 已经带着它，
// 而这一行每一列都更该拿去显示"现在是什么状态"。
//
// 右边的 `会话 <id>` 放不下就整段不画（先牺牲身份，不牺牲状态）：
// 人看这一行是为了知道"跑没跑、花了多少"。
func (a *app) statusRow(theme themeTokens, scroll, visible int) styledLine {
	row, _ := a.buildStatusRow(theme, scroll, visible)
	return row
}

// inputRow 是输入行：前缀 + 水平视口内的 grapheme + 反显光标。
//
// 光标在文本中间时反显整个 grapheme；在末尾时反显一个空格。长单行不再
// 从头部截成省略号，而是跟随光标左右滚动。CJK/emoji 以显示列入窗，ZWJ
// 序列绝不从中间切开。
func (a *app) inputRow(theme themeTokens) styledLine {
	width := a.scr.width
	if width <= 0 {
		return styledLine{}
	}
	prompt := markerUser + " "
	promptColor := theme.userLabel
	if a.searching {
		prompt = "/ "
		promptColor = theme.accent
	}
	// 1 列只留光标；2 列留单字符前缀与光标。正常尺寸仍用 `❯ ` / `/ `。
	if width == 1 {
		prompt = ""
	} else if width == 2 {
		prompt = string([]rune(prompt)[0])
	}
	capacity := width - displayWidth(prompt)
	if capacity < 1 {
		capacity = 1
	}
	start, caret, clusters := a.inputWindow(capacity)
	row := styledLine{}
	if prompt != "" {
		row = append(row, span{text: prompt, fg: promptColor, bold: true})
	}
	used := 0
	caretDrawn := false
	for i := start; i < len(clusters); i++ {
		c := clusters[i]
		if used+c.width > capacity {
			break
		}
		isCaret := i == caret
		row = append(row, span{
			text: string(a.input[c.start:c.end]), fg: theme.text, reverse: isCaret,
		})
		used += c.width
		if isCaret {
			caretDrawn = true
		}
	}
	if !caretDrawn && used+1 <= capacity {
		// 末尾光标，或 1 列终端放不下一个宽 grapheme 时的可见降级。
		row = append(row, span{text: " ", reverse: true})
	}
	return row
}

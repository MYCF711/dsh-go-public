package main

// 状态字段的鼠标命中、hover/click 详情层，以及状态行与 hit box 的共同几何。
// 详情只读 app 已有的 session.load / tool/call / connectionEvent 状态；
// 不请求 picker、不构造工具目录，也不把缺失的 context breakdown 编出来。

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type statusField uint8

const (
	statusFieldNone statusField = iota
	statusFieldModel
	statusFieldEffort
	statusFieldContext
	statusFieldTools
	statusFieldSession
)

func (f statusField) String() string {
	switch f {
	case statusFieldModel:
		return "model"
	case statusFieldEffort:
		return "effort"
	case statusFieldContext:
		return "context"
	case statusFieldTools:
		return "tools"
	case statusFieldSession:
		return "session"
	default:
		return ""
	}
}

// statusHitBox 使用终端 1-based 行号 y 与 0-based 半开列区间 [x0,x1)。
// 只有真实渲染得出来的字段才产生 box；被特殊状态替换或完全裁掉的字段不隐形接单。
type statusHitBox struct {
	field  statusField
	x0, x1 int
	y      int
}

func (b statusHitBox) contains(x, y int) bool {
	col := x - 1
	return y == b.y && col >= b.x0 && col < b.x1
}

type statusPopupState struct {
	field  statusField
	sid    string
	pinned bool // false=hover 预览；true=点击固定，leave 不收
}

// buildStatusRow 同时产出画面与字段几何，避免另写一套坐标在截断后漂移。
// 无弹层时它与原 statusRow 的文字、顺序、颜色和裁宽规则保持一致。
func (a *app) buildStatusRow(theme themeTokens, scroll, visible int) (styledLine, []statusHitBox) {
	model := a.model
	if model == "" {
		model = "?"
	}
	effort := a.effort
	if effort == "" {
		effort = "?"
	}
	left := ""
	boxes := []statusHitBox{}
	add := func(text string) { left += text }
	addField := func(field statusField, text string) {
		start := displayWidth(left)
		left += text
		boxes = append(boxes, statusHitBox{field: field, x0: start, x1: start + displayWidth(text)})
	}
	shiftBoxes := func(delta int) {
		for i := range boxes {
			boxes[i].x0 += delta
			boxes[i].x1 += delta
		}
	}

	addField(statusFieldModel, model)
	add(" · ")
	addField(statusFieldEffort, effort+" effort")
	// 状态紧跟模型：这一行窄终端上会被按列截断，而"跑没跑、能不能打字"
	// 比后面那些常驻指标更要紧——把它放到行尾，截断时第一个牺牲的就是它。
	if a.promptInputLocked() {
		add(" · 发送中 · 草稿只读")
	} else if a.busy {
		// 已用时长只累计连接可用的活跃段；断线墙钟不计入，重连也不清零。
		add(" · 运行中")
		if d := a.runningElapsed(); d >= time.Second {
			add(" " + fmtDuration(d))
		}
		if a.connectionDown {
			add(" · 断线暂停")
		}
	} else if a.status != "" {
		add(" · " + a.status)
	}
	add(" · ")
	addField(statusFieldTools, fmt.Sprintf("工具 %d", a.tools))
	add(" · ")
	addField(statusFieldContext, "ctx "+ctxPercent(a.ctxTokens, a.ctxWindow))
	if a.tps > 0 {
		add(fmt.Sprintf(" · %.1f tok/s", a.tps))
	}
	if a.outTokens > 0 {
		add(fmt.Sprintf(" · 累计 out %s", fmtTokens(a.outTokens)))
	}
	if !a.showToolResults && visible < len(a.lines) {
		// 有被折叠的行才提示，没有就不占地方。
		add(" · 结果已折叠 Ctrl+T")
	}
	tone := theme.inactive
	if a.searching {
		// 搜索时状态行只说搜索的事：命中数放在最前面，窄终端也不会被截掉。
		if a.searchQuery == "" {
			left = " 搜索：输入关键词 · Esc 退出"
		} else {
			left = fmt.Sprintf(" 搜索「%s」· 匹配 %d 处 · Esc 退出",
				trunc(a.searchQuery, 20), matchCount(a.visibleLines(), a.searchQuery))
		}
		boxes = nil
		tone = theme.accent
	}
	if a.pending.id != "" {
		left = a.approvalStatus()
		boxes = nil
		tone = theme.warning
	} else if a.err != "" {
		left = " ! " + a.err
		boxes = nil
		tone = theme.error
	}
	if scroll > 0 {
		// N 是回到底部还要过的行数，和 PgDn/Ctrl+End 使用同一口径。
		prefix := " " + fmt.Sprintf("↑ 回看中（还有 %d 行）· PgDn/Ctrl+End 回到底部", scroll) + " · "
		left = prefix + strings.TrimLeft(left, " ")
		shiftBoxes(displayWidth(prefix))
		tone = theme.accent
	}
	left = " " + strings.TrimLeft(left, " ")
	shiftBoxes(1)

	// 右边：会话短 id。放不下就不画，也不留下不可见 hit box。
	right := ""
	if a.sid != "" {
		right = "会话 " + short(a.sid) + " "
	}
	gap := a.scr.width - displayWidth(left) - displayWidth(right)
	clipBoxes := func() []statusHitBox {
		out := make([]statusHitBox, 0, len(boxes))
		for _, b := range boxes {
			if b.x0 < 0 {
				b.x0 = 0
			}
			if b.x1 > a.scr.width {
				b.x1 = a.scr.width
			}
			if b.x1 > b.x0 {
				out = append(out, b)
			}
		}
		return out
	}
	if gap < 1 {
		return styledLine{{text: clipWidth(left, a.scr.width), fg: tone}}, clipBoxes()
	}
	rightStart := displayWidth(left) + gap
	boxes = append(boxes, statusHitBox{
		field: statusFieldSession, x0: rightStart, x1: rightStart + displayWidth(right),
	})
	return styledLine{
		{text: left, fg: tone},
		{text: strings.Repeat(" ", gap)},
		{text: right, fg: theme.subtle},
	}, clipBoxes()
}

// statusMouseTargets 重算当前画面的目标，不缓存上一帧宽度或滚动状态。
func (a *app) statusMouseTargets() []statusHitBox {
	if a.scr == nil || a.scr.height < 2 {
		return nil
	}
	total := a.visibleCount()
	scroll := a.scroll
	if scroll < 0 {
		scroll = 0
	}
	if maxScroll := total - a.bodyHeight(); scroll > maxScroll {
		if maxScroll < 0 {
			scroll = 0
		} else {
			scroll = maxScroll
		}
	}
	_, boxes := a.buildStatusRow(currentTheme(), scroll, total)
	for i := range boxes {
		boxes[i].y = a.scr.height - 1 // 状态行的 1-based 行号
	}
	if a.scr.height >= 4 && a.scr.width >= 14 {
		// 上游 context bar 同样响应 hover；本端复用同一真实 context 详情。
		boxes = append(boxes, statusHitBox{
			field: statusFieldContext, x0: 0, x1: a.scr.width, y: a.scr.height - 2,
		})
	}
	return boxes
}

func (a *app) statusFieldAt(x, y int) (statusField, bool) {
	for _, b := range a.statusMouseTargets() {
		if b.contains(x, y) {
			return b.field, true
		}
	}
	return statusFieldNone, false
}

// parseStatusMouse 读取既有 keyName 已完整隔离的 SGR 参数。
// M 是按下/移动，m 是 release；bit 32 是 motion，低两位是按钮。
func parseStatusMouse(seq string) (code, x, y int, press, motion, ok bool) {
	if len(seq) < 2 || seq[0] != '<' || (seq[len(seq)-1] != 'M' && seq[len(seq)-1] != 'm') {
		return 0, 0, 0, false, false, false
	}
	parts := strings.Split(seq[1:len(seq)-1], ";")
	if len(parts) != 3 {
		return 0, 0, 0, false, false, false
	}
	var err error
	code, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, false, false, false
	}
	x, errX := strconv.Atoi(parts[1])
	y, errY := strconv.Atoi(parts[2])
	if errX != nil || errY != nil || x < 1 || y < 1 {
		return 0, 0, 0, false, false, false
	}
	press = seq[len(seq)-1] == 'M'
	motion = press && code&32 != 0
	return code, x, y, press, motion, true
}

func (a *app) activeStatusPopup() *statusPopupState {
	p := a.statusPopup
	if p == nil || p.sid != a.sid || p.field.String() == "" {
		return nil
	}
	return p
}

func (a *app) closeStatusPopup() bool {
	if a.statusPopup == nil {
		return false
	}
	a.statusPopup = nil
	return true
}

func (a *app) canShowStatusPopup() bool {
	// 至少留一行正文/装饰区给详情，绝不盖掉状态行与输入行。
	return a.scr != nil && a.scr.width >= 2 && a.scr.height >= 3
}

// handleStatusSpecial 返回 true 表示状态层已消费。非状态区左键仍返回 false，
// 让既有鲸鱼点击继续命中；release、滚轮和未知按钮绝不切换弹层。
func (a *app) handleStatusSpecial(key string) bool {
	if !strings.HasPrefix(key, "mouse:") {
		return false
	}
	code, x, y, press, motion, ok := parseStatusMouse(strings.TrimPrefix(key, "mouse:"))
	if !ok {
		return false
	}
	if motion {
		field, hit := a.statusFieldAt(x, y)
		p := a.activeStatusPopup()
		if !hit {
			if p != nil && !p.pinned {
				a.closeStatusPopup()
			}
			return true
		}
		if !a.canShowStatusPopup() {
			return true
		}
		if p == nil || !p.pinned {
			a.statusPopup = &statusPopupState{field: field, sid: a.sid}
		}
		return true
	}
	if !press || code&3 != 0 || code&64 != 0 {
		return false
	}
	field, hit := a.statusFieldAt(x, y)
	if !hit {
		if p := a.activeStatusPopup(); p != nil && !p.pinned {
			a.closeStatusPopup()
		}
		return false
	}
	if !a.canShowStatusPopup() {
		return true
	}
	p := a.activeStatusPopup()
	if p != nil && p.field == field && p.pinned {
		a.closeStatusPopup()
		return true
	}
	if p != nil && p.field == field {
		p.pinned = true // hover 后第一次点击是固定，不是“再次点击”
		return true
	}
	a.statusPopup = &statusPopupState{field: field, sid: a.sid, pinned: true}
	return true
}

func statusTextOrUnknown(text string) string {
	if strings.TrimSpace(text) == "" {
		return "未知"
	}
	return text
}

func statusIntOrUnknown(n int) string {
	if n <= 0 {
		return "未知"
	}
	return strconv.Itoa(n)
}

func (a *app) statusToolCountText() string {
	if a.tools < 0 {
		return "未知"
	}
	return strconv.Itoa(a.tools)
}

func (a *app) statusConnectionText() string {
	if a.connectionDown {
		return "已断开，正在重连"
	}
	if a.connectionSubscription > 0 && a.connectionGeneration > 0 {
		return "已连接"
	}
	return "未知（尚未收到连接事件）"
}

// statusPopupDetails 每次渲染重新读取当前状态，发送、断线和统计刷新不会留下旧值。
func (a *app) statusPopupDetails(field statusField) []string {
	sid := "session id: " + statusTextOrUnknown(a.sid)
	connection := "连接状态: " + a.statusConnectionText()
	switch field {
	case statusFieldModel:
		return []string{sid, "model: " + statusTextOrUnknown(a.model), connection, "来源: session.load"}
	case statusFieldEffort:
		return []string{sid, "effort: " + statusTextOrUnknown(a.effort), "model: " + statusTextOrUnknown(a.model), connection,
			"来源: session.load；缺失或空值就是未知"}
	case statusFieldContext:
		return []string{sid, "contextTokens: " + statusIntOrUnknown(a.ctxTokens),
			"contextWindow: " + statusIntOrUnknown(a.ctxWindow), connection, "来源: session.load；未知不填 0"}
	case statusFieldTools:
		return []string{sid, "tool count: " + a.statusToolCountText(), connection,
			"来源: 当前会话已接收 tool/call 帧；不是宿主工具目录"}
	case statusFieldSession:
		return []string{sid, "model: " + statusTextOrUnknown(a.model), "effort: " + statusTextOrUnknown(a.effort),
			"contextTokens: " + statusIntOrUnknown(a.ctxTokens), "contextWindow: " + statusIntOrUnknown(a.ctxWindow),
			"tool count: " + a.statusToolCountText(), connection}
	default:
		return nil
	}
}

// statusPanelText 画一条恰好 width 列的边框/内容行；1/2 列也有确定降级。
func statusPanelText(left, right, fill, content string, width int) string {
	if width <= 0 {
		return ""
	}
	if width == 1 {
		return clipWidth(content, 1)
	}
	inner := width - 2
	body := truncDisp(content, inner)
	return left + body + strings.Repeat(fill, inner-displayWidth(body)) + right
}

// statusPopupLayout 把详情画进已有行数组，行数不变，并永久保留最后两行。
func (a *app) statusPopupLayout(rows []styledLine) []styledLine {
	p := a.activeStatusPopup()
	if p == nil || !a.canShowStatusPopup() {
		return rows
	}
	details := a.statusPopupDetails(p.field)
	if len(details) == 0 {
		return rows
	}
	width, height := a.scr.width, a.scr.height
	available := height - 2
	panelWidth := width
	if panelWidth > 72 {
		panelWidth = 72
	}
	left := (width - panelWidth) / 2
	theme := currentTheme()
	title := "状态详情 · " + p.field.String()
	hint := "移出字段关闭 · 点击固定"
	if p.pinned {
		hint = "Esc / 再次点击关闭"
	}

	panel := []string{}
	if available <= 2 {
		first := title
		if len(details) > 0 {
			first += " · " + details[0]
		}
		panel = append(panel, clipWidth(first, panelWidth))
		if available == 2 {
			panel = append(panel, clipWidth(hint, panelWidth))
		}
	} else {
		capLines := available - 2
		if len(details) > capLines {
			details = details[:capLines]
		}
		panel = append(panel, statusPanelText("┌", "┐", "─", " "+title+" ", panelWidth))
		for _, line := range details {
			panel = append(panel, statusPanelText("│", "│", " ", " "+line, panelWidth))
		}
		panel = append(panel, statusPanelText("└", "┘", "─", " "+hint+" ", panelWidth))
	}
	top := (available - len(panel)) / 2
	if top < 0 {
		top = 0
	}
	for i, line := range panel {
		row := top + i
		if row < 0 || row >= len(rows) || row >= available {
			continue
		}
		rows[row] = styledLine{
			{text: strings.Repeat(" ", left)},
			{text: line, fg: theme.accent, bg: theme.barFreeFill},
		}
	}
	return rows
}

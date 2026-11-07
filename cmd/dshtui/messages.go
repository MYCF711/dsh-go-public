package main

// 消息层接线：把 Transport 的帧变成**排版好的行**。
//
// 这一层的边界很重要：上面是"帧是什么"（handleFrame 的分派），
// 下面是"这一行长什么样"（markdown/codeframe/toolcard/thinking 四个纯函数）。
// 接线的部分只管三件事——顺序、折叠元信息、以及"哪些字段没有就不许编"。

import (
	"encoding/json"
	"strings"
	"time"
)

// contentWidth 是正文可用的列数。
//
// 零值保护是给"测试里直接构造的 app"用的：宽度取不到时按 80 列排，
// 而不是排成 1 列（那会把每个字都折成一行，测试会以最莫名其妙的方式失败）。
func (a *app) contentWidth() int {
	w := 0
	if a.scr != nil {
		w = a.scr.width
	}
	if w < 20 {
		w = 80
	}
	return w
}

// pushUserMessage 画用户说的话。
//
// 用户消息**不折成 markdown**：他说的是字，不是文档。折了会把 `*` 之类的
// 原文记号吃掉，而人回头核对"我当时到底发了什么"时，那正是要看的东西。
func (a *app) pushUserMessage(text string) {
	width := a.contentWidth() - 2
	if width < 8 {
		width = 8
	}
	lines := wrapDisp(strings.TrimRight(text, "\n"), width)
	for i, ln := range lines {
		role := roleUserCont
		if i == 0 {
			role = roleUser
		}
		a.pushMeta(ln, lineMeta{role: role})
	}
	a.push("")
}

// assistantBlock 是 assistant/message 里的一个内容块。
//
// 只认 text 与 reasoning：tool-call 块由 tool/call 帧负责（两处都画会重复），
// 别的类型（图片等）这一版不渲染——**不渲染就什么都不加**，
// 不拿占位符假装"这里有东西"。
type assistantBlock struct {
	kind string // "text" | "reasoning"
	text string
}

// parseAssistantBlocks 按**原顺序**读出内容块。
//
// 三种落法都要认（与 extractText 的历史一致）：
//   - `{"text":"…","reasoning":"…"}`：宿主推帧的形状（只有思考、没有正文时
//     reasoning 是唯一的内容）；
//   - `{"message":{"content":[…]}}`：v4 事件的形状；
//   - `{"content":[…]}`：WS 夹具的形状。
//
// 为什么按顺序：思考在前、正文在后是模型的真实产出顺序。把思考统一挪到最前
// 或最后，回看时"它想完才说"这层信息就没了。
func parseAssistantBlocks(data json.RawMessage) []assistantBlock {
	var m struct {
		Text      string `json:"text"`
		Reasoning string `json:"reasoning"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	if len(m.Message.Content) > 0 {
		return blocksFromContent(m.Message.Content)
	}
	if len(m.Content) > 0 {
		return blocksFromContent(m.Content)
	}
	var out []assistantBlock
	if strings.TrimSpace(m.Reasoning) != "" {
		out = append(out, assistantBlock{kind: "reasoning", text: m.Reasoning})
	}
	if m.Text != "" {
		out = append(out, assistantBlock{kind: "text", text: m.Text})
	}
	return out
}

func blocksFromContent(raw json.RawMessage) []assistantBlock {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	out := make([]assistantBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "reasoning":
			if strings.TrimSpace(b.Text) != "" {
				out = append(out, assistantBlock{kind: "reasoning", text: b.Text})
			}
		case "text":
			if b.Text != "" {
				out = append(out, assistantBlock{kind: "text", text: b.Text})
			}
		}
	}
	return out
}

// extractUserText 取 user/message 的正文（三种落法同上）。
func extractUserText(data json.RawMessage) string {
	var m struct {
		Text    string `json:"text"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	if m.Text != "" {
		return strings.TrimRight(m.Text, "\n")
	}
	if s := joinContent(m.Message.Content); s != "" {
		return strings.TrimRight(s, "\n")
	}
	return strings.TrimRight(joinContent(m.Content), "\n")
}

// pushAssistant 处理一条完整的 assistant/message。Transport 这里不是 token 增量，
// 所以消息结束也是 reasoning-only 块的落定边界；不能攒到下一条消息才显示。
func (a *app) pushAssistant(f ServerFrame) {
	durationMs := assistantReasoningDuration(f.Data)
	for _, blk := range parseAssistantBlocks(f.Data) {
		switch blk.kind {
		case "reasoning":
			a.addThinking(blk.text)
			if durationMs > 0 {
				a.think.duration = time.Duration(durationMs) * time.Millisecond
				a.think.hasDuration = true
			}
		case "text":
			// 正文到了 = 思考结束了。先把思考结算成一行，再画正文。
			a.flushThinking(f.Time)
			a.pushAssistantText(blk.text)
		}
	}
	// reasoning-only 也是一条完整回答（serve_turn.go 明确会推这种形状）。
	a.flushThinking(f.Time)
}

// assistantReasoningDuration 只认通道显式记录的 durationMs。当前 Transport
// 不带它时就不显示时长；整轮耗时、单个时间戳都不能冒充推理耗时。
func assistantReasoningDuration(data json.RawMessage) int64 {
	var m struct {
		DurationMs          int64 `json:"durationMs"`
		ReasoningDurationMs int64 `json:"reasoningDurationMs"`
	}
	_ = json.Unmarshal(data, &m)
	if m.ReasoningDurationMs > 0 {
		return m.ReasoningDurationMs
	}
	return m.DurationMs
}

// pushAssistantText 画助手正文：`● ` 两列 + markdown 正文。
//
// 与上游 AssistantTextMessage 一致：正文整体缩进两列，`●` 只出现在第一行，
// 续行是空格——这样"这段是一个人的一整段话"在视觉上是一块，
// 而不是每行前面都顶一个点。
func (a *app) pushAssistantText(text string) {
	width := a.contentWidth() - 2
	if width < 8 {
		width = 8
	}
	lines := renderMarkdownLines(strings.TrimRight(text, "\n"), width)
	for i, ln := range lines {
		ln.meta.role = roleAssistantCont
		if i == 0 {
			ln.meta.role = roleAssistant
		}
		a.pushMeta(ln.text, ln.meta)
	}
	a.push("")
}

// extractCallID 取调用 id。三种名字都认（会话帧里叫法不一），取不到就是空——
// 空 id 不会让结果挂错卡，只会退回"最后一张还挂着的卡"。
func extractCallID(data json.RawMessage) string {
	var m struct {
		CallID     string `json:"callId"`
		ToolCallID string `json:"toolCallId"`
		ID         string `json:"id"`
		Message    struct {
			CallID     string `json:"callId"`
			ToolCallID string `json:"toolCallId"`
		} `json:"message"`
	}
	_ = json.Unmarshal(data, &m)
	for _, id := range []string{m.CallID, m.ToolCallID, m.ID, m.Message.CallID, m.Message.ToolCallID} {
		if id != "" {
			return id
		}
	}
	return ""
}

// pushToolCall 画一张工具卡的头一行。
func (a *app) pushToolCall(f ServerFrame) {
	name, title := extractTool(f.Data)
	c := &toolCard{callID: extractCallID(f.Data), name: name, title: title, hdr: -1}
	// 状态只从帧里来：这一轮在跑（busy）时，call 帧就是"它正在跑"的证据；
	// 不是在跑（重放历史）时**不下结论**——那可能只是结果帧被裁掉了。
	if a.busy {
		c.status = toolRunning
	} else {
		c.status = toolUnknown
	}
	a.tools++
	a.cards = append(a.cards, c)
	if len(a.cards) > 64 {
		// 卡片只用于"结算时回头改口"，不需要无限留着；64 张足够覆盖
		// 一轮里还挂着的调用（裁剪后更老的卡已经被正文裁剪掉了）。
		a.cards = a.cards[len(a.cards)-64:]
	}
	c.hdr = len(a.lines)
	a.pushMeta(renderToolHeader(c, a.contentWidth()), lineMeta{role: roleTool})
}

// pushToolResult 结算一张工具卡：改头、排正文。
func (a *app) pushToolResult(f ServerFrame) {
	res := parseToolResult(f.Data)
	c := a.cardForResult(f)
	if c == nil {
		// 没有对应的 call（重放只留了结果）：也要画出来。
		// 但**不冒充**"有个调用"——名字就叫"未知工具"，状态用帧里的事实。
		c = &toolCard{name: extractToolName(f.Data), hdr: -1}
		a.cards = append(a.cards, c)
		c.status = toolOK
		if res.isError {
			c.status = toolErr
		}
		c.hdr = len(a.lines)
		a.pushMeta(renderToolHeader(c, a.contentWidth()), lineMeta{role: roleTool})
	}

	if res.isError {
		c.status = toolErr
	} else {
		c.status = toolOK
	}
	if res.hasDuration {
		c.duration = time.Duration(res.durationMs) * time.Millisecond
		c.hasDuration = true
	}
	// 两个 ServerFrame.time 的差包含排队、审批与宿主调度，不等于工具执行耗时。
	// 没有显式 durationMs 就保持未知，不能把帧间隔画成 `· Ns`。
	c.body = make([]mdLine, 0, len(res.lines))
	for _, ln := range res.lines {
		c.body = append(c.body, mdLine{text: ln})
	}
	a.redrawCard(c)
	for _, ln := range renderToolBody(c, a.contentWidth()) {
		ln.meta.role = roleToolResult
		a.pushMeta(ln.text, ln.meta)
	}
}

// cardForResult 找这张结果属于哪张卡。
//
// 有 id 就按 id；没有就取**最后一张还没结算**的卡。取"最后一张"而不是
// "第一张"，是因为工具是串行跑完一个接一个的——结果总是属于最近那一次调用。
func (a *app) cardForResult(f ServerFrame) *toolCard {
	id := extractCallID(f.Data)
	if id != "" {
		for i := len(a.cards) - 1; i >= 0; i-- {
			if a.cards[i].callID == id {
				return a.cards[i]
			}
		}
	}
	for i := len(a.cards) - 1; i >= 0; i-- {
		if a.cards[i].status == toolRunning || a.cards[i].status == toolUnknown {
			return a.cards[i]
		}
	}
	return nil
}

// extractToolName 只取名字（结果帧可能也带 name，带就带上）。
func extractToolName(data json.RawMessage) string {
	name, _ := extractTool(data)
	return name
}

// redrawCard 把一张卡的头部重画成当前状态。
//
// hdr 是**行号**而不是指针：正文是统一的一维行数组（滚动/搜索/折叠都按行算），
// 单独给卡片留一块内存会让这三样各多一条分支。行号在裁头时会跟着调整（见 pushMeta）。
func (a *app) redrawCard(c *toolCard) {
	if c.hdr < 0 || c.hdr >= len(a.lines) {
		return
	}
	a.lines[c.hdr] = renderToolHeader(c, a.contentWidth())
}

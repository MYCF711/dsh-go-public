package main

// 思考行：移植上游 `src/components/messages/AssistantThinkingMessage.tsx`
// 的**落定折叠行**（本地参考里的 `ThinkingToggle.tsx` 是 `/thinking` 那个设置
// 对话框，不是转录里的折叠行；真正的折叠行在 AssistantThinkingMessage，
// 是从 raw.githubusercontent.com 取的完整文件，sha256 见 docs/tui-message.md）。
//
// 上游落定行的形状：
//
//	⚓ 思考 · 12s （Ctrl+O 展开）
//
// 照搬的部分：
//   - 一个锚点符号 + `思考` + 真实耗时 + 展开提示；
//   - 耗时**只从通道记录的 durationMs 来**，且 < 1s 不显示（上游同样判据：
//     0.3 秒的"思考"显示出来只会让人以为界面在抖）；
//   - 展开后是完整的思考正文。
//
// 不做的部分：流式旋转指针（80ms 一帧的动画在我们的重画模型里要额外
// 维护一帧一槽的状态，而这一版的目标是"真实数据先对"）、token 估算
// （上游用 `~N tokens` 估，我们**不估**——编出来的数字比没有更糟）。

import (
	"strings"
	"time"
)

// renderThinkingLines 排一个思考块：第一行永远是折叠行，正文在它后面。
//
// 返回的正文带 meta.thinking：默认不显示，Ctrl+O 展开。
// 折叠是**显示层**的决定，不是删行——思考正文留在 lines 里，
// 展开时不重放、不重新计数。
func renderThinkingLines(text string, width int, dur time.Duration, hasDur bool) []mdLine {
	// layout 负责 ⚓ 与 Ctrl+O 提示；这里只保留真实标签和时长。
	width -= 2
	if width < 8 {
		width = 8
	}
	label := "思考"
	if hasDur && dur >= time.Second {
		label += " · " + shortDur(dur)
	}
	out := []mdLine{{text: truncDisp(label, width), meta: lineMeta{role: roleThinking}}}

	body := renderMarkdownLines(strings.TrimRight(text, "\n"), width)
	for _, ln := range body {
		if ln.text == "" {
			continue
		}
		ln.meta.role = roleThinkingBody
		ln.meta.thinking = true
		out = append(out, ln)
	}
	return out
}

// thinkingBuf 是当前完整 assistant/message 里的思考块。正文在消息末尾落定；
// 可选时长只来自显式 durationMs，不从帧时间或整轮 wall-clock 反推。
type thinkingBuf struct {
	text        string
	open        bool
	duration    time.Duration
	hasDuration bool
}

func (a *app) addThinking(text string) {
	if !a.think.open {
		a.think.open = true
		a.think.text = ""
		a.think.duration = 0
		a.think.hasDuration = false
	}
	if a.think.text != "" {
		a.think.text += "\n"
	}
	a.think.text += text
}

// flushThinking 把攒下的思考结算成一行（正文作为默认折叠的续行）。
//
// 没有显式 durationMs 就只写"思考"，不拿整个回合或两帧间隔冒充思考时长。
func (a *app) flushThinking(endMs int64) {
	if !a.think.open {
		return
	}
	_ = endMs // 保留帧边界参数，明确它不参与时长计算。
	for _, ln := range renderThinkingLines(a.think.text, a.contentWidth(), a.think.duration, a.think.hasDuration) {
		a.pushMeta(ln.text, ln.meta)
	}
	a.think = thinkingBuf{}
}

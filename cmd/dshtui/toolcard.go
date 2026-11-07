package main

// 工具卡：移植上游 `src/components/messages/AssistantToolUseMessage.tsx` 的
// **头部 + 折叠正文**。本地 key 参考是 55 行截断件；完整版另从
// raw.githubusercontent.com 取得（1103 行），两者 sha256 都记在 docs/tui-message.md。
//
// 上游的形状：
//
//	● bash 列出目录                    · 1.2s · 成功
//	   ⎿ 退出码 0，输出 12 字节
//
// 照搬的部分：
//   - 头部一行：状态点（`●` 进行中 / `•` 已结算 / `✗` 失败，两列宽）+ 工具名
//     （加粗、按类别着色）+ 一行摘要 + 耗时；
//   - 正文挂在 ` ⎿ ` 头/空续行的悬挂缩进下，默认只留 3 行
//     （上游 TEXT_BODY_MAX_LINES），多的折起来并写明还有几行；
//
// derived：失败正文用错误色且**不参与折叠**（保留本端既有契约；
// 上游会让长错误也参与三行折叠，本端选择不藏失败）。
//
// 不做的部分（写成"未做"，不假装有）：diff 面板、文件跳转、语法高亮、
// 运行中的实时输出。这些都要结构化 view（ToolCallView/ToolResultView），
// 而 Transport 现在给的是文本 —— 没有数据就不画，绝不按工具名猜一个样子。

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// toolBodyRows 是工具正文的折叠阈值（上游 TEXT_BODY_MAX_LINES = 3）。
const toolBodyRows = 3

// toolStatus 是工具调用的状态。**只从帧里来**，绝不按"看起来像完成了"猜。
type toolStatus int

const (
	// toolUnknown：只见过 tool/call，不知道这一轮还开不开
	// （断线重放、被裁掉的历史都可能只留一半）。
	toolUnknown toolStatus = iota
	// toolRunning：call 帧到了、result 还没到，且这一轮还在跑。
	toolRunning
	// toolOK：收到了 result 帧且不是错误。
	toolOK
	// toolErr：收到了 result 帧且是错误。
	toolErr
	// toolAborted：这一轮结束了（turn/done），但始终没收到 result。
	// 单独一档是因为"没结果"和"成功"长得一样会很危险——
	// 人会把一条没有输出的调用当成"跑成功了"。
	toolAborted
)

// toolCard 是一张工具卡的全部事实。
type toolCard struct {
	callID string
	name   string
	title  string
	status toolStatus

	// duration 只在 result 显式带 durationMs 时有值；没有就不显示。
	duration    time.Duration
	hasDuration bool

	// body 是结果正文（已排版，默认只显示前 toolBodyRows 行）。
	body []mdLine

	// hdr 是这张卡头部行在 a.lines 里的下标（-1 = 已被裁掉）。
	// 状态要到 result 帧甚至 turn/done 才知道，而头早在 call 帧就画出来了——
	// 所以得记住它在哪一行，到结算时把那一行重画。
	hdr int
}

// toolDot 是状态点，两列（字形 + 空格），与上游 ToolUseLoader 的
// minWidth=2 对齐：结算前后标签不横跳。
func toolDot(st toolStatus) string {
	switch st {
	case toolErr:
		return stErr + "✗" + stErrEnd + " "
	case toolRunning:
		return stAccent + "●" + stAccentEnd + " "
	case toolOK:
		return stOK + "•" + stOKEnd + " "
	default:
		return stDim + "•" + stDimEnd + " "
	}
}

// toolStatusText 是状态的中文短词。
//
// 上游只用点的颜色表达状态（终端里看得见），我们还要在无色彩
// （转储、以及色彩被关掉的终端）下可读，所以补一个词。
// toolUnknown 故意**不给词**：不知道就不说，比编一个"完成"强。
func toolStatusText(st toolStatus) string {
	switch st {
	case toolRunning:
		return "运行中"
	case toolOK:
		return "成功"
	case toolErr:
		return "失败"
	case toolAborted:
		return "结果未知"
	}
	return ""
}

// renderToolHeader 画一张卡的头行（宽度 width）。
//
// 名称缺失时写"未知工具"而不是留空：空着的卡看起来像渲染坏了。
func renderToolHeader(c *toolCard, width int) string {
	name := c.name
	if strings.TrimSpace(name) == "" {
		name = "未知工具"
	}
	lead := toolDot(c.status) + stBold + stAccent + name + stAccentEnd + stBoldEnd
	suffix := ""
	if c.hasDuration {
		suffix += " " + stDim + "· " + shortDur(c.duration) + stDimEnd
	}
	if st := toolStatusText(c.status); st != "" {
		suffix += " " + stDim + "· " + stDimEnd
		if c.status == toolErr {
			suffix += stErr + st + stErrEnd
		} else if c.status == toolOK {
			suffix += stOK + st + stOKEnd
		} else {
			suffix += stDim + st + stDimEnd
		}
	}

	// 状态与显式耗时是事实，先给它们留列；长参数只能牺牲参数自己。
	// 否则一个长命令会把 `运行中` / `失败` 顶出行尾，卡片看起来就像没有状态。
	lead = truncDisp(lead, width-dispWidth(suffix))
	if title := strings.TrimSpace(c.title); title != "" {
		available := width - dispWidth(lead) - dispWidth(suffix) - 1
		if available > 0 {
			lead += " " + truncDisp(title, available)
		}
	}
	return truncDisp(lead+suffix, width)
}

// renderToolBody 画正文：第一行挂 ` ⎿ `，续行空三列（上游 GUTTER_FIRST/REST）。
//
// 每行都带上折叠元信息：超过 toolBodyRows 的行在折叠态隐藏，
// 由 Ctrl+O 统一展开——这与工具结果的 Ctrl+T 是**两件事**，
// 前者是"这一行太长"，后者是"结果要不要看"，混在一起会互相打架。
func renderToolBody(c *toolCard, width int) []mdLine {
	if len(c.body) == 0 {
		return nil
	}
	out := make([]mdLine, 0, len(c.body)+1)
	width -= 3
	if width < 8 {
		width = 8
	}
	rank := 0
	fold := c.status != toolErr // 失败正文全显示，也不制造"还有 N 行"的假提示。
	for i, ln := range c.body {
		gutter := "   "
		if i == 0 {
			gutter = " ⎿ "
		}
		text := ln.text
		for j, wrapped := range wrapDisp(text, width) {
			meta := lineMeta{
				toolResult: true,
				isError:    c.status == toolErr,
				capRank:    rank,
			}
			if fold {
				meta.capLimit = toolBodyRows
			}
			g := gutter
			if j > 0 {
				g = "   "
			}
			out = append(out, mdLine{text: g + wrapped, meta: meta})
			rank++
		}
	}
	// 上游 capLines：只多一行就直接展示——藏一行却又多画一行提示没有收益。
	if fold && rank == toolBodyRows+1 {
		for i := range out {
			out[i].meta.capLimit = 0
		}
	}
	if fold && rank-toolBodyRows > 1 {
		out = append(out, mdLine{
			text: "   " + stDim + foldHint(rank-toolBodyRows) + stDimEnd,
			meta: lineMeta{
				toolResult: true,
				capHint:    rank - toolBodyRows,
			},
		})
	}
	return out
}

// shortDur 把真实耗时写成一眼能读的长度。
//
// 不足 1 秒也照样写（`0.4s`）：这类调用最容易被误读成"卡住了"。
func shortDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	ms := d.Milliseconds()
	if ms < 10_000 {
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	secs := int(d / time.Second)
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	mins := secs / 60
	return fmt.Sprintf("%dm%02ds", mins, secs%60)
}

// toolResult 是 tool/result 帧里我们真的用得上的那几样。
type toolResult struct {
	lines       []string
	isError     bool
	durationMs  int64
	hasDuration bool
}

// parseToolResult 把 tool/result 的两种落法读成正文行 + 判定。
//
// 两种落法必须都认：漏认一种的后果不是"少一个标记"，而是**错误被折叠藏起来**、
// 或者一条只有 message.content 的结果显示成空行。
func parseToolResult(data json.RawMessage) toolResult {
	var raw struct {
		Summary    string          `json:"summary"`
		Text       string          `json:"text"`
		Content    json.RawMessage `json:"content"`
		IsError    bool            `json:"isError"`
		DurationMs int64           `json:"durationMs"`
		Duration   int64           `json:"duration"`
		Message    struct {
			Text    string          `json:"text"`
			Content json.RawMessage `json:"content"`
			IsError bool            `json:"isError"`
		} `json:"message"`
	}
	_ = json.Unmarshal(data, &raw)

	var out toolResult
	out.isError = raw.IsError || raw.Message.IsError
	if raw.DurationMs > 0 {
		out.durationMs, out.hasDuration = raw.DurationMs, true
	} else if raw.Duration > 0 {
		out.durationMs, out.hasDuration = raw.Duration, true
	}

	text := raw.Summary
	if text == "" {
		text = raw.Text
	}
	if text == "" {
		text = joinContent(raw.Content)
	}
	if text == "" {
		text = raw.Message.Text
	}
	if text == "" {
		text = joinContent(raw.Message.Content)
	}
	text = strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if text == "" {
		if out.isError {
			out.lines = []string{"ERROR"}
		}
		return out
	}
	out.lines = strings.Split(text, "\n")
	if out.isError {
		// 错误前缀与旧版 extractBrief 一致：**折叠状态下也看得见**的那一行
		// 必须自带"这是失败"，否则一行普通文字会被当成正常输出。
		out.lines[0] = "ERROR " + out.lines[0]
	}
	return out
}

package session

import (
	"strings"

	"dshgo/codecs/v4"
)

// ReasoningText 报告一个内容块是不是可识别的思考块；是则回它的正文。
//
// 判据就只有这一份：块类型必须是 `reasoning`，且正文 TrimSpace 之后非空。
// 空白思考块和"没有"没区别——它既不是内容（assistantHasPayload 也按 TrimSpace 判），
// 也不该把一条真空消息变成可搜命中。
//
// 为什么除了事件级函数还要导出这个块级判据：有些读路径必须**按块顺序**消费一条
// 事件（`cmd/dshd/history.go` 的 eventText 要按 text / reasoning / tool-call 的
// 原始顺序拼正文），它不能只拿思考正文，但仍然必须用同一条判据——否则"什么算
// 思考块"又变成两处各写一遍。
func ReasoningText(kind, text string) (string, bool) {
	// 空白思考块和"没有"是一个意思：它既不是内容，也不许把真空消息变成可搜命中。
	// 单独提一个名字，是因为这条判断和"类型对不对"是两件事——读的人该一眼看出
	// 过滤掉的是**空白**，不是"短的"或"没有正文的"。
	blank := strings.TrimSpace(text) == ""
	if kind != "reasoning" || blank {
		return "", false
	}
	return text, true
}

// ReasoningBlocks 返回一条 v4 事件里的**全部**思考正文（按原顺序，不丢第二块）。
//
// 这是"思考块怎么认"的唯一一份实现。以前它写了两遍（`cmd/dshd` 的 reasoningBySeq
// 与 session 的 reasoningBlocksBySeq）：同一形状各解一遍 JSON、同一条件各写一遍，
// 漂了也没人报错——一处认顶层 content、另一处不认，症状就是"有时候思考块在、
// 有时候不在"。
//
// 两条规则：
//   - 只认 `assistant/message`。别的类型先返回，**不解析 JSON**（工具结果可能很大，
//     不该为了找思考块把它们全部解一遍）；
//   - 块从哪取不在这儿：那条规则只有一份，在 ContentBlocks（`message.content` 优先、
//     顶层 `content` 兜底）。这里只回答"这些块里哪些是思考"，判据是 ReasoningText。
//
// 解不开就当"没有思考"，不返回错误：调用方（fork / 界面投影 / 历史组装）要的是
// 一份投影，一条坏事件不该把整批读路径打断。
func ReasoningBlocks(ev v4.Event) []string {
	if ev.Type != "assistant/message" {
		return nil
	}
	// 取块是单点（ContentBlocks）：这里只判"块里哪些是思考"。
	blocks, ok := ContentBlocks(ev.Data, PreferNested)
	if !ok {
		return nil
	}
	var texts []string
	for i := range blocks {
		if text, ok := ReasoningText(blocks[i].Type, blocks[i].Text); ok {
			texts = append(texts, text)
		}
	}
	return texts
}

// ReasoningBlocksBySeq 按 seq 建"这条事件带哪些思考"的表：seq → 思考正文（按原顺序）。
//
// 为什么不能从 SurfaceMsg 拿：`llm.Message` 没有 reasoning 字段，`type:"reasoning"`
// 的块在投影那一步就被丢掉了。要保住"它为什么这么干"，只能回原始事件按 seq 取。
// 这样也**不用改 SurfaceMsg 的公开形状**（只读 v4.Log.Events）。
//
// 两个口径，写在调用点，不写在识别里：
//   - fork / ACP load 重放要**全带**（把正文搬到新日志、逐条 emit 给客户端）；
//   - serve 的历史组装只回答"有没有"，取 `[0]` 就够（llm.Message 装不下正文，
//     而把模型的心里话当回答塞回去是错的）——见 cmd/dshd/serve_turn.go 的
//     reasoningBySeq，那是既有行为，合并实现不许顺带改掉。
func ReasoningBlocksBySeq(log *v4.Log) map[int64][]string {
	out := make(map[int64][]string)
	for _, ev := range log.Events {
		// 非 assistant/message 在 ReasoningBlocks 的第一个判断就返回，不触发 JSON
		// 解析；所以这里**不再**前置一次类型判断——那会是第二份规则，而它只会漂。
		if texts := ReasoningBlocks(ev); len(texts) > 0 {
			out[ev.Seq] = texts
		}
	}
	return out
}

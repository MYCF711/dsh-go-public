package session

import (
	"fmt"
	"sort"
	"strings"

	"dshgo/codecs/v4"
	"dshgo/internal/spi"
)

// isSurfaceVisible 报告一条事件会不会在 surface 上留下一个节点。
//
// 与 Surface 的输入保持同一份词汇表：5 类消息 + compaction/summary
// （摘要自己也是一个（替身）节点——级联压缩压的就是它）。
// 别的类型（tool/call、compaction/start、turn/end…）只进日志，不进模型眼前。
func isSurfaceVisible(typ string) bool {
	switch typ {
	case "system/message", "developer/message", "user/message", "assistant/message",
		"tool/result", "compaction/summary":
		return true
	}
	return false
}

// Span 是要被压缩遮蔽的区间（按事件 seq，含端点）。
type Span struct {
	Start int64
	End   int64
}

// CompactionMeta 落进 compaction/summary 的元信息。
type CompactionMeta struct {
	Provider      string         `json:"provider,omitempty"`
	Model         string         `json:"model,omitempty"`
	MaxTokens     int            `json:"maxTokens,omitempty"`
	Usage         map[string]any `json:"usage,omitempty"`
	TokenCount    int            `json:"shadowedTokenCount,omitempty"`
	RawOutput     string         `json:"rawOutput,omitempty"`
	LLMStreamCall bool           `json:"llmStreamCall,omitempty"`
}

// Compact 追加 `compaction/start` → `compaction/summary` → `compaction/end` 三条事件，
// 返回被遮蔽的 seq 列表。
//
// 三条铁律（PLAN §10 / research/21）：
//  1. **日志永不删**——只追加，遮蔽发生在派生期；
//  2. `shadowedSeqs` 必须是被遮蔽事件的精确列表（不是范围近似）；
//  3. 摘要进入 surface 成为替身节点。
func Compact(l *Log, events []v4.Event, span Span, summary []spi.Block, meta CompactionMeta) (string, []int64, error) {
	if span.Start > span.End {
		return "", nil, fmt.Errorf("区间非法：start=%d > end=%d", span.Start, span.End)
	}
	shadowed := make([]int64, 0, span.End-span.Start+1)
	visible := 0
	for _, ev := range events {
		if ev.Seq < span.Start || ev.Seq > span.End {
			continue
		}
		shadowed = append(shadowed, ev.Seq)
		if isSurfaceVisible(ev.Type) {
			visible++
		}
	}
	if len(shadowed) == 0 {
		return "", nil, fmt.Errorf("区间 [%d,%d] 里没有任何事件", span.Start, span.End)
	}
	// 遮蔽的全是记账事件（tool/call、compaction/start…）时，surface 一个节点都不会少。
	// 那却照样报「压缩成功」并插入一条摘要，等于凭空多出一条"历史被压过了"的假象。
	// 判据是"模型看得见的东西真的变少了"，不是"事件条数减少了"。
	if visible == 0 {
		return "", nil, fmt.Errorf("区间 [%d,%d] 里没有模型可见的事件（只有 tool/call 之类的记账事件）："+
			"压了 surface 不会变，摘要反而凭空多一条；选一段包含对话的区间", span.Start, span.End)
	}
	// 摘要是空的就不许遮蔽：被压掉的事件会换成一段空白，模型眼前的历史
	// 突然"变短了、却什么也没留下"，而 compaction/end 照样说自己成功。
	// 这是一个典型的静默成功（有产出、没载荷、不报错），所以在写任何事件**之前**拦。
	//
	// 只认 type == "text" 的块：surface 的 joinText 也只看 text，
	// 别的类型（例如 reasoning）写进去同样等于空。
	var summaryText strings.Builder
	for _, b := range summary {
		if b.Type == "text" {
			summaryText.WriteString(b.Text)
		}
	}
	if strings.TrimSpace(summaryText.String()) == "" {
		return "", nil, fmt.Errorf("拒绝压缩 [%d,%d]：摘要为空——遮蔽之后这段历史会变成空白。"+
			"检查摘要生成是否返回了空串（模型空响应/被截断），或改用 --summary 显式给一段", span.Start, span.End)
	}
	sort.Slice(shadowed, func(i, j int) bool { return shadowed[i] < shadowed[j] })

	id := "cmp-" + NewID()
	blocks := make([]map[string]any, 0, len(summary))
	for _, b := range summary {
		blocks = append(blocks, map[string]any{"type": b.Type, "text": b.Text})
	}

	if err := l.Append("compaction/start", map[string]any{
		"compactionId": id,
	}); err != nil {
		return "", nil, err
	}
	if err := l.Append("compaction/summary", map[string]any{
		"compactionId":       id,
		"summary":            blocks,
		"shadowedRange":      map[string]any{"start": span.Start, "end": span.End},
		"shadowedSeqs":       shadowed,
		"shadowedTokenCount": meta.TokenCount,
		"provider":           meta.Provider,
		"model":              meta.Model,
		"maxTokens":          meta.MaxTokens,
		"usage":              meta.Usage,
		"rawOutput":          meta.RawOutput,
		"llmStreamCall":      meta.LLMStreamCall,
	}); err != nil {
		return "", nil, err
	}
	if err := l.Append("compaction/end", map[string]any{
		"compactionId": id,
	}); err != nil {
		return "", nil, err
	}
	return id, shadowed, nil
}

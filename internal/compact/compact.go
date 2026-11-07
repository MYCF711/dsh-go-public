// Package compact 决定"压哪一段"并生成摘要。
//
// 它与 session 分工：session 负责**写事件**（append-only + 精确遮蔽），
// 这里只负责策略——所以可以单独测，不需要碰文件。
package compact

import (
	"context"
	"fmt"
	"strings"

	"dshgo/internal/llm"
	"dshgo/internal/session"
)

// Options 是压缩策略参数。
type Options struct {
	// KeepRecent 是**至少**保留的最近消息数（默认 2）。
	//
	// 它现在是下限，不是保留量：真正的保留量由 KeepTokens 定。
	// 踩过：按"8 条"留，在一条工具输出就 2,300 token 的会话里，
	// 8 条能顶到 8k 以上，压缩看着一直在跑，上下文却纹丝不动。
	KeepRecent int
	// KeepTokens 是保留尾巴的 token 预算（默认 2000）。
	//
	// 按体积留而不是按条数留：**一条大输出和一条短消息不该等价**。
	KeepTokens int
	// MinToCompact 是触发压缩的最小条数（默认 2）。太少不值得压。
	MinToCompact int
	// MaxMessages 是表面超过多少条才触发（0 = 不看条数，只按调用方要求）。
	MaxMessages int
}

func (o Options) withDefaults() Options {
	if o.KeepRecent <= 0 {
		o.KeepRecent = 2
	}
	if o.KeepTokens <= 0 {
		o.KeepTokens = 2000
	}
	if o.MinToCompact <= 0 {
		o.MinToCompact = 2
	}
	return o
}

// keepFrom 返回尾巴的起始下标：从末尾往前累计，直到超出 token 预算。
//
// 两条硬约束：
//  1. 至少留 KeepRecent 条（不然模型眼前一片空白）；
//  2. 再多留就是浪费预算——一条 2,300 token 的工具输出不该因为"条数还够"就整条留下。
func keepFrom(surface []session.SurfaceMsg, o Options) int {
	acc := 0
	for i := len(surface) - 1; i >= 0; i-- {
		acc += session.EstimateRunes(surface[i].Message.Content)
		kept := len(surface) - i
		if acc > o.KeepTokens && kept > o.KeepRecent {
			// i 这一条装不进预算了：从它之后切，保留 surface[i+1:].
			return i + 1
		}
	}
	return 0
}

// Plan 选出一个可压缩的前缀区间（保留给只关心 seq 的调用方）。
func Plan(surface []session.SurfaceMsg, opts Options) (session.Span, int, bool) {
	items, span, ok := Select(surface, opts)
	return span, len(items), ok
}

// Select 选出要压缩的节点本身，并给出它们的 seq 区间。
//
// 为什么还要返回 items：**surface 的顺序不是 seq 顺序**——压缩替身会被插到
// 它遮蔽的位置，但它的 seq 是最后追加的。按 seq 区间回头去筛节点会筛空
// （踩过：日志里出现「没有可摘要的消息」）。
//
// 两条硬约束：
//  1. 只动前缀——最近的 `KeepRecent` 条一定留；
//  2. 开头的 system/message 不压：DSH 只允许被「恰好覆盖该节点的 system/message」
//     替换，压掉它等于让模型忘记身份。压缩替身同样不主动压（没有新信息可提炼）。
func Select(surface []session.SurfaceMsg, opts Options) ([]session.SurfaceMsg, session.Span, bool) {
	o := opts.withDefaults()
	if o.MaxMessages > 0 && len(surface) <= o.MaxMessages {
		return nil, session.Span{}, false
	}

	start := 0
	for start < len(surface) && (surface[start].Type == "system/message" || surface[start].Summary) {
		start++
	}
	end := keepFrom(surface, o) // 开区间右端：按 token 预算推出
	if end <= start {
		return nil, session.Span{}, false
	}
	cut := surface[start:end]
	if len(cut) < o.MinToCompact {
		return nil, session.Span{}, false
	}

	minSeq, maxSeq := cut[0].Seq, cut[0].Seq
	for _, m := range cut {
		if m.Seq < 0 {
			continue
		}
		if minSeq < 0 || m.Seq < minSeq {
			minSeq = m.Seq
		}
		if m.Seq > maxSeq {
			maxSeq = m.Seq
		}
	}
	return cut, session.Span{Start: minSeq, End: maxSeq}, true
}

// 输入预算：太大既贵又容易被截断，太小压不住长会话。
const (
	maxPromptChars  = 24000
	maxMessageChars = 1200
)

// Chat 是摘要生成依赖的模型入口。
type Chat interface {
	Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event) error) (*llm.Response, error)
}

// Summarize 把一段 surface 消息压成文字摘要。
func Summarize(ctx context.Context, chat Chat, model string, items []session.SurfaceMsg) (string, error) {
	if len(items) == 0 {
		return "", fmt.Errorf("没有可摘要的消息")
	}
	var b strings.Builder
	b.WriteString("把下面这段对话压成一份**结构化摘要**，供后续继续工作时参考。\n\n")
	b.WriteString("要求：\n")
	b.WriteString("1. 保留：用户目标、已确认的决定与原因、尚未完成的事、关键文件路径与命令。\n")
	b.WriteString("2. 丢掉：寒暄、重复解释、已被推翻的方案。\n")
	b.WriteString("3. 不要编造原文里没有的内容；不确定就写「未提及」。\n")
	b.WriteString("4. 用中文，分小节，控制在 400 字以内。\n\n")
	b.WriteString("--- 对话开始 ---\n")
	for i, m := range items {
		role := m.Message.Role
		if m.Summary {
			role = "earlier-summary"
		}
		if b.Len() > maxPromptChars {
			fmt.Fprintf(&b, "（后面还有 %d 条因篇幅省略）\n", len(items)-i)
			break
		}
		text := strings.TrimSpace(m.Message.Content)
		if len([]rune(text)) > maxMessageChars {
			text = string([]rune(text)[:maxMessageChars]) + "…（本条已截断）"
		}
		if text == "" && len(m.Message.ToolCalls) > 0 {
			names := make([]string, 0, len(m.Message.ToolCalls))
			for _, tc := range m.Message.ToolCalls {
				names = append(names, tc.Function.Name)
			}
			text = "（调用工具：" + strings.Join(names, ", ") + "）"
		}
		fmt.Fprintf(&b, "[%s] %s\n", role, text)
	}
	b.WriteString("--- 对话结束 ---\n")

	resp, err := chat.Stream(ctx, llm.Request{
		Model: model,
		Messages: []llm.Message{
			{Role: "system", Content: "你是会话压缩器。只输出摘要正文，不要客套。"},
			{Role: "user", Content: b.String()},
		},
		MaxTokens: 2000,
	}, nil)
	if err != nil {
		return "", fmt.Errorf("生成摘要失败: %w", err)
	}
	out := strings.TrimSpace(resp.Content)
	if out == "" {
		return "", fmt.Errorf("模型返回了空摘要（finish=%q，输出 %d token；推理型模型可能把预算花在了思维链上）",
			resp.FinishReason, resp.Usage.CompletionTokens)
	}
	return out, nil
}

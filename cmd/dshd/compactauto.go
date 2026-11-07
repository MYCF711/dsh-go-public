package main

// 自动压缩：把"选区间 → 生成摘要 → 追加三条事件"这段流程做成可复用的函数。
//
// 为什么要抽出来：这段逻辑原本只活在 CLI 的 `dshd session compact` 里，
// 于是 Transport 的 `session.compact`（界面用来点"压缩"的那个）就没有实现可调——
// 界面点了只会得到"宿主还没提供"。抽出来之后两条入口走同一段代码，
// 也就不用担心"CLI 压对了、界面压错了"。

import (
	"context"
	"fmt"

	"dshgo/codecs/v4"
	"dshgo/internal/compact"
	"dshgo/internal/llm"
	"dshgo/internal/session"
	"dshgo/internal/spi"
)

// compactResult 是一次自动压缩的结果。
type compactResult struct {
	// Compacted 表示真的压了；false 表示"没到该压的时候"。
	Compacted bool
	From, To  int64 // 被遮蔽的 seq 区间
	Shadowed  int   // 被遮蔽的事件条数
	Summary   int   // 摘要字数
}

// autoCompact 对一个会话文件做一次自动压缩。
//
// keep：保留最近多少条表面消息；maxMessages：表面超过多少条才触发（0=不设）。
// 返回 Compacted=false 不是错误——"现在不需要压"是正常结果。
func autoCompact(ctx context.Context, path string, keep, maxMessages int, model, base string) (compactResult, error) {
	var out compactResult

	before, err := v4.ReadFile(path)
	if err != nil {
		return out, err
	}
	surface := session.Surface(before)
	span, n, ok := compact.Plan(surface, compact.Options{
		KeepRecent: keep, MinToCompact: 2, MaxMessages: maxMessages,
	})
	if !ok {
		return out, nil
	}

	items := make([]session.SurfaceMsg, 0, n)
	for _, m := range surface {
		if m.Seq >= span.Start && m.Seq <= span.End {
			items = append(items, m)
		}
	}
	text, err := compact.Summarize(ctx, llm.New(base, envOr("DSHD_LLM_KEY", "")), model, items)
	if err != nil {
		return out, fmt.Errorf("生成摘要失败: %w", err)
	}

	l, err := session.OpenSessionAt(path)
	if err != nil {
		return out, err
	}
	_, shadowed, err := session.Compact(l, before.Events, session.Span{Start: span.Start, End: span.End},
		[]spi.Block{{Type: "text", Text: text}},
		session.CompactionMeta{Provider: "dshd", Model: model, TokenCount: len([]rune(text))})
	closeErr := l.Close()
	if err != nil {
		return out, err
	}
	if closeErr != nil {
		return out, closeErr
	}

	out.Compacted = true
	out.From, out.To = span.Start, span.End
	out.Shadowed = len(shadowed)
	out.Summary = len([]rune(text))
	return out, nil
}

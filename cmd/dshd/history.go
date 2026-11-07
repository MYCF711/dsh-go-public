package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"dshgo/codecs/v4"
	"dshgo/internal/kernel"
	"dshgo/internal/session"
	"dshgo/internal/spi"
)

// historyHolder 把"当前会话的日志"暴露成可检索的服务。
//
// 它是**动态**的：装配时还没有会话，所以这里只是一个指针，
// 每轮 prompt 前由 ACP 层指过去。
type historyHolder struct {
	mu  sync.RWMutex
	dir string // 会话文件路径（空 = 还没有活动会话）
}

// attachHistory 把会话指给 history_search。
//
// 两条入口（run / acp）都调它：漏掉任何一条，模型回查就会撞上"没有活动会话"。
func attachHistory(log *session.Log) {
	if log == nil {
		return
	}
	hostHistory.set(log.Path)
}

func (h *historyHolder) set(path string) {
	h.mu.Lock()
	h.dir = path
	h.mu.Unlock()
}

func (h *historyHolder) Search(query string, limit int) ([]spi.HistoryHit, error) {
	if query == "" {
		return nil, fmt.Errorf("query 为空")
	}
	h.mu.RLock()
	path := h.dir
	h.mu.RUnlock()
	if path == "" {
		return nil, fmt.Errorf("没有活动会话")
	}
	log, err := v4.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读会话失败: %w", err)
	}

	// 哪些 seq 已经被压缩遮蔽——命中了也要标出来，别让模型以为它还在表面上。
	hidden := hiddenSeqs(log)

	hits := make([]spi.HistoryHit, 0, limit)
	for _, ev := range log.Events {
		text := eventText(ev)
		if text == "" || !strings.Contains(text, query) {
			continue
		}
		hits = append(hits, spi.HistoryHit{
			Seq:     ev.Seq,
			Type:    ev.Type,
			Role:    roleOf(ev.Type),
			Excerpt: excerpt(text, query),
			Hidden:  hidden[ev.Seq],
		})
		if len(hits) >= limit {
			break
		}
	}
	return hits, nil
}

// hiddenSeqs 收集所有被压缩遮蔽的 seq。
func hiddenSeqs(log *v4.Log) map[int64]bool {
	out := map[int64]bool{}
	for _, ev := range log.Events {
		if ev.Type != "compaction/summary" && ev.Type != "compaction/prune" {
			continue
		}
		var d struct {
			ShadowedSeqs []int64 `json:"shadowedSeqs"`
		}
		if json.Unmarshal(ev.Data, &d) != nil {
			continue
		}
		for _, s := range d.ShadowedSeqs {
			out[s] = true
		}
	}
	return out
}

// eventText 抽出一段可用于检索的正文。
//
// 不只 `type=="text"`：工具调用与思考块同样是"这一轮说过的话"。
// 旧实现只拼 text 块，于是**只有 `tool_calls` 的 assistant 在 history_search 里
// 完全查不到**——而"上次那个 ls 是哪一轮"正是这个工具存在的理由。
//
// 形态与 subagent_fork 的继承转写共用 toolCallText / reasoningText（同一份，不写第二份）：
// 模型看见 `[调用 bash：{"command":"ls"}]` 就知道该按什么搜；片段里也不会是整段
// 事件 JSON（那是机器形状，翻出来全是转义符）。
//
// 判据与 assistantHasPayload 对齐：正文、思考、工具调用占一样就有正文可搜；
// 三样都没有的事件（比如只有一个空 text 块）仍然返回空串，不算内容。
func eventText(ev v4.Event) string {
	// 取块规则只有一份（session.ContentBlocks）：message.content 优先、顶层 content 兜底。
	// 这里只负责"按块顺序把 text / reasoning / tool-call 拼出来"。
	blocks, ok := session.ContentBlocks(ev.Data, session.PreferNested)
	if !ok {
		return ""
	}
	if len(blocks) == 0 {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			// 空白的 text 块和"没有"没区别（assistantHasPayload 也按 TrimSpace 判）。
			if strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		case "reasoning":
			// 判据只有一份（session.ReasoningText）：空白思考块和"没有"没区别，
			// 不能让它把真空消息变成可搜命中。
			//
			// 为什么不直接调 session.ReasoningBlocks(ev)：这里要的是**按块顺序**拼
			// text / reasoning / tool-call，只能自己走块列表；能共用的是"什么算一个
			// 思考块"这条判据，不是整条事件的取法。
			if text, ok := session.ReasoningText(b.Type, b.Text); ok {
				parts = append(parts, reasoningText(text))
			}
		case "tool-call":
			parts = append(parts, toolCallText(b.Name, b.Arguments))
		}
	}
	return strings.Join(parts, "\n")
}

func roleOf(t string) string {
	switch t {
	case "user/message":
		return "user"
	case "assistant/message":
		return "assistant"
	case "tool/result":
		return "tool"
	case "system/message":
		return "system"
	}
	return ""
}

// excerpt 取命中位置前后各一小段，避免把整段历史倒给模型。
func excerpt(text, query string) string {
	const before, after = 80, 120
	idx := strings.Index(text, query)
	if idx < 0 {
		return text
	}
	start := idx - before
	if start < 0 {
		start = 0
	}
	end := idx + len(query) + after
	if end > len(text) {
		end = len(text)
	}
	s := text[start:end]
	s = strings.ReplaceAll(s, "\n", " ")
	prefix := ""
	if start > 0 {
		prefix = "…"
	}
	suffix := ""
	if end < len(text) {
		suffix = "…"
	}
	return prefix + s + suffix
}

func historyModule(h *historyHolder) kernel.Module {
	return kernel.Module{
		ID:       "history",
		Backend:  kernel.BackendStatic,
		Provides: []string{"history"},
		Register: func(s *kernel.Scope) error {
			return kernel.Provide(s, spi.HistoryKey, spi.HistorySearch(h))
		},
	}
}

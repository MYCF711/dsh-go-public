package main

// 官方桌面端 0.2.2-rc.1 的 session.* 方法的**宿主侧接线**。
//
// # 分工
//
// 契约、参数校验、分页/等待/搜索语义、错误信封都在
// `internal/transport/handlers_session_official.go`；本文件只提供 transport
// 拿不到的真实数据：
//
//	DurableEvents   读真实 v4 durable 事件（transport 不许 import 会话编解码包，R7）
//	LiveState       读真实 live turn 与真实 pending 队列（promptService）
//	Search          读真实会话正文（sessionSource.Messages 的投影）
//	RemoveQueueItem 精确撤掉真实 pending 队列里的一项
//
// # 为什么挂在 opt.Source 上
//
// transport 的 Options 在 server.go 里，本轮写作用域不允许改它。官方会话读本来
// 就是"会话数据源"的活，所以把能力挂在 Source 上；transport 用类型断言取，
// 断言不到就回 501。包装用**嵌入具体类型** `*sessionSource`（不是嵌入接口），
// 这样它已有的全部可选能力（Tail/Seq/Usage/Window/Model/Effort/EffortSelection/
// Goal/Permission/Cwd）由 Go 的提升规则原样保留——直接嵌入 `transport.Source`
// 接口会让这些类型断言全部失效，session.load 会静默退化。

import (
	"errors"
	"fmt"
	"strings"

	"dshgo/codecs/v4"
	"dshgo/internal/transport"
)

// officialSessionHost 是 transport.OfficialSessionHost 的真实实现。
type officialSessionHost struct {
	// src 是生产会话数据源：路径解析（cwd 提示、session- 前缀、裸 uuid）与
	// 消息投影都复用它，不另开第二条读盘入口。
	src *sessionSource
	// home 是 <home>；搜索要列会话，列表只有一份真相（session.ListSessions）。
	home string
	// prompts 是真实轮次/队列服务；可为 nil（不带 serve 的入口）。
	prompts *promptService
}

// DurableEvents 读该会话**全部** durable v4 事件与日志末端 seq。
//
// 返回的是事件原样（type/seq/time/data + surfaceOp/sourceEventSeqs/ignorable），
// 不是 messages 投影——官方 page/events 的 records 就是 durable 事件本身，
// 用投影反推 event.type 等于编。
func (h *officialSessionHost) DurableEvents(sessionID string) ([]transport.DurableEvent, int64, error) {
	path, err := h.src.resolve(sessionID)
	if err != nil {
		return nil, 0, err
	}
	lg, err := v4.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("读会话日志失败: %w", err)
	}
	events := make([]transport.DurableEvent, 0, len(lg.Events))
	for _, ev := range lg.Events {
		events = append(events, transport.DurableEvent{
			Type:            ev.Type,
			Seq:             ev.Seq,
			Time:            ev.Time,
			Data:            ev.Data,
			Ignorable:       ev.Ignorable,
			SourceEventSeqs: ev.SourceEventSeqs,
			SurfaceOp:       ev.SurfaceOp,
		})
	}
	logEnd := int64(-1)
	if len(events) > 0 {
		logEnd = events[len(events)-1].Seq
	}
	return events, logEnd, nil
}

// LiveState 读真实轮次与真实 pending 队列。
//
// running 是"这一会话此刻真有 live turn"；queued 是真实的排队 TurnID。
// 没有 prompts（无 serve 入口）时如实回零值，不编。
func (h *officialSessionHost) LiveState(sessionID string) (transport.OfficialLiveState, error) {
	if h.prompts == nil {
		return transport.OfficialLiveState{}, nil
	}
	running, queued := h.prompts.officialSnapshot(sessionID)
	return transport.OfficialLiveState{
		Active:   running || len(queued) > 0,
		Running:  running,
		QueueIDs: queued,
	}, nil
}

// Search 按官方语义搜真实会话正文。
//
// 只搜 user/message 与 assistant/message（官方 eventFilters），一个会话最多出一条
// （官方 acceptedIds 去重），按 limit+1 探测 hasMore。坏会话跳过，不让整次搜索失败。
func (h *officialSessionHost) Search(query string, limit int) ([]transport.OfficialSearchItem, bool, error) {
	sessions, err := listSessions(h.home, "")
	if err != nil {
		return nil, false, fmt.Errorf("列会话失败: %w", err)
	}
	items := make([]transport.OfficialSearchItem, 0, limit)
	for _, summary := range sessions {
		msgs, merr := h.src.Messages(summary.ID)
		if merr != nil {
			continue
		}
		snippet, ok := officialSnippetOf(msgs, query)
		if !ok {
			continue
		}
		items = append(items, transport.OfficialSearchItem{SessionID: summary.ID, Snippet: snippet})
		if len(items) > limit {
			return items[:limit], true, nil
		}
	}
	return items, false, nil
}

// officialSnippetOf 在一条会话的投影消息里找第一个命中（大小写不敏感）。
//
// 命中点用 transport.OfficialSearchWindow 定位（按 rune 计），snippet 用
// transport.OfficialSnippet 裁到官方上限（240 code point）。
func officialSnippetOf(msgs []transport.Message, query string) (string, bool) {
	for _, msg := range msgs {
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		if strings.TrimSpace(msg.Text) == "" {
			continue
		}
		hit, ok := transport.OfficialSearchWindow(msg.Text, query)
		if !ok {
			continue
		}
		return transport.OfficialSnippet(msg.Text, hit), true
	}
	return "", false
}

// RemoveQueueItem 精确撤掉真实 pending 队列里的一项。
//
// 在 `p.queues` 里命中时**在同一把锁内**剪掉，避免"先查再撤"之间该项被提升成
// live turn 然后被误取消（那会杀掉正在跑的一轮）。命中 interrupting 那一项时
// 交给 CancelPlacement——它还要顺带把下一项真的启动起来。
func (h *officialSessionHost) RemoveQueueItem(sessionID, itemID string) error {
	if h.prompts == nil {
		return transport.ErrSessionOfficialUnsupported
	}
	removed, ok, err := h.prompts.officialRemoveQueued(sessionID, itemID)
	if err != nil {
		return err
	}
	if ok {
		dropQueuedTurns([]*queuedTurn{removed})
		return nil
	}
	if !h.prompts.officialQueueHas(sessionID, itemID) {
		return transport.ErrNoSuchQueueItem
	}
	res, err := h.prompts.CancelPlacement(sessionID, "user", itemID)
	if err != nil {
		if isNoSuchTurn(err) {
			// 查与撤之间它已经不再是排队项（被提升/被撤走），如实说不在了。
			return transport.ErrNoSuchQueueItem
		}
		return err
	}
	if len(res.CancelledQueued) == 0 {
		return transport.ErrNoSuchQueueItem
	}
	return nil
}

// isNoSuchTurn 判断是不是"这一轮已经不在"。
func isNoSuchTurn(err error) bool {
	return errors.Is(err, ErrNoSuchTurn)
}

// officialSessionSource 把官方会话能力挂在生产会话数据源上。
//
// 两个嵌入指针缺一不可：`*sessionSource` 提供 Source 与全部可选能力，
// `*officialSessionHost` 提供官方四方法。
type officialSessionSource struct {
	*sessionSource
	*officialSessionHost
}

// withOfficialSessionOptions 接上官方 session.* 的真实宿主能力。
//
// 只在 opt.Source 就是生产 `*sessionSource` 时替换；别的形态（测试假源、
// 第三方宿主）一律原样返回，保持 501 闭合分派——不猜、不拿假的源替换真的。
func withOfficialSessionOptions(home string, opt transport.Options, prompts *promptService) transport.Options {
	src, ok := opt.Source.(*sessionSource)
	if !ok || src == nil {
		return opt
	}
	opt.Source = &officialSessionSource{
		sessionSource: src,
		officialSessionHost: &officialSessionHost{
			src:     src,
			home:    home,
			prompts: prompts,
		},
	}
	return opt
}

// officialSnapshot 返回"此刻是否真有 live turn"与真实排队 TurnID。
func (p *promptService) officialSnapshot(sessionID string) (running bool, queued []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bySID[sessionID] != nil {
		running = true
	}
	return running, p.queueIDsLocked(sessionID)
}

// officialQueueHas 报告 itemID 是否还是 pending 队列项（只读）。
func (p *promptService) officialQueueHas(sessionID, itemID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range p.queueIDsLocked(sessionID) {
		if id == itemID {
			return true
		}
	}
	return false
}

// officialRemoveQueued 在**同一把锁内**从 p.queues 剪掉一项。
//
// 只处理普通 FIFO 队列；interrupting 那一项返回 ok=false 交给 CancelPlacement，
// 因为撤它还要把队列里的下一项真的启动起来。
func (p *promptService) officialRemoveQueued(sessionID, itemID string) (*queuedTurn, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	queue := p.queues[sessionID]
	for i, item := range queue {
		if item.id != itemID {
			continue
		}
		queue = append(queue[:i], queue[i+1:]...)
		if len(queue) == 0 {
			delete(p.queues, sessionID)
		} else {
			p.queues[sessionID] = queue
		}
		return item, true, nil
	}
	return nil, false, nil
}

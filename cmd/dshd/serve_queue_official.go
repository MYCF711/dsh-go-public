package main

// 官方 `session.state.queue` / `session.updateQueue` 的**宿主侧**实现。
//
// 契约、参数校验、错误信封在 `internal/transport/handlers_session_official.go`；
// 本文件只提供 transport 拿不到的真实数据与真实动作：
//
//	LiveQueue       真实 pending 项的完整待发内容（官方 state.queue 的 content）
//	EditQueueItem   改真实待发内容（改完之后 runner **真的**按新内容发）
//	SteerQueueItem  把一项从队列摘掉，走真实 steer 通道塞进正在跑的那一轮
//
// # 为什么内容要留在队列里
//
// 官方 queue 项的 content 是必填数组，`updateQueue.edit` 改的也是那一份内容。
// 以前 `queuedTurn` 只保留 TurnID 与 runner 闭包（content 被闭包捕获），
// 于是 `state.queue` 只能给 null、edit/steer 只能 501。现在 queuedTurn 保留
// 真正的待发内容：state 回真实条目，edit 改的是 runner 启动时要读的那一份，
// steer 摘下来的也是同一份——三处共用一个真相，不存在"改了一份视图"。
//
// # placement 只有 queued
//
// 官方 placement 三个值：queued（next-turn）、steering（next-step 且来源是用户）、
// context（next-step 且来源是上下文）。本宿主的 promptService 只有 next-turn 队列
// （另一类是 now 保留项，它同样在 next-turn 语义里），没有 next-step inbox 投影，
// 所以真实值只有 queued。**不编 steering/context 充数**：那会让客户端以为还有
// 一步边界内的注入队列，而这里根本没有。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"dshgo/internal/transport"
)

// officialSteerInjectTimeout 是 updateQueue.steer 注入的上限。
//
// 注入是同步完成的（成功返回即已接管到下一个 step 边界），但 runner 可能在
// 收尾边缘短暂阻塞；给它一个上限，超时就如实报错并把这一项放回队列。
const officialSteerInjectTimeout = 30 * time.Second

// officialQueuedPlacement 是本宿主 eviction 之外唯一的真实 placement。
const officialQueuedPlacement = "queued"

// setPromptContent 记下这一项的待发内容。
//
// 两个参数互斥：text 走 `turnRunner.Run`，blocks 走 `contentTurnRunner.RunContent`。
// 两者都为空表示"这一项没有内容视图"（旧调用方），此时 state.queue 里仍会给出
// 它自己的 TurnID 与空 content——但那只可能来自**没有**经 Submit* 提交的内部项，
// 生产路径上不会出现（Submit* 一定会写其中一份）。
func (q *queuedTurn) setPromptContent(text string, blocks []transport.ContentBlock) {
	q.contentMu.Lock()
	q.text = text
	q.blocks = blocks
	q.contentMu.Unlock()
}

// promptContent 返回这一项此刻的待发内容（runner 启动时读，编辑后立刻生效）。
func (q *queuedTurn) promptContent() (string, []transport.ContentBlock) {
	q.contentMu.Lock()
	defer q.contentMu.Unlock()
	if len(q.blocks) == 0 {
		return q.text, nil
	}
	blocks := make([]transport.ContentBlock, len(q.blocks))
	copy(blocks, q.blocks)
	return q.text, blocks
}

// runQueued 按这一项**此刻**的内容启动一轮：有序块走 RunContent，纯文本走 Run。
//
// 这里不能沿用提交时捕获的闭包参数：updateQueue.edit 会在排队期间换内容，
// 而 runner 是在那一项真的成为当前轮时才启动的。
func (p *promptService) runQueued(ctx context.Context, sessionID, turnID string, item *queuedTurn) error {
	text, blocks := item.promptContent()
	if len(blocks) > 0 {
		runner, ok := p.run.(contentTurnRunner)
		if !ok {
			return fmt.Errorf("%w: 当前 runner 未实现有序 content blocks", transport.ErrPlacementUnsupported)
		}
		return runner.RunContent(ctx, sessionID, turnID, blocks)
	}
	return p.run.Run(ctx, sessionID, turnID, text)
}

// steerInto 走**真实的 steer 注入通道**；没有该能力的 runner 明确失败，
// 绝不退化成 followup 队列（那会变成"排到最后"而不是"塞进这一轮"）。
func (p *promptService) steerInto(ctx context.Context, sessionID, turnID, text string, blocks []transport.ContentBlock) error {
	if len(blocks) > 0 {
		runner, ok := p.run.(steerContentTurnRunner)
		if !ok {
			return fmt.Errorf("%w: 当前 runner 没有真实 content steer 注入口", transport.ErrPlacementUnsupported)
		}
		return runner.SteerContent(ctx, sessionID, turnID, blocks)
	}
	runner, ok := p.run.(steerTurnRunner)
	if !ok {
		return fmt.Errorf("%w: 当前 runner 没有真实 steer 注入口", transport.ErrPlacementUnsupported)
	}
	return runner.Steer(ctx, sessionID, turnID, text)
}

// officialQueueContent 把这一项的待发内容铺成官方 content 数组。
//
// 数组元素是本宿主 v4 durable 块的**原样** JSON（text / attachment / reference），
// 与 session.history 返回的块同一套词表。官方 content 的元素类型是任意 JsonValue，
// 所以这仍然是合法 content；而编成官方的 image/file 形状是做不到的——那些块要
// 带上真 base64 或上传 receipt，队列里没有，也不该为了形状去复制一份。
func (q *queuedTurn) officialQueueContent() []json.RawMessage {
	text, blocks := q.promptContent()
	out := make([]json.RawMessage, 0, len(blocks)+1)
	if len(blocks) == 0 {
		if strings.TrimSpace(text) == "" {
			return []json.RawMessage{}
		}
		raw, err := json.Marshal(map[string]string{"type": "text", "text": text})
		if err != nil {
			return []json.RawMessage{}
		}
		return append(out, raw)
	}
	for _, block := range blocks {
		raw, err := json.Marshal(block)
		if err != nil {
			continue
		}
		out = append(out, raw)
	}
	return out
}

// officialQueueEntries 读出真实 pending 项（顺序与 queueIDsLocked 一致：
// now 保留项在前，然后是按先来后到的 FIFO）。
//
// 调用方必须持有 p.mu。读内容时再拿 item.contentMu——它永不回头拿 p.mu，
// 所以这里不会与 runner 的读形成环。
func (p *promptService) officialQueueEntriesLocked(sessionID string) []transport.OfficialQueueItem {
	pending := p.interrupting[sessionID]
	queue := p.queues[sessionID]
	items := make([]transport.OfficialQueueItem, 0, len(queue)+1)
	add := func(item *queuedTurn) {
		if item == nil || item.id == "" {
			return
		}
		items = append(items, transport.OfficialQueueItem{
			ID:        item.id,
			Placement: officialQueuedPlacement,
			Content:   item.officialQueueContent(),
		})
	}
	add(pending)
	for _, item := range queue {
		add(item)
	}
	return items
}

// officialEditQueued 用新内容替换一项 pending 内容，返回是否命中。
//
// 只认同一个真相（item 的 content）：runner 启动时读它，所以改完立刻生效。
// 命中 interrupting（now 保留项）也算命中——官方 nextTurn 同样包含它。
func (p *promptService) officialEditQueued(sessionID, itemID, text string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pending := p.interrupting[sessionID]; pending != nil && pending.id == itemID {
		pending.setPromptContent(text, nil)
		return true
	}
	for _, item := range p.queues[sessionID] {
		if item.id != itemID {
			continue
		}
		item.setPromptContent(text, nil)
		return true
	}
	return false
}

// officialRestoreQueued 把一项按原位置放回 FIFO（steer 注入失败时用）。
//
// 为什么不是 append：放回队尾会让它在别的排队项之后，等于这一次失败把用户的
// 消息往后挪了；index 是它被摘走时的位置，restore 时按 ≤ 的位置插回去。
func (p *promptService) officialRestoreQueued(sessionID string, item *queuedTurn, index int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	queue := p.queues[sessionID]
	if index < 0 || index > len(queue) {
		index = len(queue)
	}
	queue = append(queue, nil)
	copy(queue[index+1:], queue[index:])
	queue[index] = item
	p.queues[sessionID] = queue
}

// officialDetachQueuedAt 与 officialDetachQueued 同义，另外回报原位置（restore 用）。
func (p *promptService) officialDetachQueuedAt(sessionID, itemID string) (*queuedTurn, int, bool) {
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
		return item, i, true
	}
	return nil, -1, false
}

// officialQueueHasPending 报告是否还有这一项（只读，用于 steer 的错误分类）。
func (p *promptService) officialQueueHasPending(sessionID, itemID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pending := p.interrupting[sessionID]; pending != nil && pending.id == itemID {
		return true
	}
	for _, item := range p.queues[sessionID] {
		if item.id == itemID {
			return true
		}
	}
	return false
}

// LiveQueue 返回真实待发内容。
//
// 冷会话（这一进程里既没有 live turn、也没有 pending 项）回 nil，也就是官方
// 契约里的 `queue: null`（官方 readState：`agents.get(id) === undefined → null`）。
// 只有"这一轮真在跑或真有排队项"时才给数组——那个数组可以合法地是空数组。
func (h *officialSessionHost) LiveQueue(sessionID string) ([]transport.OfficialQueueItem, error) {
	if h.prompts == nil {
		return nil, nil
	}
	h.prompts.mu.Lock()
	defer h.prompts.mu.Unlock()
	cold := h.prompts.bySID[sessionID] == nil &&
		h.prompts.interrupting[sessionID] == nil &&
		len(h.prompts.queues[sessionID]) == 0
	if cold {
		return nil, nil
	}
	return h.prompts.officialQueueEntriesLocked(sessionID), nil
}

// EditQueueItem 改真实待发内容。
//
// content 由 transport 侧校验过（只接受 text 块、必须含非空白文本），这里再解一次
// 是为了把它变成 runner 真正会读的那份内容；解析失败按参数错如实回报。
func (h *officialSessionHost) EditQueueItem(sessionID, itemID string, content []json.RawMessage) error {
	if h.prompts == nil {
		return transport.ErrSessionOfficialUnsupported
	}
	if len(content) == 0 {
		return fmt.Errorf("%w: edit 需要非空 content", transport.ErrPlacementInvalid)
	}
	blocks := make([]transport.ContentBlock, 0, len(content))
	for _, raw := range content {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			return fmt.Errorf("%w: edit 的 content 元素必须是 JSON 对象: %v", transport.ErrPlacementInvalid, err)
		}
		if block.Type != "text" {
			return fmt.Errorf("%w: edit 只接受 text 块（官方 QUEUE_EDIT_NON_TEXT）", transport.ErrPlacementInvalid)
		}
		blocks = append(blocks, transport.ContentBlock{Type: "text", Text: block.Text})
	}
	text := joinQueueTextBlocks(blocks)
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("%w: edit 内容必须含非空白文本", transport.ErrPlacementInvalid)
	}
	if !h.prompts.officialEditQueued(sessionID, itemID, text) {
		return transport.ErrNoSuchQueueItem
	}
	return nil
}

// joinQueueTextBlocks 把 text 块按 "\\n" 拼成 runner 的文本输入。
//
// 与 v4 的文本投影同一口径（blocks.go 的 joinText 只留 text 块、按换行拼），
// 所以 edit 之后的正文与"这条消息一开始就是这段文本"完全一致。
func joinQueueTextBlocks(blocks []transport.ContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, "\n")
}

// SteerQueueItem 把一项 pending 内容塞进**正在跑**的那一轮。
//
// 官方判据（index.js:930）：目标必须是 next-turn 项，且 agent 正在跑；否则报
// session/steer-unavailable。这里同口径：
//   - 项不在队列里（已被提升/撤回）→ ErrNoSuchQueueItem；
//   - 没有 live turn → ErrSteerUnavailable（晚到的 steer 不能变成新的一轮）；
//   - 命中 now 保留项 → ErrSteerUnavailable（当前轮正在被它接替，注入没有落点）；
//   - 注入失败 → 按原位置放回队列并原样返回错误（不吞消息、不假装成功）。
func (h *officialSessionHost) SteerQueueItem(sessionID, itemID string) error {
	if h.prompts == nil {
		return transport.ErrSessionOfficialUnsupported
	}
	h.prompts.mu.Lock()
	live := h.prompts.bySID[sessionID]
	pendingNow := h.prompts.interrupting[sessionID]
	h.prompts.mu.Unlock()
	if pendingNow != nil && pendingNow.id == itemID {
		return fmt.Errorf("%w: 这一项是 now 保留项，正在接替当前轮，没有可注入的落点", transport.ErrSteerUnavailable)
	}
	if live == nil {
		// 已经在队列里但没有当前轮：官方同样拒绝（steer 只作用于正在跑的那一轮）。
		if h.prompts.officialQueueHasPending(sessionID, itemID) {
			return fmt.Errorf("%w: 这一会话没有正在跑的一轮", transport.ErrSteerUnavailable)
		}
		return transport.ErrNoSuchQueueItem
	}
	turnID := live.id
	item, index, ok := h.prompts.officialDetachQueuedAt(sessionID, itemID)
	if !ok {
		return transport.ErrNoSuchQueueItem
	}
	text, blocks := item.promptContent()
	if strings.TrimSpace(text) == "" && len(blocks) == 0 {
		h.prompts.officialRestoreQueued(sessionID, item, index)
		return fmt.Errorf("%w: 这一项没有可注入的内容", transport.ErrSteerUnavailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), officialSteerInjectTimeout)
	defer cancel()
	if err := h.prompts.steerInto(ctx, sessionID, turnID, text, blocks); err != nil {
		// 注入失败：这一轮没吃到它，那就还是排队项。原位置放回。
		h.prompts.officialRestoreQueued(sessionID, item, index)
		if errors.Is(err, transport.ErrPlacementUnsupported) {
			return fmt.Errorf("%w: %v", transport.ErrSteerUnavailable, err)
		}
		return err
	}
	// 注入成功：它在当前轮里了，不再排队。retire 关掉 ctx/done，避免这条
	// 已经完成使命的排队项留下一个永不关闭的 done。
	item.retire()
	return nil
}

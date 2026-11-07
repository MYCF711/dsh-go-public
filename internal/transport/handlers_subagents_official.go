package transport

// 本文件是官方 `subagents.*`（`@deepseek-ai/dsh-subagent` 的
// `SubagentRuntime.prompt` / `SubagentRuntime.interruptByParent`）在宿主侧的
// **可续问子会话**实现。
//
// # 为什么要这一层（本仓原来的缺口）
//
// 本仓子代理是**同步委派**（`subRunner.RunSub` 跑完即返回正文）：没有 child
// 会话 id、没有 inbox、没有父子 activation，所以官方这两个方法无地址可用。
// 官方 wire 的形状是"浏览器持有一个 (parentSessionId, childSessionId, mode)
// 控制地址，往子会话的 inbox 投递一条消息，再能按父授权中断它"。
//
// # 最小可行设计
//
//	地址：子会话 = 一条**真实 durable v4 会话**（由宿主 CreateSession 建），
//	      注册表只加两样东西——**父子归属**与**投递回执**。没有归属的子会话
//	      拒绝投递（cross_session），未注册的子会话不存在（no_such_session）。
//	inbox：queue / steer 两条道，与官方 `inbox.nextTurn` / `inbox.nextStep` 同构。
//	      queue = 排到当前轮收工之后（transport PlacementFollowup），
//	      steer = 注入当前轮的下一 step 边界（transport PlacementSteer）。
//	      真实排队与真实注入都在宿主的 promptService 里，本层只做**地址解析 +
//	      授权 + 幂等 + 回执**，不复制一份队列（复制就会出现两个真值）。
//	取消：interruptByParent 走宿主注入的 Interrupt（真实 cancel 当前轮）。
//	      官方语义"accepted 只表示取消信号已被接受，不代表目标已静止"。
//	隔离：每个子会话只属于**一个**父会话；父 id 不匹配即 cross_session 403。
//	      A/B 两个 Server 各有一份注册表（按 *Server 挂载），互不可见。
//	并发：注册表一把 RWMutex；投递是"取快照 → 释放锁 → 调宿主"，
//	      宿主回调里再回锁不会自锁。同一 requestId 的重投是**幂等**的
//	      （回同一个 messageId，不再投递第二次）。
//
// # 诚实边界
//
// 本层**不会**在没有真实子会话时编一个 messageId：没有 Spawn 过的 child
// 一律 no_such_session。反过来，只要回了 messageId，就说明消息真的持久化
// 过、且真的交给了宿主的投递通道。

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// 子会话
// ---------------------------------------------------------------------------

// OfficialChildMessage 是投递进子会话 inbox 的一条消息（durable 回执）。
type OfficialChildMessage struct {
	MessageID      string                          `json:"messageId"`
	RequestID      string                          `json:"requestId"`
	ParentID       string                          `json:"parentSessionId"`
	Delivery       string                          `json:"delivery"`
	Content        []OfficialSubagentPromptContent `json:"content"`
	ClientTimeZone string                          `json:"clientTimeZone,omitempty"`
	EnqueuedAt     int64                           `json:"enqueuedAt"`
	// Delivered 为真表示宿主投递通道已接受；失败时 Err 带原因。
	Delivered bool   `json:"delivered"`
	Err       string `json:"error,omitempty"`
}

// OfficialChild 是一个活的、可续问的子会话地址。
type OfficialChild struct {
	ID              string
	ParentSessionID string
	Cwd             string

	mu     sync.Mutex
	closed bool
	inbox  []OfficialChildMessage
	// byRequest 让同一个 requestId 的重投幂等（返回同一个 messageId）。
	byRequest map[string]int
}

// Inbox 返回 inbox 的**只读快照**（按投递顺序）。
func (c *OfficialChild) Inbox() []OfficialChildMessage {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]OfficialChildMessage, len(c.inbox))
	copy(out, c.inbox)
	return out
}

// ---------------------------------------------------------------------------
// 注册表
// ---------------------------------------------------------------------------

// OfficialChildSessionOptions 是注册表需要的全部真实能力（宿主注入）。
type OfficialChildSessionOptions struct {
	// Create 真建一条 durable 子会话（宿主：session.CreateSession）。
	// parentID 必须记进会话元数据，否则重启后父子关系就丢了。
	Create func(childSessionID, parentSessionID, cwd string) error
	// Deliver 把内容真的投进子会话的 prompt 通道。
	// placement 是 transport 的 canonical placement（followup / steer）。
	Deliver func(ctx context.Context, childSessionID string, content []OfficialSubagentPromptContent, placement string) error
	// Interrupt 真的中断子会话当前轮；返回"是否真有在飞的一轮被打断"。
	Interrupt func(childSessionID string) (bool, error)
	// Persist 把一条投递回执写进 durable 日志（宿主：v4 append）。
	// 为 nil 时只是不记账，不影响投递本身。
	Persist func(childSessionID, eventType string, data any) error
	// Now 是时钟（测试可替换）。
	Now func() time.Time
}

// OfficialChildRegistry 是"可续问子会话"的地址表。
type OfficialChildRegistry struct {
	opts OfficialChildSessionOptions

	mu       sync.RWMutex
	children map[string]*OfficialChild
	// parents 是 parent → children 的反向索引（列/清理用）。
	parents map[string]map[string]struct{}
	token   string
	seq     atomic.Uint64
}

// NewOfficialChildRegistry 造一个空注册表。
func NewOfficialChildRegistry(opts OfficialChildSessionOptions) *OfficialChildRegistry {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &OfficialChildRegistry{
		opts:     opts,
		children: map[string]*OfficialChild{},
		parents:  map[string]map[string]struct{}{},
		token:    strconv.FormatInt(opts.Now().UnixNano()&0xffffffff, 16),
	}
}

// Spawn 真建一条子会话并登记父子地址。
//
// 这是官方 `subagent` 工具 `continuable` 模式在本仓的落点：宿主（或工具层）
// 调它，之后 `subagents.prompt` 才有地址可投。重复 Spawn 同一个 id 是**幂等**
// 的（同一个父返回既有条目；不同父返回 cross_session）。
func (r *OfficialChildRegistry) Spawn(childSessionID, parentSessionID, cwd string) (*OfficialChild, error) {
	if childSessionID == "" || parentSessionID == "" {
		return nil, officialBadRequest("子会话 id / 父会话 id 不能是空串")
	}
	if childSessionID == parentSessionID {
		return nil, officialBadRequest("子会话不能就是父会话（%q）", childSessionID)
	}
	r.mu.RLock()
	existing := r.children[childSessionID]
	r.mu.RUnlock()
	if existing != nil {
		if existing.ParentSessionID != parentSessionID {
			return nil, &officialError{
				status: 403, code: "cross_session",
				msg: fmt.Sprintf("子会话 %q 属于父会话 %q，不是 %q", childSessionID, existing.ParentSessionID, parentSessionID),
			}
		}
		return existing, nil
	}
	if r.opts.Create != nil {
		if err := r.opts.Create(childSessionID, parentSessionID, cwd); err != nil {
			return nil, officialInternal("建子会话 %q 失败: %v", childSessionID, err)
		}
	}
	child := &OfficialChild{
		ID: childSessionID, ParentSessionID: parentSessionID, Cwd: cwd,
		byRequest: map[string]int{},
	}
	r.mu.Lock()
	if dup := r.children[childSessionID]; dup != nil {
		r.mu.Unlock()
		return dup, nil
	}
	r.children[childSessionID] = child
	if r.parents[parentSessionID] == nil {
		r.parents[parentSessionID] = map[string]struct{}{}
	}
	r.parents[parentSessionID][childSessionID] = struct{}{}
	r.mu.Unlock()
	return child, nil
}

// Lookup 取一个已登记的子会话。
func (r *OfficialChildRegistry) Lookup(childSessionID string) (*OfficialChild, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	child, ok := r.children[childSessionID]
	return child, ok
}

// Children 返回某个父会话名下的子会话 id（升序无关，按登记顺序）。
func (r *OfficialChildRegistry) Children(parentSessionID string) []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	set := r.parents[parentSessionID]
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}

// Prompt 把一条浏览器消息投进子会话 inbox，返回**已持久化**的 messageId。
//
// 官方契约的每一句都在这里兑现：
//   - "Identity persisted on the accepted message, minted before the call"：
//     messageId 在投递之前铸造，回执先落盘；
//   - 同一个 requestId 重投 → 回同一个 messageId（幂等，不投第二次）；
//   - 未登记子会话 → no_such_session；父 id 不匹配 → cross_session 403；
//   - 投递失败 → 返回错误（**绝不**回一个 messageId 假装成功）。
func (r *OfficialChildRegistry) Prompt(ctx context.Context, req OfficialSubagentPromptRequest) (OfficialSubagentPromptReceipt, error) {
	if r == nil {
		return OfficialSubagentPromptReceipt{}, officialNotImplemented("subagents.prompt 未接线：宿主没有子会话注册表")
	}
	child, ok := r.Lookup(req.ChildSessionID)
	if !ok {
		return OfficialSubagentPromptReceipt{}, officialNoSuchSession(req.ChildSessionID)
	}
	if child.ParentSessionID != req.ParentSessionID {
		return OfficialSubagentPromptReceipt{}, &officialError{
			status: 403, code: "cross_session",
			msg: fmt.Sprintf("子会话 %q 属于父会话 %q，不是 %q",
				req.ChildSessionID, child.ParentSessionID, req.ParentSessionID),
		}
	}
	placement, err := officialChildPlacement(req.Delivery)
	if err != nil {
		return OfficialSubagentPromptReceipt{}, err
	}

	child.mu.Lock()
	if child.closed {
		child.mu.Unlock()
		return OfficialSubagentPromptReceipt{}, officialBadRequest("子会话 %q 已经关闭", req.ChildSessionID)
	}
	if idx, dup := child.byRequest[req.RequestID]; dup {
		messageID := child.inbox[idx].MessageID
		child.mu.Unlock()
		// 幂等重投：同一个 requestId 只投一次，回同一个 messageId。
		return OfficialSubagentPromptReceipt{MessageID: messageID}, nil
	}
	messageID := fmt.Sprintf("msg-%s-%d", r.token, r.seq.Add(1))
	record := OfficialChildMessage{
		MessageID: messageID, RequestID: req.RequestID, ParentID: req.ParentSessionID,
		Delivery: req.Delivery, Content: req.Content, ClientTimeZone: req.ClientTimeZone,
		EnqueuedAt: r.opts.Now().UnixMilli(),
	}
	child.byRequest[req.RequestID] = len(child.inbox)
	child.inbox = append(child.inbox, record)
	child.mu.Unlock()

	persist := func(rec OfficialChildMessage) {
		if r.opts.Persist == nil {
			return
		}
		_ = r.opts.Persist(req.ChildSessionID, "subagent/message", rec)
	}
	persist(record)

	if r.opts.Deliver == nil {
		record.Err = "宿主没有投递通道（promptService 未接线）"
		child.mu.Lock()
		child.inbox[len(child.inbox)-1].Err = record.Err
		child.mu.Unlock()
		persist(record)
		return OfficialSubagentPromptReceipt{}, officialNotImplemented(
			"subagents.prompt 未接线：宿主没有把消息投进子会话的 prompt 通道")
	}
	if err := r.opts.Deliver(ctx, req.ChildSessionID, req.Content, placement); err != nil {
		record.Err = err.Error()
		child.mu.Lock()
		child.inbox[len(child.inbox)-1].Err = record.Err
		child.mu.Unlock()
		persist(record)
		return OfficialSubagentPromptReceipt{}, officialInternal("投递到子会话 %q 失败: %v", req.ChildSessionID, err)
	}
	child.mu.Lock()
	child.inbox[len(child.inbox)-1].Delivered = true
	child.mu.Unlock()
	record.Delivered = true
	persist(record)
	return OfficialSubagentPromptReceipt{MessageID: messageID}, nil
}

// InterruptByParent 按父授权中断子会话当前轮。
func (r *OfficialChildRegistry) InterruptByParent(childSessionID, parentSessionID string) (OfficialAccepted, error) {
	if r == nil {
		return OfficialAccepted{}, officialNotImplemented("subagents.interruptByParent 未接线：宿主没有子会话注册表")
	}
	child, ok := r.Lookup(childSessionID)
	if !ok {
		return OfficialAccepted{}, officialNoSuchSession(childSessionID)
	}
	if child.ParentSessionID != parentSessionID {
		return OfficialAccepted{}, &officialError{
			status: 403, code: "cross_session",
			msg: fmt.Sprintf("子会话 %q 属于父会话 %q，不是 %q",
				childSessionID, child.ParentSessionID, parentSessionID),
		}
	}
	if r.opts.Interrupt == nil {
		return OfficialAccepted{}, officialNotImplemented(
			"subagents.interruptByParent 未接线：宿主没有取消子会话当前轮的通道")
	}
	// 官方：accepted 只表示取消信号已被接受，不代表目标已静止。
	// 所以"没有在飞的一轮"也是 accepted——那是真实的当前状态，不是失败。
	if _, err := r.opts.Interrupt(childSessionID); err != nil {
		return OfficialAccepted{}, officialInternal("中断子会话 %q 失败: %v", childSessionID, err)
	}
	return OfficialAccepted{Accepted: true}, nil
}

// Close 关闭一个子会话地址（之后投递一律拒绝）。
func (r *OfficialChildRegistry) Close(childSessionID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	child := r.children[childSessionID]
	delete(r.children, childSessionID)
	if child != nil {
		if set := r.parents[child.ParentSessionID]; set != nil {
			delete(set, childSessionID)
			if len(set) == 0 {
				delete(r.parents, child.ParentSessionID)
			}
		}
	}
	r.mu.Unlock()
	if child != nil {
		child.mu.Lock()
		child.closed = true
		child.mu.Unlock()
	}
}

// officialChildPlacement 把官方 delivery 词表翻成 transport 的 canonical placement。
func officialChildPlacement(delivery string) (string, error) {
	switch delivery {
	case "queue":
		return string(PlacementFollowup), nil
	case "steer":
		return string(PlacementSteer), nil
	}
	return "", officialBadRequest("request.delivery 必须是 \"queue\" 或 \"steer\"（收到 %q）", delivery)
}

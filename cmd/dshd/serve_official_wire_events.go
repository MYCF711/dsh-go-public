package main

// `$events` 的**宿主事件源**：把本仓真实的审批请求接到官方 0.2.2 的转发事件流上。
//
// 为什么是审批：官方 `$events` 的 waterfall 语义是"宿主把事件当请求抛给客户端，
// 等回答才继续"（lib/index.js:1098-1150 的 startRemoteEvent）。本仓里唯一同构的
// **宿主 → 客户端请求-应答**就是审批（approvalBroker.handle）：工具跑到一半停下来问，
// 没人回答就不许执行。turn 结束是 emit（广播，不需要回答），也接在这条链上。
//
// 完整回路（全部真实组件，没有一处打桩）：
//
//	子进程 stderr 一行 `@@APPROVAL@@ {…}`
//	  → approvalBroker.handle（serve_approval.go:273）**先**登记 pending 再 emit
//	  → emit 回调（生产是 serve.go:323 的 `lr.emit`；本文件挂在同一个点上）
//	  → OnApprovalFrame → `$events` 的 waterfall 帧
//	  → 官方客户端 ClientRemoteEvents.answer()
//	  → POST /api/$events/result {clientId,eventId,outcome}
//	  → Answer → approvalBroker.Resolve（serve_approval.go:398）认领那条 pending
//	  → 等待中的 handle goroutine 调 reply → 写子进程 stdin `@@APPROVAL-RESP@@ {…}`
//	  → **宿主继续**（工具执行/不执行由这一行决定）
//
// 为什么不能"造一个假事件源"：官方客户端的 waterfall 分支会**真的**去
// `adapter.resolve(agentId)` 然后 `rpc.call('$events/result')`；一条没有真实
// pending 的 emit/waterfall 只会让客户端去答一个不存在的请求，而宿主这一侧
// 没有任何等待者——界面上的表现是"永远转圈"。所以没有事件源时宁可不发
// （见 internal/officialwire 的 EventSource 注释）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"

	"dshgo/internal/officialwire"
	"dshgo/internal/transport"
)

// 审批事件名。用官方的**同一个事件名**（不是自造一个 "approval"）：
// 官方把 Cordis 事件名原样放在 wire 的 `event` 字段上（lib/index.js:1106），
// 本仓的审批事件名就是 journal 里那一帧的 type（serve_approval.go:340 的
// `approval/request`、:358 的 `approval/timeout`、:488 的 `approval/cancel`）。
// 两处同名，界面、journal 与 wire 才不会各说各话。
const (
	approvalRequestEvent = "approval/request"
	approvalTimeoutEvent = "approval/timeout"
	approvalCancelEvent  = "approval/cancel"

	// approvalOutcomeOptionField 是 `$events/result` 的 `outcome.value` 里
	// 客户端的选项字段名。
	//
	// 官方 `outcome.value` 是"listener 的返回值"，形状由**宿主**定
	// （stream-protocol.js:38-45 只校验"无损 JSON"）。这里沿用审批负载里
	// `options[].optionId` 的同一个词，客户端不必再翻译一层。
	approvalOutcomeOptionField = "optionId"

	// rejectOptionID 是"拒绝"在审批契约里的 optionId
	// （serve_approval.go:531 的 decisionOf 认得它）。
	rejectOptionID = "reject_once"

	// approvalEventQueue 是每条 `$events` 订阅的下行队列长度。
	//
	// 满了就丢并打一行 stderr：一个连 64 条待批都读不完的客户端，让它
	// 阻塞整个审批链才是更坏的选择（那条链上还有子进程 stdin 在等）。
	approvalEventQueue = 64
)

// approvalEventSource 是 `officialwire.EventSource` 的真实实现。
//
// 并发模型：一张 `subs`（clientID → 订阅）+ 一张 `pending`（eventID → 待应答）。
// 两张表都在 s.mu 下改；**推送在锁外**（Push 会写 socket，持锁推会让审批链
// 和订阅/退订互相等）。
type approvalEventSource struct {
	broker *approvalBroker
	// newEventID 可注入（测试要确定性）；缺省 serveID（crypto/rand 的 uuid 形状）。
	newEventID func() (string, error)

	mu        sync.Mutex
	subs      map[string]*approvalSubscription
	pending   map[string]*approvalPending
	byRequest map[string]string
}

// approvalSubscription 是一条 `$events` 流的订阅。
//
// 自带的 mutex 与 closed 标志是必需的：mux 的 cancel 与 teardown 两条路径
// 都会调 stop，而对同一通道"close 两次"直接 panic —— 那会带走整条 mux 连接。
type approvalSubscription struct {
	clientID string
	events   chan officialwire.Event

	mu     sync.Mutex
	closed bool
}

// push 非阻塞投递；返回 false 表示这条订阅已经不在了或队列满。
func (s *approvalSubscription) push(event officialwire.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	select {
	case s.events <- event:
		return true
	default:
		return false
	}
}

func (s *approvalSubscription) close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.events)
	}
	s.mu.Unlock()
}

// approvalPending 是一条已经下发、等客户端回答的 waterfall。
type approvalPending struct {
	eventID   string
	requestID string
	sessionID string
	// request 是原始审批负载。重放给新订阅者时直接用它——broker 的
	// Pending 按 sessionID 索引，取不到"那一条"。
	request json.RawMessage
	// deliveries 是"这条事件已经投给哪些客户端"（官方 pending.deliveries）。
	//
	// 为什么要记而不是绑一个 clientID：官方把同一条 pending 事件投给**所有**
	// answer 型客户端，`next` 只是"这一个不管"，只有最后一个也说 next 才结算
	// （lib/index.js:1164-1176）。少记一个就会出现"两个界面里另一个再也答不了"。
	deliveries map[string]struct{}
}

// newApprovalEventSource 造事件源。broker 为 nil 时返回 nil
// （调用方据此走"没有事件源"的分支，而不是拿一个每次 Answer 都 panic 的东西）。
func newApprovalEventSource(broker *approvalBroker) *approvalEventSource {
	if broker == nil {
		return nil
	}
	return &approvalEventSource{
		broker:     broker,
		newEventID: serveID,
		subs:       map[string]*approvalSubscription{},
		pending:    map[string]*approvalPending{},
		byRequest:  map[string]string{},
	}
}

var _ officialwire.EventSource = (*approvalEventSource)(nil)

// Subscribe 开一条 `$events` 订阅。
//
// 订阅时把**当前还挂着的** waterfall 全部补投一遍（官方 lib/index.js:1055-1058
// 的 `for (const pending of this.pendingRemoteEvents.values()) this.deliverRemoteEvent(...)`）：
// 客户端断线重连时，那条"等你批准"必须重放出来，否则子进程一直等到超时被拒，
// 而用户屏幕上什么都没变。
func (s *approvalEventSource) Subscribe(ctx context.Context, clientID string) (<-chan officialwire.Event, func()) {
	sub := &approvalSubscription{clientID: clientID, events: make(chan officialwire.Event, approvalEventQueue)}

	s.mu.Lock()
	stale := s.subs[clientID]
	s.subs[clientID] = sub
	replay := make([]officialwire.Event, 0, len(s.pending))
	for eventID, pending := range s.pending {
		pending.deliveries[clientID] = struct{}{}
		replay = append(replay, officialwire.Event{
			Type:    officialwire.EventWaterfall,
			Event:   approvalRequestEvent,
			EventID: eventID,
			AgentID: pending.sessionID,
			Request: append(json.RawMessage(nil), pending.request...),
		})
	}
	s.mu.Unlock()

	// 同一个 clientId 重复订阅：旧流收掉，否则它永远收不到 end。
	if stale != nil && stale != sub {
		stale.close()
	}

	for _, event := range replay {
		sub.push(event)
	}

	var once sync.Once
	stop := func() {
		once.Do(func() {
			s.mu.Lock()
			if current, ok := s.subs[clientID]; ok && current == sub {
				delete(s.subs, clientID)
				// 这条流上的投递记录一并摘掉：之后没人能答它了。
				for _, pending := range s.pending {
					delete(pending.deliveries, clientID)
				}
			}
			s.mu.Unlock()
			sub.close()
		})
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	return sub.events, stop
}

// OnApprovalFrame 是审批 broker 的 emit 回调（生产上是 `lr.emit` 那个包装）。
//
// 三种帧的处理不同：
//
//	approval/request  → 造一条 waterfall（**需要回答**）
//	approval/timeout  → emit（广播）+ 撤销同一条请求上的 waterfall
//	approval/cancel   → emit（广播）+ 撤销同一条请求上的 waterfall
//
// 为什么 timeout/cancel 是 emit 而不是 waterfall：它们不需要任何人回答，
// 只是"这件事结束了"的通知（官方 broadcastRemoteEvent，lib/index.js:1068-1094）。
func (s *approvalEventSource) OnApprovalFrame(sessionID, typ string, data json.RawMessage) {
	if s == nil {
		return
	}
	switch typ {
	case approvalRequestEvent:
		s.startWaterfall(sessionID, data)
	case approvalTimeoutEvent, approvalCancelEvent:
		s.broadcast(typ, data)
		if requestID, ok := requestIDOf(data); ok {
			s.cancelByRequest(requestID)
		}
	}
}

// startWaterfall 把一条审批请求变成 waterfall 帧。
func (s *approvalEventSource) startWaterfall(sessionID string, data json.RawMessage) {
	requestID, ok := requestIDOf(data)
	if !ok {
		fmt.Fprintf(os.Stderr, "officialwire: 审批请求没有 requestId，无法作为 waterfall 下发：%s\n", string(data))
		return
	}
	eventID, err := s.newEventID()
	if err != nil {
		fmt.Fprintf(os.Stderr, "officialwire: 生成 waterfall eventId 失败：%v\n", err)
		return
	}
	event := officialwire.Event{
		Type:    officialwire.EventWaterfall,
		Event:   approvalRequestEvent,
		EventID: eventID,
		// agentId 用**会话 id**：官方要求非空 Agent 身份（isRemoteEventAgentId），
		// 而本仓的"谁在等这个批准"就是那个会话。客户端拿它去 resolve 一个
		// Agent；本仓没有 Agent 注册表，所以它只是关联标签，不参与路由。
		AgentID: sessionID,
		Request: data,
	}
	if failure := event.Validate(); failure != nil {
		fmt.Fprintf(os.Stderr, "officialwire: waterfall 帧不合官方形状（不下发）：%v\n", failure)
		return
	}
	pending := &approvalPending{
		eventID:    eventID,
		requestID:  requestID,
		sessionID:  sessionID,
		request:    append(json.RawMessage(nil), data...),
		deliveries: map[string]struct{}{},
	}

	s.mu.Lock()
	s.pending[eventID] = pending
	s.byRequest[requestID] = eventID
	subs := make([]*approvalSubscription, 0, len(s.subs))
	for _, sub := range s.subs {
		pending.deliveries[sub.clientID] = struct{}{}
		subs = append(subs, sub)
	}
	s.mu.Unlock()

	for _, sub := range subs {
		if !sub.push(event) {
			fmt.Fprintf(os.Stderr,
				"officialwire: 审批 %s 的下行队列已满（客户端跟不上），丢弃这条 waterfall\n", requestID)
		}
	}
}

// broadcast 把一条不需要回答的事件投给所有订阅者。
func (s *approvalEventSource) broadcast(typ string, data json.RawMessage) {
	if len(data) == 0 {
		return
	}
	args := make(json.RawMessage, 0, len(data)+2)
	args = append(args, '[')
	args = append(args, data...)
	args = append(args, ']')
	event := officialwire.Event{Type: officialwire.EventEmit, Event: typ, Args: args}
	if failure := event.Validate(); failure != nil {
		fmt.Fprintf(os.Stderr, "officialwire: emit 帧不合官方形状（不下发）：%v\n", failure)
		return
	}

	s.mu.Lock()
	subs := make([]*approvalSubscription, 0, len(s.subs))
	for _, sub := range s.subs {
		subs = append(subs, sub)
	}
	s.mu.Unlock()

	for _, sub := range subs {
		if !sub.push(event) {
			fmt.Fprintf(os.Stderr,
				"officialwire: %s 的下行队列已满（客户端跟不上），丢弃这条 emit\n", typ)
		}
	}
}

// cancelByRequest 撤销一条请求上挂着的 waterfall，并推 `{type:'cancel', eventId}`。
//
// 官方 finishRemoteEvent（lib/index.js:1194-1206）：结算时先摘表，再给每个
// 投递过的客户端推一条 cancel —— 客户端据此 abort 自己那条等待，不会挂到连接断。
func (s *approvalEventSource) cancelByRequest(requestID string) {
	s.mu.Lock()
	eventID, ok := s.byRequest[requestID]
	if !ok {
		s.mu.Unlock()
		return
	}
	pending := s.pending[eventID]
	delete(s.byRequest, requestID)
	delete(s.pending, eventID)
	subs := make([]*approvalSubscription, 0, len(pending.deliveries))
	for clientID := range pending.deliveries {
		if sub, live := s.subs[clientID]; live {
			subs = append(subs, sub)
		}
	}
	s.mu.Unlock()

	event := officialwire.Event{Type: officialwire.EventCancel, EventID: eventID}
	for _, sub := range subs {
		sub.push(event)
	}
}

// Answer 交付客户端对一条 waterfall 的 outcome。
//
// 判定逐条对齐官方 receiveRemoteEventResult（lib/index.js:1164-1176）：
//
//	未知 eventId                  → 静默 ok（早返回，不报错）
//	eventId 存在但没投给这个客户端 → gateway/interactions-forbidden
//	next                          → 摘掉这个客户端的投递；最后一个也 next 才结算
//	result                        → 立刻结算（第一个答的赢）
//	rejected                      → 按拒绝结算
func (s *approvalEventSource) Answer(clientID, eventID string, outcome officialwire.Outcome) *officialwire.Error {
	s.mu.Lock()
	pending, ok := s.pending[eventID]
	if !ok {
		s.mu.Unlock()
		// 官方：`if (pending === void 0 || !pending.deliveries.has(client)) return`
		// —— 未知 eventId 是**静默成功**，不是错误。这条事件已经被别人结算过
		// （L1 界面点了、超时了、cancel 了）。
		return nil
	}
	if _, delivered := pending.deliveries[clientID]; !delivered {
		s.mu.Unlock()
		return &officialwire.Error{
			Code:    officialwire.CodeInteractionsForbidden,
			Message: "这条转发事件不属于这个 Peer（官方：Remote event result belongs to another Peer）",
			Details: map[string]any{"endpoint": officialwire.EndpointEventResult,
				"clientId": clientID, "eventId": eventID},
		}
	}
	s.mu.Unlock()

	switch outcome.Kind {
	case officialwire.OutcomeNext:
		// "这条事件我不处理"：摘掉**这个**客户端的投递，审批本身仍然挂着
		// （L1 界面还能答）。只有最后一个投递也走了才结算。
		s.mu.Lock()
		delete(pending.deliveries, clientID)
		if len(pending.deliveries) == 0 {
			s.forgetLocked(pending)
		}
		s.mu.Unlock()
		return nil

	case officialwire.OutcomeResult:
		s.mu.Lock()
		s.forgetLocked(pending)
		s.mu.Unlock()
		optionID, failure := approvalOptionID(outcome.Value)
		if failure != nil {
			// 值形状不对 = 客户端 bug：**fail-loud**。那条 waterfall 已经从
			// 宿主表里摘掉了，所以没有等待者会挂住（审批本身走超时/别人答）。
			return failure
		}
		return s.resolve(pending, optionID)

	case officialwire.OutcomeRejected:
		s.mu.Lock()
		s.forgetLocked(pending)
		s.mu.Unlock()
		// 客户端处理时抛了：按**拒绝**结算。工具一定不执行 —— 比"当成 next
		// 放它过去"诚实得多。
		return s.resolve(pending, rejectOptionID)
	}

	// ParseEventResult 已经把未知 kind 拒了；到这里说明协议层与这里不同步。
	return officialwire.ArgumentsInvalid(officialwire.EndpointEventResult, "outcome.kind",
		"不认识的 outcome.kind："+strconv.Quote(outcome.Kind))
}

// forgetLocked 把一条 pending 从两张表里摘掉。调用方必须持有 s.mu。
func (s *approvalEventSource) forgetLocked(pending *approvalPending) {
	delete(s.pending, pending.eventID)
	if eventID, ok := s.byRequest[pending.requestID]; ok && eventID == pending.eventID {
		delete(s.byRequest, pending.requestID)
	}
}

// resolve 把选项交给**真实审批 broker**（这一步就是"宿主继续"）。
func (s *approvalEventSource) resolve(pending *approvalPending, optionID string) *officialwire.Error {
	switch err := s.broker.Resolve(pending.requestID, optionID); {
	case err == nil:
		return nil
	case errors.Is(err, transport.ErrBadOption):
		return officialwire.ArgumentsInvalid(officialwire.EndpointEventResult,
			"outcome.value."+approvalOutcomeOptionField,
			"审批没有这个选项："+strconv.Quote(optionID)+"（只有 allow_once/allow_always/reject_once）")
	case errors.Is(err, transport.ErrNoSuchRequest):
		// 已经答过 / 超时 / 被取消：官方对"事件已经不在了"是静默 ok。
		return nil
	default:
		return officialwire.Errorf(officialwire.CodeInternal, "结算审批失败：%v", err)
	}
}

// approvalOptionID 从 `outcome.value` 里取 optionId。
//
// 契约：`{"optionId":"allow_once"}`。**不**接受裸字符串——那会让"客户端传了个
// 别的东西"被静默当成一个合法选项。
func approvalOptionID(value json.RawMessage) (string, *officialwire.Error) {
	if len(value) == 0 {
		return "", officialwire.ArgumentsInvalid(officialwire.EndpointEventResult, "outcome.value",
			"outcome.kind=result 时审批需要 outcome.value."+approvalOutcomeOptionField)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(value, &payload); err != nil || payload == nil {
		return "", officialwire.ArgumentsInvalid(officialwire.EndpointEventResult, "outcome.value",
			"outcome.value 必须是对象（{"+approvalOutcomeOptionField+":\"…\"}）")
	}
	var optionID string
	if err := json.Unmarshal(payload[approvalOutcomeOptionField], &optionID); err != nil || optionID == "" {
		return "", officialwire.ArgumentsInvalid(officialwire.EndpointEventResult,
			"outcome.value."+approvalOutcomeOptionField,
			"outcome.value."+approvalOutcomeOptionField+" 必须是非空字符串")
	}
	return optionID, nil
}

// requestIDOf 从审批负载里取 requestId。
func requestIDOf(data json.RawMessage) (string, bool) {
	if len(data) == 0 {
		return "", false
	}
	var probe struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || probe.RequestID == "" {
		return "", false
	}
	return probe.RequestID, true
}

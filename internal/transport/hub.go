package transport

import (
	"reflect"
	"sync"
)

// Subscriber 收帧。返回 error 表示这个订阅者已经废了，Hub 应当把它踢掉。
type Subscriber interface {
	Push(ServerFrame) error
}

// subscription 是一条订阅记录：谁、从哪个序号之后开始收。
//
// sinceSeq 是**订阅时拍下的水位**，不是"最新收到的 seq"。Publish 只拿它跟
// frame.Seq 比：后来才补进来的旧帧（seq <= sinceSeq）一律丢掉。
// 理由写在 Subscribe 上：重连时重复比丢帧更难查。
type subscription struct {
	sub      Subscriber
	sinceSeq int
}

// Hub 是一个 topic → 订阅者的表。并发安全。
//
// topic 形如 "session:<id>"。**一个订阅者可以订多个 topic**（界面切换会话时会有短暂重叠）。
//
// 锁的纪律（很重要）：mu 只保护下面两张表，**绝不在持锁时调 sub.Push**。
// Push 会写网络，慢；更糟的是订阅者可能在自己的 Push 里回头调 Hub
// （比如发现写着写着要退订），持锁调用立刻自锁。所以 Publish 分两步：
// 持锁挑人 → 放锁推送 → 再持锁踢掉坏掉的。
type Hub struct {
	mu sync.Mutex

	// topics 存 topic → 该 topic 上的订阅者（保持订阅先后，推送顺序稳定）。
	// 用切片而不是 map[Subscriber]，是为了不必要求 Subscriber 可比较（见 sameSubscriber）。
	topics map[string][]*subscription

	// journals 是序号域（H14），只用于订阅时判断世代是否过期。
	// 为 nil 时不发 session/reset——老测试不必构造它。
	journals *Journals

	// pendingReplay 给出"这个会话上还挂着、需要重放给新连接"的帧（H13）。
	// 典型场景：审批问题问出去了，客户端断线重连——气泡没了，但请求还挂着。
	pendingReplay func(sessionID string) []ServerFrame
}

// SetPendingReplay 挂上"挂起请求重放"的回调。
func (h *Hub) SetPendingReplay(f func(sessionID string) []ServerFrame) { h.pendingReplay = f }

// SetJournals 挂上序号域：订阅时会核对 journalEpoch，过期就发 session/reset。
func (h *Hub) SetJournals(j *Journals) { h.journals = j }

// NewHub 造一个空表。
func NewHub() *Hub {
	return &Hub{topics: make(map[string][]*subscription)}
}

// Subscribe 让 sub 订阅 topic，并从 sinceSeq 之后开始收。
//
// sinceSeq 的语义：**只发 seq > sinceSeq 的帧**（0 表示从头）。
// 如果后来才补进一个 seq <= sinceSeq 的帧，**必须丢掉**——重连时重复比丢帧更难查。
// 同一个 sub 重复订阅同一个 topic：以最后一次 sinceSeq 为准。
//
// 负数按 0 处理（"比 0 大"＝全部），这样调用方少一处 if。
// sub 为 nil 时什么也不做——空订阅者只会让 Publish 每次空手而归。
func (h *Hub) Subscribe(topic string, sinceSeq int, sub Subscriber) {
	if sub == nil {
		return
	}
	if sinceSeq < 0 {
		sinceSeq = 0
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.topics == nil { // 零值 Hub 也能用
		h.topics = make(map[string][]*subscription)
	}
	list := h.topics[topic]
	for _, s := range list {
		if sameSubscriber(s.sub, sub) {
			// 重复订阅：只换水位，不换位置，也不重复收帧。
			s.sinceSeq = sinceSeq
			return
		}
	}
	h.topics[topic] = append(list, &subscription{sub: sub, sinceSeq: sinceSeq})
}

// Unsubscribe 取消 sub 在 topic 上的订阅。没订阅过也不报错。
func (h *Hub) Unsubscribe(topic string, sub Subscriber) {
	if sub == nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.dropLocked(topic, sub)
}

// UnsubscribeAll 把这个 sub 从所有 topic 上摘掉（连接断开时调）。
func (h *Hub) UnsubscribeAll(sub Subscriber) {
	if sub == nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// 直接遍历删除是安全的：Go 允许在 range 中 delete 当前 key。
	for topic := range h.topics {
		h.dropLocked(topic, sub)
	}
}

// Publish 把一帧发给所有订阅了该 topic 的 sub（只看 frame.Seq 与订阅时的 sinceSeq）。
//
// 某个 sub 的 Push 返回 error → 把它从**所有** topic 上摘掉（连接已经坏了，留着没用）。
//
// 推送在锁外做，所以多个 Publish 可能并发调用同一个 sub.Push；
// 单条连接要不要串行由 Subscriber 自己保证（写 socket 那一层本来就得串行）。
func (h *Hub) Publish(topic string, frame ServerFrame) {
	if h == nil {
		return
	}

	h.mu.Lock()
	list := h.topics[topic]
	// 挑人时就把订阅者抄出来：放锁之后表可能已经被改（Unsubscribe / 新订阅）。
	// 抄的是 *subscription 指针，所以并发改 sinceSeq 会读到新值——那没关系，
	// 语义上"以最后一次 sinceSeq 为准"正是这个意思。
	targets := make([]Subscriber, 0, len(list))
	for _, s := range list {
		if frame.Seq > s.sinceSeq {
			targets = append(targets, s.sub)
		}
	}
	h.mu.Unlock()

	if len(targets) == 0 {
		return
	}

	// 锁外推送。逐个摘掉坏掉的，不在循环里改表（改表要锁，会跟上面的纪律打架）。
	var failed []Subscriber
	for _, sub := range targets {
		if err := sub.Push(frame); err != nil {
			failed = append(failed, sub)
		}
	}

	if len(failed) == 0 {
		return
	}

	h.mu.Lock()
	for _, sub := range failed {
		for t := range h.topics {
			h.dropLocked(t, sub)
		}
	}
	h.mu.Unlock()
}

// dropLocked 把 sub 从 topic 上摘掉；topic 空了整个键删掉，免得表里堆一堆空切片。
// 调用方必须已持有 h.mu。
func (h *Hub) dropLocked(topic string, sub Subscriber) {
	list := h.topics[topic]
	if len(list) == 0 {
		return
	}

	kept := list[:0]
	for _, s := range list {
		if !sameSubscriber(s.sub, sub) {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		delete(h.topics, topic)
		return
	}
	// 把尾巴清空：kept 与 list 共用底层数组，不清会留下对已退订者的引用（GC 收不掉）。
	for i := len(kept); i < len(list); i++ {
		list[i] = nil
	}
	h.topics[topic] = kept
}

// sameSubscriber 判断两个接口值是不是同一个订阅者。
//
// 不能直接 a == b：动态类型不可比较（带切片/map 字段的结构体、func）时，
// 接口比较会 **panic**，而这里是推送路径，panic 会带走整条连接。
// 所以不可比较的类型一律当"不是同一个"——代价是那种订阅者重复订阅会收两遍，
// 比崩掉好，而且正常实现都是指针接收者。
func sameSubscriber(a, b Subscriber) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta := reflect.TypeOf(a)
	if ta != reflect.TypeOf(b) {
		return false
	}
	if !ta.Comparable() {
		return false
	}
	return a == b
}

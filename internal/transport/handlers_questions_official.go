package transport

// 本文件是官方 `userQuestions.attachWait`（`@deepseek-ai/dsh-user-questions`
// 的 `UserQuestionService.attachWait`，`@Remote({ mode: "stream" })`）在
// L1 上的**真实定时等待通道**。
//
// # 官方语义（lib/index.js 的 TimedQuestionWait）
//
//	askTimed(request, callId, timeoutMs) 先算 deadline = now + timeoutMs，
//	把 TimedQuestionWait 放进 waits[agent][callId]；attachWait 只做一件事：
//	若 wait 还在，就 yield 一帧 { remainingMs: max(0, deadline - now) }；
//	wait 结束（超时/被回答/流关闭）之后**一帧都不发**。
//
// 所以 remainingMs 不是可编造的展示值，它等于"这个前台等待还剩多久"。
//
// # 本仓的落点
//
// 本宿主的 `ask_user_question` 没有 per-call timeout（它靠结束本轮把控制权
// 交回人），所以**没有任何一个 callId 自带 deadline**。要给出真值只有两条路：
//
//	1. 明确未实现（原来那条：remainingMs 没有真值来源）；
//	2. 给宿主一个**真实生效**的前台等待窗口，deadline 从 durable 的
//	   `tool/result` 事件时间起算。
//
// 本文件走第 2 条：`OfficialQuestionWaits.Timeout` 是宿主配置的前台等待窗口
// （默认 agentQuestionWaitTimeout），deadline = 问题被问出来的那一刻 + 窗口。
// remainingMs 是**这个窗口的真实剩余**——它到 0 之后等待就结束（官方同样
// 只结束前台等待，不取消问题本身：官方超时后答案仍然投递）。
//
// 不与官方对齐的地方只有一处，且已登记在 RESULT 的"显式差异"里：
// 官方的窗口由**工具调用方**给（askTimed(timeoutMs)），本仓由**宿主**配置。
// 这是本仓 ask 工具没有 timeout 参数的直接后果，不是编出来的数。

import (
	"sync"
	"time"
)

// agentQuestionWaitTimeout 是宿主默认的前台等待窗口。
//
// 120 s：足够人读完一到四个问题并选完，又不至于让 UI 转圈到让人以为卡死。
// 官方这个值是工具传的 timeoutMs；本仓没有那个入参，所以由宿主给默认值。
const agentQuestionWaitTimeout = 120 * time.Second

// OfficialQuestionWaitOptions 装配等待表。
type OfficialQuestionWaitOptions struct {
	// Timeout 是前台等待窗口；<=0 用 agentQuestionWaitTimeout。
	Timeout time.Duration
	// Now 是时钟（测试可替换）。
	Now func() time.Time
}

// officialQuestionWait 是一次前台等待。
type officialQuestionWait struct {
	callID   string
	deadline time.Time
}

// OfficialQuestionWaits 是 (sessionID, callID) → 前台等待 的表。
//
// 并发：一把 Mutex。Arm 是**幂等**的——同一个 callId 重复 attach 拿同一个
// deadline（官方 attach 也允许同一 wait 被多个 UI 占用，deadline 不变）。
type OfficialQuestionWaits struct {
	timeout time.Duration
	now     func() time.Time

	mu    sync.Mutex
	waits map[string]map[string]*officialQuestionWait
	// settled 记下"已经结算过"的 callId，避免答案到达后又被重新 arm。
	settled map[string]map[string]struct{}
}

// NewOfficialQuestionWaits 造一张空等待表。
func NewOfficialQuestionWaits(opts OfficialQuestionWaitOptions) *OfficialQuestionWaits {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = agentQuestionWaitTimeout
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &OfficialQuestionWaits{
		timeout: timeout,
		now:     now,
		waits:   map[string]map[string]*officialQuestionWait{},
		settled: map[string]map[string]struct{}{},
	}
}

// Timeout 返回当前生效的前台等待窗口（诊断用）。
func (w *OfficialQuestionWaits) Timeout() time.Duration {
	if w == nil {
		return 0
	}
	return w.timeout
}

// Arm 占用（或复用）一个前台等待，返回**真实剩余毫秒**。
//
// askedAt 是问题被问出来的真实时刻（宿主从 durable 的 `tool/result` 事件上
// 取）。live=false 表示这个等待已经结束（超时或已结算），调用方应当**不发帧**
// ——这正是官方"wait 结束后一帧都不发"的 L1 投影。
func (w *OfficialQuestionWaits) Arm(sessionID, callID string, askedAt time.Time) (remainingMs int64, live bool) {
	if w == nil {
		return 0, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if set := w.settled[sessionID]; set != nil {
		if _, done := set[callID]; done {
			return 0, false
		}
	}
	if askedAt.IsZero() {
		askedAt = w.now()
	}
	wait := w.waits[sessionID][callID]
	if wait == nil {
		if w.waits[sessionID] == nil {
			w.waits[sessionID] = map[string]*officialQuestionWait{}
		}
		wait = &officialQuestionWait{callID: callID, deadline: askedAt.Add(w.timeout)}
		w.waits[sessionID][callID] = wait
	}
	remaining := wait.deadline.Sub(w.now())
	if remaining <= 0 {
		// 官方：deadline 已过 → close → 不再发帧。这里把它结算掉，
		// 后续 attach 一律 no-frames（不是回一个 0 骗 UI 继续转）。
		w.dropLocked(sessionID, callID)
		w.markSettledLocked(sessionID, callID)
		return 0, false
	}
	return int64(remaining / time.Millisecond), true
}

// Remaining 只读地看一眼剩余（不创建等待）。
func (w *OfficialQuestionWaits) Remaining(sessionID, callID string) (remainingMs int64, live bool) {
	if w == nil {
		return 0, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	wait := w.waits[sessionID][callID]
	if wait == nil {
		return 0, false
	}
	remaining := wait.deadline.Sub(w.now())
	if remaining <= 0 {
		return 0, false
	}
	return int64(remaining / time.Millisecond), true
}

// Settle 结算一个等待（答案到了 / 问题关掉了）。
func (w *OfficialQuestionWaits) Settle(sessionID, callID string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropLocked(sessionID, callID)
	w.markSettledLocked(sessionID, callID)
}

// Reset 清掉一个会话的全部等待状态（会话关闭/删除时用）。
func (w *OfficialQuestionWaits) Reset(sessionID string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.waits, sessionID)
	delete(w.settled, sessionID)
}

func (w *OfficialQuestionWaits) dropLocked(sessionID, callID string) {
	set := w.waits[sessionID]
	if set == nil {
		return
	}
	delete(set, callID)
	if len(set) == 0 {
		delete(w.waits, sessionID)
	}
}

func (w *OfficialQuestionWaits) markSettledLocked(sessionID, callID string) {
	if w.settled[sessionID] == nil {
		w.settled[sessionID] = map[string]struct{}{}
	}
	w.settled[sessionID][callID] = struct{}{}
}

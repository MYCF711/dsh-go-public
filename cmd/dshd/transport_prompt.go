package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"dshgo/internal/transport"
)

var (
	ErrBusy          = errors.New("上一轮还没结束")
	ErrNoSuchTurn    = errors.New("没有这一轮")
	ErrCancelTimeout = errors.New("取消等待超时：上一轮仍在收工")
)

// cancelSettleWait 是"取消时等上一轮收工"的上限。
//
// 为什么要有上限：Cancel 要等这一轮真的收工（见 liveTurn.done），但卡住的是子进程，
// 不能把 HTTP 请求也一起挂死。真机上子进程被 TerminateProcess 杀掉之后，
// 收尾（等 Wait、清写锁、补 turn/end）是几十毫秒级；15 秒是给大日志/慢磁盘留的余量。
const cancelSettleWait = 15 * time.Second

// turnRunner 是"真的跑一轮"的东西。宿主侧实现（会调模型），这里只认接口。
type turnRunner interface {
	// Run 跑到本轮结束。prompt 不等它跑完。
	Run(ctx context.Context, sessionID, turnID, text string) error
}

// contentTurnRunner 是 additive 实现；旧测试/旧 runner 不需要为实现块接口而改动。
type contentTurnRunner interface {
	RunContent(ctx context.Context, sessionID, turnID string, blocks []transport.ContentBlock) error
}

// steerTurnRunner 是 busy steer 的唯一合法入口。实现必须把 text 注入 turnID
// 对应的**当前** runner/loop 的下一 step 边界；没有真实注入能力就不要实现它。
// 返回 nil 前必须已同步确认 live inbox/loop 接管，不能只异步排队后依赖入参 ctx
// 存活（SubmitPlacement 成功后会释放本次临时 ctx）。promptService 不会把 steer
// 退成 followup，也不会凭一次排队返回 accepted。
type steerTurnRunner interface {
	Steer(ctx context.Context, sessionID, turnID, text string) error
}

// steerContentTurnRunner 是有序内容块的同义 additive 入口。
type steerContentTurnRunner interface {
	SteerContent(ctx context.Context, sessionID, turnID string, blocks []transport.ContentBlock) error
}

// promptService 管每个会话的当前轮、FIFO followup 与 now 中断预留。
//
// 三条不可动摇的规矩：
//  1. turn/排队/steer 确认立即返回，模型回答走 WS；now 只在旧轮收工后 accepted；
//  2. busy 只能在 runner 真正返回后清，队列也只能在这之后启动；
//  3. 同会话只有一个 v4 写入者。now 的 reservation 挡住旧轮结束时的自动 drain。
type promptService struct {
	mu           sync.Mutex
	bySID        map[string]*liveTurn
	queues       map[string][]*queuedTurn
	interrupting map[string]*queuedTurn
	deleting     map[string]struct{}
	seq          atomic.Int64
	run          turnRunner
	// cancelWait 是测试可缩短的等待上限；零值使用 cancelSettleWait。
	cancelWait time.Duration
	// interrupt 由 serve 注入 cancelServeTurn，保证 now 也先结算 pending approval。
	// 测试/无 broker 的调用方留空，直接取消当前 liveTurn。
	interrupt func(sessionID string) error
}

type queuedTurn struct {
	id     string
	ctx    context.Context
	cancel context.CancelFunc
	run    func(context.Context, string) error
	// 待发内容（transport 块）与它的文本形式。
	//
	// 为什么现在要留着：官方 `session.state.queue` 的 content 是必填数组，
	// `session.updateQueue` 的 edit 要改的也是**这一份**内容。以前它只被 runner
	// 闭包捕获，队列里只剩一个 TurnID，于是 state.queue 只能给 null、edit 只能 501。
	// 改完必须**真的**按新内容发，所以 runner 在启动时才读它（见 runQueued）。
	//
	// contentMu 与 p.mu 分开：runner 在别的 goroutine 里读，改内容/撤项走 p.mu，
	// 两把锁不互相等待（本锁永不回头拿 p.mu）。
	contentMu sync.Mutex
	text      string
	blocks    []transport.ContentBlock
	// done 在真实收工或尚未运行就被撤回时关闭；retireOnce 统一收敛并发
	// cancel/delete/now-timeout，任何路径都不能第二次 close。
	done       chan struct{}
	retireOnce sync.Once
}

func (q *queuedTurn) retire() {
	q.retireOnce.Do(func() {
		q.cancel()
		close(q.done)
	})
}

type liveTurn struct {
	id     string
	cancel context.CancelFunc
	// done 在这一轮**真的收工**（runner 返回）之后关闭。
	// Cancel/now 必须等它，不能与收尾中的工具或 v4 写入并发。
	done chan struct{}
	ctx  context.Context
	run  func(context.Context, string) error
	item *queuedTurn
}

func newPromptService(run turnRunner) *promptService {
	return &promptService{
		bySID:        make(map[string]*liveTurn),
		queues:       make(map[string][]*queuedTurn),
		interrupting: make(map[string]*queuedTurn),
		deleting:     make(map[string]struct{}),
		run:          run,
	}
}

// SubmitBlocks 走有序内容块 runner；没有该能力的旧 runner 明确失败，不退成拼文件名文本。
func (p *promptService) SubmitBlocks(ctx context.Context, sessionID string, blocks []transport.ContentBlock) (transport.PromptResult, error) {
	return p.SubmitBlocksPlacement(ctx, sessionID, blocks, string(transport.PlacementTurn))
}

// SubmitBlocksPlacement 是 content 的 placement 入口。busy steer 要求独立的
// SteerContent 真实能力；RunContent 本身不是 steer 证据。
func (p *promptService) SubmitBlocksPlacement(ctx context.Context, sessionID string, blocks []transport.ContentBlock, placement string) (transport.PromptResult, error) {
	canonical, err := transport.NormalizeSubmitPlacement(placement)
	if err != nil {
		return transport.PromptResult{}, err
	}
	if _, ok := p.run.(contentTurnRunner); !ok {
		return transport.PromptResult{}, fmt.Errorf("%w: 当前 runner 未实现有序 content blocks", transport.ErrPlacementUnsupported)
	}
	var item *queuedTurn
	item = newQueuedTurn(ctx, func(runCtx context.Context, turnID string) error {
		return p.runQueued(runCtx, sessionID, turnID, item)
	})
	item.setPromptContent("", blocks)
	steer := func(steerCtx context.Context, turnID string) error {
		if _, ok := p.run.(steerContentTurnRunner); !ok {
			return fmt.Errorf("%w: 当前 runner 没有真实 content steer 注入口", transport.ErrPlacementUnsupported)
		}
		text, blocks := item.promptContent()
		return p.steerInto(steerCtx, sessionID, turnID, text, blocks)
	}
	return p.submitPlacement(sessionID, canonical, item, steer)
}

// Submit 建一轮并立刻返回。同一会话已有在飞的一轮仍返回 ErrBusy；placement
// 能力全部走 additive SubmitPlacement，旧调用方的 turn 语义不变。
func (p *promptService) Submit(ctx context.Context, sessionID, text string) (transport.PromptResult, error) {
	return p.SubmitPlacement(ctx, sessionID, text, string(transport.PlacementTurn))
}

// SubmitPlacement 实现 turn/steer/followup/queue/now。queue 在 transport 层已
// canonical 成 followup。busy steer 成功回的是被注入的当前 TurnID，不创建第二轮。
func (p *promptService) SubmitPlacement(ctx context.Context, sessionID, text, placement string) (transport.PromptResult, error) {
	canonical, err := transport.NormalizeSubmitPlacement(placement)
	if err != nil {
		return transport.PromptResult{}, err
	}
	var item *queuedTurn
	item = newQueuedTurn(ctx, func(runCtx context.Context, turnID string) error {
		return p.runQueued(runCtx, sessionID, turnID, item)
	})
	item.setPromptContent(text, nil)
	steer := func(steerCtx context.Context, turnID string) error {
		text, blocks := item.promptContent()
		return p.steerInto(steerCtx, sessionID, turnID, text, blocks)
	}
	return p.submitPlacement(sessionID, canonical, item, steer)
}

func newQueuedTurn(ctx context.Context, run func(context.Context, string) error) *queuedTurn {
	runCtx, cancel := context.WithCancel(ctx)
	return &queuedTurn{ctx: runCtx, cancel: cancel, run: run, done: make(chan struct{})}
}

func (p *promptService) submitPlacement(sessionID, placement string, item *queuedTurn, steer func(context.Context, string) error) (transport.PromptResult, error) {
	p.mu.Lock()
	if _, deleting := p.deleting[sessionID]; deleting {
		p.mu.Unlock()
		item.cancel()
		return transport.PromptResult{}, ErrBusy
	}

	// steer 只有当前轮真实注入这一条路。idle/已收工必须明确报过晚，
	// 绝不能借 PlacementTurn 的 idle 分支伪造成新 turn 或 followup。
	if placement == string(transport.PlacementSteer) {
		if lt := p.bySID[sessionID]; lt != nil {
			turnID := lt.id
			p.mu.Unlock()
			if err := steer(item.ctx, turnID); err != nil {
				item.cancel()
				return transport.PromptResult{}, err
			}
			item.cancel() // steer 不拥有这条 run context，也不新建 turn。
			return transport.PromptResult{Accepted: true, TurnID: turnID}, nil
		}
		p.mu.Unlock()
		item.cancel()
		return transport.PromptResult{}, ErrSteerLate
	}

	switch placement {
	case string(transport.PlacementTurn), string(transport.PlacementSteer):
		if p.bySID[sessionID] != nil || p.interrupting[sessionID] != nil {
			p.mu.Unlock()
			item.cancel()
			return transport.PromptResult{}, ErrBusy
		}
		lt := p.startLocked(sessionID, item)
		p.mu.Unlock()
		go p.execute(sessionID, lt)
		return transport.PromptResult{Accepted: true, TurnID: item.id}, nil

	case string(transport.PlacementFollowup):
		if p.bySID[sessionID] == nil && p.interrupting[sessionID] == nil && len(p.queues[sessionID]) == 0 {
			lt := p.startLocked(sessionID, item)
			p.mu.Unlock()
			go p.execute(sessionID, lt)
			return transport.PromptResult{Accepted: true, TurnID: item.id}, nil
		}
		item.id = fmt.Sprintf("turn-%d", p.seq.Add(1))
		p.queues[sessionID] = append(p.queues[sessionID], item)
		p.mu.Unlock()
		return transport.PromptResult{Accepted: true, TurnID: item.id}, nil

	case string(transport.PlacementNow):
		if p.interrupting[sessionID] != nil {
			p.mu.Unlock()
			item.cancel()
			return transport.PromptResult{}, ErrBusy
		}
		if p.bySID[sessionID] == nil {
			lt := p.startLocked(sessionID, item)
			p.mu.Unlock()
			go p.execute(sessionID, lt)
			return transport.PromptResult{Accepted: true, TurnID: item.id}, nil
		}
		// DSH 官方 adapter 的 now = dropOwn(cancel) + followup：中断时撤掉旧 inbox，
		// 再把本项放到最前。reservation 让旧轮 done 后不会先启动新到的 followup。
		item.id = fmt.Sprintf("turn-%d", p.seq.Add(1))
		p.interrupting[sessionID] = item
		dropped := p.detachQueueLocked(sessionID)
		p.mu.Unlock()
		dropQueuedTurns(dropped)

		err := p.interruptSession(sessionID)
		if err != nil && !errors.Is(err, ErrNoSuchTurn) {
			p.abortNow(sessionID, item)
			return transport.PromptResult{}, err
		}
		return p.startNow(sessionID, item)
	}

	p.mu.Unlock()
	item.cancel()
	return transport.PromptResult{}, fmt.Errorf("%w: %q", transport.ErrPlacementInvalid, placement)
}

// startLocked 注册一个新 liveTurn；调用方必须持有 p.mu，且已确认可以启动。
func (p *promptService) startLocked(sessionID string, item *queuedTurn) *liveTurn {
	if item.id == "" {
		item.id = fmt.Sprintf("turn-%d", p.seq.Add(1))
	}
	lt := &liveTurn{id: item.id, cancel: item.cancel, done: item.done, ctx: item.ctx, run: item.run, item: item}
	p.bySID[sessionID] = lt
	return lt
}

func (p *promptService) execute(sessionID string, lt *liveTurn) {
	defer p.finish(sessionID, lt.id)
	_ = lt.run(lt.ctx, lt.id)
}

// finish 是唯一正常/错误收工出口。先关闭旧轮 done，再在同一临界区登记 FIFO
// 下一项；因此队列不会与旧轮收尾并发，也不会留下“idle 但队列没人 drain”的窗口。
func (p *promptService) finish(sessionID, turnID string) {
	p.mu.Lock()
	lt := p.bySID[sessionID]
	if lt == nil || lt.id != turnID {
		p.mu.Unlock()
		return
	}
	delete(p.bySID, sessionID)
	lt.item.retire()
	next := p.takeNextLocked(sessionID)
	p.mu.Unlock()
	if next != nil {
		go p.execute(sessionID, next)
	}
}

// takeNextLocked 在可行时取 FIFO 第一项并登记为 live；调用方持有 p.mu。
func (p *promptService) takeNextLocked(sessionID string) *liveTurn {
	if p.bySID[sessionID] != nil || p.interrupting[sessionID] != nil {
		return nil
	}
	if _, deleting := p.deleting[sessionID]; deleting {
		return nil
	}
	queue := p.queues[sessionID]
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		if item.ctx.Err() != nil {
			item.retire()
			continue
		}
		if len(queue) == 0 {
			delete(p.queues, sessionID)
		} else {
			p.queues[sessionID] = queue
		}
		return p.startLocked(sessionID, item)
	}
	delete(p.queues, sessionID)
	return nil
}

// interruptSession 是 now 的取消路径；serve 注入的函数会同时结算 pending approval。
func (p *promptService) interruptSession(sessionID string) error {
	if p.interrupt != nil {
		return p.interrupt(sessionID)
	}
	p.mu.Lock()
	lt := p.bySID[sessionID]
	p.mu.Unlock()
	if lt == nil {
		return ErrNoSuchTurn
	}
	return p.cancelLiveTurn(lt)
}

// startNow 只有旧轮已真实收工且 reservation 仍归本项时才启动 now。
func (p *promptService) startNow(sessionID string, item *queuedTurn) (transport.PromptResult, error) {
	p.mu.Lock()
	if p.interrupting[sessionID] != item {
		p.mu.Unlock()
		item.cancel()
		return transport.PromptResult{}, ErrNoSuchTurn
	}
	delete(p.interrupting, sessionID)
	if _, deleting := p.deleting[sessionID]; deleting {
		p.mu.Unlock()
		item.retire()
		return transport.PromptResult{}, ErrBusy
	}
	if p.bySID[sessionID] != nil {
		// cancel 成功却仍有 live 是状态矛盾：撤回 now、恢复普通 FIFO，
		// 明确报超时，绝不能与旧轮并发写 v4 或把 reservation 留成僵尸。
		p.interrupting[sessionID] = item
		p.mu.Unlock()
		p.abortNow(sessionID, item)
		return transport.PromptResult{}, ErrCancelTimeout
	}
	lt := p.startLocked(sessionID, item)
	p.mu.Unlock()
	go p.execute(sessionID, lt)
	return transport.PromptResult{Accepted: true, TurnID: item.id}, nil
}

// abortNow 撤回未被 accepted 的 now，并在旧轮已经空闲时恢复普通 FIFO drain。
func (p *promptService) abortNow(sessionID string, item *queuedTurn) {
	p.mu.Lock()
	if p.interrupting[sessionID] == item {
		delete(p.interrupting, sessionID)
	}
	next := p.takeNextLocked(sessionID)
	p.mu.Unlock()
	item.retire()
	if next != nil {
		go p.execute(sessionID, next)
	}
}

// Cancel 保留旧入口与官方 CancelCause=user 语义：取消当前轮、保留 FIFO。
func (p *promptService) Cancel(sessionID string) error {
	_, err := p.CancelPlacement(sessionID, "user", "")
	return err
}

// CancelPlacement 区分当前轮与队列。cause=user 保留队列；interrupt/switch/dispose
// 清空队列。turnID 命中 queued 项时只撤那一项，不取消当前轮；命中当前轮时按 cause
// 取消当前轮。超时返回错误，绝不发 confirmed receipt。
func (p *promptService) CancelPlacement(sessionID, cause, turnID string) (transport.CancelResult, error) {
	if cause == "" {
		cause = "user"
	}
	switch cause {
	case "user", "interrupt", "switch", "dispose":
	default:
		return transport.CancelResult{}, fmt.Errorf("%w: cancel cause %q", transport.ErrPlacementInvalid, cause)
	}

	p.mu.Lock()
	var dropped []*queuedTurn
	var cancelledQueued []string
	if turnID != "" {
		queue := p.queues[sessionID]
		for i, item := range queue {
			if item.id != turnID {
				continue
			}
			dropped = append(dropped, item)
			cancelledQueued = append(cancelledQueued, item.id)
			queue = append(queue[:i], queue[i+1:]...)
			if len(queue) == 0 {
				delete(p.queues, sessionID)
			} else {
				p.queues[sessionID] = queue
			}
			still := p.queueIDsLocked(sessionID)
			p.mu.Unlock()
			dropQueuedTurns(dropped)
			return transport.CancelResult{
				Cancelled: true, TurnID: turnID, CancelledQueued: cancelledQueued,
				StillQueued: still, Outcome: "confirmed",
			}, nil
		}
		if pending := p.interrupting[sessionID]; pending != nil && pending.id == turnID {
			delete(p.interrupting, sessionID)
			dropped = append(dropped, pending)
			cancelledQueued = append(cancelledQueued, pending.id)
			next := p.takeNextLocked(sessionID)
			still := p.queueIDsLocked(sessionID)
			p.mu.Unlock()
			dropQueuedTurns(dropped)
			if next != nil {
				go p.execute(sessionID, next)
			}
			return transport.CancelResult{
				Cancelled: true, TurnID: turnID, CancelledQueued: cancelledQueued,
				StillQueued: still, Outcome: "confirmed",
			}, nil
		}
	}

	lt := p.bySID[sessionID]
	if turnID != "" && (lt == nil || lt.id != turnID) {
		// 迟到回执只能撤它指向的那一轮；绝不能因为当前另有 live 就误杀新轮。
		p.mu.Unlock()
		return transport.CancelResult{}, ErrNoSuchTurn
	}

	_, deleting := p.deleting[sessionID]
	if cause != "user" || deleting {
		if pending := p.interrupting[sessionID]; pending != nil {
			delete(p.interrupting, sessionID)
			dropped = append(dropped, pending)
			cancelledQueued = append(cancelledQueued, pending.id)
		}
		queued := p.detachQueueLocked(sessionID)
		for _, item := range queued {
			cancelledQueued = append(cancelledQueued, item.id)
		}
		dropped = append(dropped, queued...)
	}
	still := p.queueIDsLocked(sessionID)
	p.mu.Unlock()
	dropQueuedTurns(dropped)

	if lt == nil {
		if len(cancelledQueued) > 0 {
			return transport.CancelResult{
				Cancelled: true, CancelledQueued: cancelledQueued,
				StillQueued: still, Outcome: "confirmed",
			}, nil
		}
		return transport.CancelResult{}, ErrNoSuchTurn
	}
	if err := p.cancelLiveTurn(lt); err != nil {
		return transport.CancelResult{}, err
	}
	// user receipt 是请求时保留队列的快照；首项在取消后已立即成为 live，
	// 也不能从 receipt 中凭空消失（官方 pending keys 同样保留到 claimed）。
	return transport.CancelResult{
		Cancelled: true, TurnID: lt.id, CancelledQueued: cancelledQueued,
		StillQueued: still, Outcome: "confirmed",
	}, nil
}

// cancelLiveTurn 请求取消并等 runner 真正收工；超时保留 liveTurn/busy。
func (p *promptService) cancelLiveTurn(lt *liveTurn) error {
	lt.cancel()
	wait := p.cancelWait
	if wait <= 0 {
		wait = cancelSettleWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-lt.done:
		return nil
	case <-timer.C:
		fmt.Fprintf(os.Stderr,
			"取消：等上一轮收工超过 %v；保留 busy 与活动轮次，待 runner 真返回后再清理\n", wait)
		return fmt.Errorf("%w（已等待 %v）", ErrCancelTimeout, wait)
	}
}

func dropQueuedTurns(items []*queuedTurn) {
	for _, item := range items {
		item.retire()
	}
}

// detachQueueLocked 只摘普通 FIFO；now reservation 由调用方单独处理。
func (p *promptService) detachQueueLocked(sessionID string) []*queuedTurn {
	items := p.queues[sessionID]
	delete(p.queues, sessionID)
	return items
}

func (p *promptService) queueIDsLocked(sessionID string) []string {
	queue := p.queues[sessionID]
	extra := 0
	if p.interrupting[sessionID] != nil {
		extra = 1
	}
	ids := make([]string, 0, len(queue)+extra)
	if pending := p.interrupting[sessionID]; pending != nil {
		ids = append(ids, pending.id)
	}
	for _, item := range queue {
		ids = append(ids, item.id)
	}
	return ids
}

// currentTurnID 返回此刻登记的 live turn；空串 = 没有活动轮次。
func (p *promptService) currentTurnID(sessionID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if lt := p.bySID[sessionID]; lt != nil {
		return lt.id
	}
	return ""
}

// cancelServeTurn 保留 dfd73da 的“先拒 pending approval、再 cancel”语义，
// 同时保留超时与同一 turnID 仍在活动表里的真实状态。
func cancelServeTurn(b *approvalBroker, p *promptService, sessionID string) error {
	before := p.currentTurnID(sessionID)
	err := cancelTurn(b, p, sessionID)
	if err == nil && before != "" && p.currentTurnID(sessionID) == before {
		return ErrCancelTimeout
	}
	return err
}

// beginDelete 加删除屏障，并同步撤回所有尚未运行的 queued/now 项。这样删除
// 完成后不会有一个内存队列再对已删目录启动“僵尸轮次”。
func (p *promptService) beginDelete(sessionID string) (release func(), err error) {
	p.mu.Lock()
	if _, exists := p.deleting[sessionID]; exists {
		p.mu.Unlock()
		return nil, ErrBusy
	}
	p.deleting[sessionID] = struct{}{}
	dropped := p.detachQueueLocked(sessionID)
	if pending := p.interrupting[sessionID]; pending != nil {
		delete(p.interrupting, sessionID)
		dropped = append(dropped, pending)
	}
	p.mu.Unlock()
	dropQueuedTurns(dropped)
	return func() {
		p.mu.Lock()
		delete(p.deleting, sessionID)
		p.mu.Unlock()
	}, nil
}

// Busy 报告当前轮、待运行队列或 now reservation 是否存在（测试与状态用）。
func (p *promptService) Busy(sessionID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bySID[sessionID] != nil || len(p.queues[sessionID]) > 0 || p.interrupting[sessionID] != nil
}

// clear 保留旧的幂等内部入口；正常收工由 execute -> finish 触发。
func (p *promptService) clear(sessionID, turnID string) {
	p.finish(sessionID, turnID)
}

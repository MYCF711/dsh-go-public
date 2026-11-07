package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"dshgo/internal/transport"
)

// 这些测试只使用可控 runner，不接触真实模型。每个 Run 的启动、正常返回、
// 取消观察和取消后的收工都由 channel 显式控制；time.After 只用于有界等待和
// “此刻不得有事件”的短断言，不用于代替 happens-before 同步。
const (
	placementEventWait   = 2 * time.Second
	placementQuietAssert = 30 * time.Millisecond
)

type placementRunControl struct {
	release       <-chan struct{}
	cancelRelease <-chan struct{}
	cancelSeen    chan struct{}
	err           error
}

type placementRunEvent struct {
	kind      string
	sessionID string
	turnID    string
	text      string
	blocks    []transport.ContentBlock
	ctx       context.Context
	err       error
}

// placementRunner 的 controls 在构造后只读，因此并发 Run 可以安全查表。
// finish 事件在 Run 的 defer 中发送；队列实现只有等 Run 真正返回后才可能启动
// 下一项，所以事件流必须严格呈现 start/finish 交替，绝不允许两个 start 相邻。
type placementRunner struct {
	controls map[string]placementRunControl
	events   chan placementRunEvent
}

func newPlacementRunner(controls map[string]placementRunControl) *placementRunner {
	return &placementRunner{controls: controls, events: make(chan placementRunEvent, 64)}
}

func (r *placementRunner) Run(ctx context.Context, sessionID, turnID, text string) error {
	return r.execute(ctx, sessionID, turnID, text, nil)
}

func (r *placementRunner) execute(ctx context.Context, sessionID, turnID, text string, blocks []transport.ContentBlock) (retErr error) {
	r.events <- placementRunEvent{
		kind: "start", sessionID: sessionID, turnID: turnID, text: text,
		blocks: append([]transport.ContentBlock(nil), blocks...), ctx: ctx,
	}
	defer func() {
		r.events <- placementRunEvent{kind: "finish", sessionID: sessionID, turnID: turnID, text: text, err: retErr}
	}()

	ctl, ok := r.controls[text]
	if !ok {
		return nil
	}
	if ctl.release == nil {
		return ctl.err
	}
	select {
	case <-ctl.release:
		return ctl.err
	case <-ctx.Done():
		if ctl.cancelSeen != nil {
			close(ctl.cancelSeen)
		}
		if ctl.cancelRelease != nil {
			<-ctl.cancelRelease
		}
		return ctx.Err()
	}
}

type placementContentRunner struct{ *placementRunner }

func (r *placementContentRunner) RunContent(ctx context.Context, sessionID, turnID string, blocks []transport.ContentBlock) error {
	text := ""
	for _, block := range blocks {
		if block.Type == "text" {
			text += block.Text
		}
	}
	return r.execute(ctx, sessionID, turnID, text, blocks)
}

type placementSteerCall struct {
	ctx       context.Context
	sessionID string
	turnID    string
	text      string
}

type placementSteerRunner struct {
	*placementRunner
	steers chan placementSteerCall
	err    error
}

func (r *placementSteerRunner) Steer(ctx context.Context, sessionID, turnID, text string) error {
	r.steers <- placementSteerCall{ctx: ctx, sessionID: sessionID, turnID: turnID, text: text}
	return r.err
}

func placementReceive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(placementEventWait):
		t.Fatalf("等待 %s 超时", what)
		var zero T
		return zero
	}
}

func placementExpectEvent(t *testing.T, r *placementRunner, kind, sessionID, text string) placementRunEvent {
	t.Helper()
	event := placementReceive(t, r.events, kind+" "+text)
	if event.kind != kind || event.sessionID != sessionID || event.text != text {
		t.Fatalf("runner 事件 = %+v，要 %s sid=%q text=%q", event, kind, sessionID, text)
	}
	return event
}

func placementExpectNoEvent(t *testing.T, r *placementRunner, why string) {
	t.Helper()
	select {
	case event := <-r.events:
		t.Fatalf("%s：意外 runner 事件 %+v", why, event)
	case <-time.After(placementQuietAssert):
	}
}

func placementMustSubmit(t *testing.T, p *promptService, sessionID, text, placement string) transport.PromptResult {
	t.Helper()
	result, err := p.SubmitPlacement(context.Background(), sessionID, text, placement)
	if err != nil {
		t.Fatalf("SubmitPlacement(%q, %q) 失败：%v", text, placement, err)
	}
	if !result.Accepted || result.TurnID == "" {
		t.Fatalf("SubmitPlacement(%q, %q) = %+v，要 accepted 且 turnID 非空", text, placement, result)
	}
	return result
}

func placementMustSubmitBlocks(t *testing.T, p *promptService, sessionID string, blocks []transport.ContentBlock, placement string) transport.PromptResult {
	t.Helper()
	result, err := p.SubmitBlocksPlacement(context.Background(), sessionID, blocks, placement)
	if err != nil {
		t.Fatalf("SubmitBlocksPlacement(%q) 失败：%v", placement, err)
	}
	if !result.Accepted || result.TurnID == "" {
		t.Fatalf("SubmitBlocksPlacement(%q) = %+v，要 accepted 且 turnID 非空", placement, result)
	}
	return result
}

func placementAssertIDs(t *testing.T, got, want []string, field string) {
	t.Helper()
	if len(got) != len(want) || !reflect.DeepEqual(append([]string(nil), got...), append([]string(nil), want...)) {
		t.Fatalf("%s = %v，要 %v", field, got, want)
	}
}

func placementAssertDistinct(t *testing.T, ids ...string) {
	t.Helper()
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			t.Fatal("turnID 不得为空")
		}
		if _, exists := seen[id]; exists {
			t.Fatalf("turnID 重复：%v", ids)
		}
		seen[id] = struct{}{}
	}
}

func TestPlacementTurnBusyStillErrBusy(t *testing.T) {
	firstRelease := make(chan struct{})
	r := newPlacementRunner(map[string]placementRunControl{
		"first": {release: firstRelease},
	})
	p := newPromptService(r)
	const sid = "placement-turn-busy"
	placementMustSubmit(t, p, sid, "first", "turn")
	placementExpectEvent(t, r, "start", sid, "first")

	result, err := p.SubmitPlacement(context.Background(), sid, "second", "turn")
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("busy turn 必须 errors.Is(err, ErrBusy)，result=%+v err=%v", result, err)
	}
	if result.Accepted {
		t.Fatalf("busy turn 不得 accepted：%+v", result)
	}
	placementExpectNoEvent(t, r, "busy turn 不得起新 Run")

	close(firstRelease)
	placementExpectEvent(t, r, "finish", sid, "first")
}

func TestFollowupQueueAcceptedFIFOAfterRunnerReturns(t *testing.T) {
	firstRelease := make(chan struct{})
	followupRelease := make(chan struct{})
	queueRelease := make(chan struct{})
	lastRelease := make(chan struct{})
	r := newPlacementRunner(map[string]placementRunControl{
		"first":      {release: firstRelease},
		"followup-1": {release: followupRelease},
		"queue-2":    {release: queueRelease},
		"followup-3": {release: lastRelease},
	})
	contentRunner := &placementContentRunner{placementRunner: r}
	p := newPromptService(contentRunner)
	const sid = "placement-fifo"
	first := placementMustSubmit(t, p, sid, "first", "turn")
	placementExpectEvent(t, r, "start", sid, "first")

	one := placementMustSubmit(t, p, sid, "followup-1", "followup")
	blocks := []transport.ContentBlock{{Type: "text", Text: "queue-"}, {Type: "text", Text: "2"}}
	two := placementMustSubmitBlocks(t, p, sid, blocks, "queue")
	three := placementMustSubmit(t, p, sid, "followup-3", "followup")
	placementAssertDistinct(t, first.TurnID, one.TurnID, two.TurnID, three.TurnID)
	placementExpectNoEvent(t, r, "前一轮未返回时 followup/queue 不得启动")

	close(firstRelease)
	placementExpectEvent(t, r, "finish", sid, "first")
	startOne := placementExpectEvent(t, r, "start", sid, "followup-1")
	if startOne.turnID != one.TurnID {
		t.Fatalf("followup-1 使用 turnID=%q，接收时是 %q", startOne.turnID, one.TurnID)
	}
	placementExpectNoEvent(t, r, "FIFO 第一项未返回时第二项不得启动")

	close(followupRelease)
	placementExpectEvent(t, r, "finish", sid, "followup-1")
	startTwo := placementExpectEvent(t, r, "start", sid, "queue-2")
	if startTwo.turnID != two.TurnID {
		t.Fatalf("queue 别名使用 turnID=%q，接收时是 %q", startTwo.turnID, two.TurnID)
	}
	if !reflect.DeepEqual(startTwo.blocks, blocks) {
		t.Fatalf("SubmitBlocksPlacement 内容块改变：got=%v want=%v", startTwo.blocks, blocks)
	}
	placementExpectNoEvent(t, r, "FIFO 第二项未返回时第三项不得启动")

	close(queueRelease)
	placementExpectEvent(t, r, "finish", sid, "queue-2")
	startThree := placementExpectEvent(t, r, "start", sid, "followup-3")
	if startThree.turnID != three.TurnID {
		t.Fatalf("followup-3 使用 turnID=%q，接收时是 %q", startThree.turnID, three.TurnID)
	}
	close(lastRelease)
	placementExpectEvent(t, r, "finish", sid, "followup-3")
	placementExpectNoEvent(t, r, "队列耗尽后不得有多余 Run")
}

func TestFollowupIdleEquivalentTurn(t *testing.T) {
	release := make(chan struct{})
	r := newPlacementRunner(map[string]placementRunControl{"idle-followup": {release: release}})
	p := newPromptService(r)
	const sid = "placement-idle-followup"
	result := placementMustSubmit(t, p, sid, "idle-followup", "followup")
	start := placementExpectEvent(t, r, "start", sid, "idle-followup")
	if start.turnID != result.TurnID {
		t.Fatalf("idle followup 未等价于直接 turn：start=%q receipt=%q", start.turnID, result.TurnID)
	}
	if !p.Busy(sid) {
		t.Fatal("idle followup 的 runner 尚未返回时必须 busy")
	}
	close(release)
	placementExpectEvent(t, r, "finish", sid, "idle-followup")
}

func TestQueueContinuesAfterRunnerError(t *testing.T) {
	boom := errors.New("controlled runner failure")
	firstRelease := make(chan struct{})
	nextRelease := make(chan struct{})
	r := newPlacementRunner(map[string]placementRunControl{
		"will-fail":     {release: firstRelease, err: boom},
		"after-failure": {release: nextRelease},
	})
	p := newPromptService(r)
	const sid = "placement-runner-error"
	placementMustSubmit(t, p, sid, "will-fail", "turn")
	placementExpectEvent(t, r, "start", sid, "will-fail")
	next := placementMustSubmit(t, p, sid, "after-failure", "followup")

	close(firstRelease)
	finish := placementExpectEvent(t, r, "finish", sid, "will-fail")
	if !errors.Is(finish.err, boom) {
		t.Fatalf("仪器 runner 没有真的返回预置错误：%v", finish.err)
	}
	start := placementExpectEvent(t, r, "start", sid, "after-failure")
	if start.turnID != next.TurnID {
		t.Fatalf("错误后的队列项 turnID=%q，要 %q", start.turnID, next.TurnID)
	}
	close(nextRelease)
	placementExpectEvent(t, r, "finish", sid, "after-failure")
}

func TestSteerPlacementInjectsLiveTurnWithoutNewRun(t *testing.T) {
	release := make(chan struct{})
	base := newPlacementRunner(map[string]placementRunControl{"live": {release: release}})
	r := &placementSteerRunner{placementRunner: base, steers: make(chan placementSteerCall, 1)}
	p := newPromptService(r)
	const sid = "placement-steer"
	live := placementMustSubmit(t, p, sid, "live", "turn")
	placementExpectEvent(t, base, "start", sid, "live")

	result, err := p.SubmitPlacement(context.Background(), sid, "steer-this-turn", "steer")
	if err != nil || !result.Accepted {
		t.Fatalf("busy steer 应注入并 accepted：result=%+v err=%v", result, err)
	}
	call := placementReceive(t, r.steers, "Steer 调用")
	if call.sessionID != sid || call.turnID != live.TurnID || call.text != "steer-this-turn" {
		t.Fatalf("Steer 参数 = sid=%q turnID=%q text=%q，要 sid=%q live turnID=%q", call.sessionID, call.turnID, call.text, sid, live.TurnID)
	}
	if call.ctx == nil {
		t.Fatal("Steer ctx 不得为 nil")
	}
	placementExpectNoEvent(t, base, "steer 不得起新 Run，也不得变成 followup")

	close(release)
	placementExpectEvent(t, base, "finish", sid, "live")
}

func TestSteerPlacementUnsupportedDoesNotQueue(t *testing.T) {
	firstRelease := make(chan struct{})
	followupRelease := make(chan struct{})
	// placementRunner 只有 Run，刻意没有 Steer；这是默认无 additive 能力的 runner。
	r := newPlacementRunner(map[string]placementRunControl{
		"live":          {release: firstRelease},
		"real-followup": {release: followupRelease},
	})
	p := newPromptService(r)
	const sid = "placement-steer-unsupported"
	placementMustSubmit(t, p, sid, "live", "turn")
	placementExpectEvent(t, r, "start", sid, "live")

	result, err := p.SubmitPlacement(context.Background(), sid, "must-not-queue", "steer")
	if !errors.Is(err, transport.ErrPlacementUnsupported) {
		t.Fatalf("无 Steer 接口时必须 errors.Is(err, transport.ErrPlacementUnsupported)，result=%+v err=%v", result, err)
	}
	if result.Accepted {
		t.Fatalf("unsupported steer 不得 accepted：%+v", result)
	}
	placementMustSubmit(t, p, sid, "real-followup", "followup")
	placementExpectNoEvent(t, r, "unsupported steer 与排队项都不得抢占 live turn")

	close(firstRelease)
	placementExpectEvent(t, r, "finish", sid, "live")
	placementExpectEvent(t, r, "start", sid, "real-followup")
	close(followupRelease)
	placementExpectEvent(t, r, "finish", sid, "real-followup")
	placementExpectNoEvent(t, r, "unsupported steer 不得在旧轮结束后僵尸启动")
}

func TestNowPlacementCancelsAndWaitsForOldRunner(t *testing.T) {
	oldRelease := make(chan struct{})
	oldCancelSeen := make(chan struct{})
	oldCancelRelease := make(chan struct{})
	newRelease := make(chan struct{})
	r := newPlacementRunner(map[string]placementRunControl{
		"old": {release: oldRelease, cancelSeen: oldCancelSeen, cancelRelease: oldCancelRelease},
		"now": {release: newRelease},
	})
	p := newPromptService(r)
	p.cancelWait = 2 * time.Second
	const sid = "placement-now"
	old := placementMustSubmit(t, p, sid, "old", "turn")
	placementExpectEvent(t, r, "start", sid, "old")
	placementMustSubmit(t, p, sid, "stale-followup", "followup") // DSH now 清旧 inbox。

	type submitResult struct {
		result transport.PromptResult
		err    error
	}
	nowDone := make(chan submitResult, 1)
	go func() {
		result, err := p.SubmitPlacement(context.Background(), sid, "now", "now")
		nowDone <- submitResult{result: result, err: err}
	}()
	placementReceive(t, oldCancelSeen, "now 取消旧 runner")
	placementExpectNoEvent(t, r, "旧 runner 未收工时 now 不得返回 finish 或启动新轮")
	select {
	case got := <-nowDone:
		t.Fatalf("旧 runner 未收工时 now 已返回：%+v", got)
	case <-time.After(placementQuietAssert):
	}

	close(oldCancelRelease)
	placementExpectEvent(t, r, "finish", sid, "old")
	startNow := placementExpectEvent(t, r, "start", sid, "now")
	got := placementReceive(t, nowDone, "now 提交结果")
	if got.err != nil || !got.result.Accepted || got.result.TurnID == "" {
		t.Fatalf("旧轮收工后 now 应 accepted：result=%+v err=%v", got.result, got.err)
	}
	placementAssertDistinct(t, old.TurnID, got.result.TurnID)
	if startNow.turnID != got.result.TurnID {
		t.Fatalf("now 新轮 turnID=%q，receipt=%q", startNow.turnID, got.result.TurnID)
	}
	if startNow.ctx.Err() != nil {
		t.Fatalf("now 新轮继承了旧轮的取消：%v", startNow.ctx.Err())
	}
	close(newRelease)
	placementExpectEvent(t, r, "finish", sid, "now")
	placementExpectNoEvent(t, r, "now 之前保留的 followup 必须按 DSH dropOwn(cancel) 语义撤回")
}

func TestNowPlacementTimeoutNotAcceptedNoNewRun(t *testing.T) {
	oldRelease := make(chan struct{})
	oldCancelSeen := make(chan struct{})
	oldCancelRelease := make(chan struct{})
	afterRelease := make(chan struct{})
	r := newPlacementRunner(map[string]placementRunControl{
		"old":   {release: oldRelease, cancelSeen: oldCancelSeen, cancelRelease: oldCancelRelease},
		"after": {release: afterRelease},
	})
	p := newPromptService(r)
	p.cancelWait = 25 * time.Millisecond
	const sid = "placement-now-timeout"
	old := placementMustSubmit(t, p, sid, "old", "turn")
	placementExpectEvent(t, r, "start", sid, "old")

	result, err := p.SubmitPlacement(context.Background(), sid, "timed-out-now", "now")
	if !errors.Is(err, ErrCancelTimeout) {
		t.Fatalf("now 等旧轮收工超时必须 ErrCancelTimeout，result=%+v err=%v", result, err)
	}
	if result.Accepted {
		t.Fatalf("now 超时不得 accepted：%+v", result)
	}
	placementReceive(t, oldCancelSeen, "超时前已请求取消旧 runner")
	if !p.Busy(sid) || p.currentTurnID(sid) != old.TurnID {
		t.Fatalf("now 超时必须保留旧 live turn %q，Busy=%v current=%q", old.TurnID, p.Busy(sid), p.currentTurnID(sid))
	}
	placementExpectNoEvent(t, r, "now 超时不得起新轮")

	close(oldCancelRelease)
	placementExpectEvent(t, r, "finish", sid, "old")
	placementExpectNoEvent(t, r, "超时返回后 now 不得延迟僵尸启动")
	waitBusy(t, p, sid, false)
	placementMustSubmit(t, p, sid, "after", "turn")
	placementExpectEvent(t, r, "start", sid, "after")
	close(afterRelease)
	placementExpectEvent(t, r, "finish", sid, "after")
}

func TestCancelPlacementUserKeepsQueueAndContinues(t *testing.T) {
	liveRelease := make(chan struct{})
	liveCancelSeen := make(chan struct{})
	secondRelease := make(chan struct{})
	thirdRelease := make(chan struct{})
	r := newPlacementRunner(map[string]placementRunControl{
		"live":   {release: liveRelease, cancelSeen: liveCancelSeen},
		"second": {release: secondRelease},
		"third":  {release: thirdRelease},
	})
	p := newPromptService(r)
	p.cancelWait = 2 * time.Second
	const sid = "placement-cancel-user"
	live := placementMustSubmit(t, p, sid, "live", "turn")
	placementExpectEvent(t, r, "start", sid, "live")
	second := placementMustSubmit(t, p, sid, "second", "followup")
	third := placementMustSubmit(t, p, sid, "third", "queue")

	result, err := p.CancelPlacement(sid, "user", "")
	if err != nil {
		t.Fatalf("CancelPlacement(user) 失败：%v", err)
	}
	placementReceive(t, liveCancelSeen, "user cancel 到达旧 runner")
	if !result.Cancelled || result.TurnID != live.TurnID || result.Outcome != "confirmed" {
		t.Fatalf("CancelPlacement(user) receipt = %+v，要 cancelled live=%q outcome=confirmed", result, live.TurnID)
	}
	placementAssertIDs(t, result.CancelledQueued, nil, "user CancelledQueued")
	placementAssertIDs(t, result.StillQueued, []string{second.TurnID, third.TurnID}, "user StillQueued")
	placementExpectEvent(t, r, "finish", sid, "live")
	placementExpectEvent(t, r, "start", sid, "second")
	close(secondRelease)
	placementExpectEvent(t, r, "finish", sid, "second")
	placementExpectEvent(t, r, "start", sid, "third")
	close(thirdRelease)
	placementExpectEvent(t, r, "finish", sid, "third")
}

func TestCancelPlacementInterruptDropsQueue(t *testing.T) {
	liveRelease := make(chan struct{})
	liveCancelSeen := make(chan struct{})
	afterRelease := make(chan struct{})
	r := newPlacementRunner(map[string]placementRunControl{
		"live":          {release: liveRelease, cancelSeen: liveCancelSeen},
		"drop-followup": {release: make(chan struct{})},
		"drop-queue":    {release: make(chan struct{})},
		"after":         {release: afterRelease},
	})
	p := newPromptService(r)
	p.cancelWait = 2 * time.Second
	const sid = "placement-cancel-interrupt"
	live := placementMustSubmit(t, p, sid, "live", "turn")
	placementExpectEvent(t, r, "start", sid, "live")
	followup := placementMustSubmit(t, p, sid, "drop-followup", "followup")
	queued := placementMustSubmit(t, p, sid, "drop-queue", "queue")

	result, err := p.CancelPlacement(sid, "interrupt", "")
	if err != nil {
		t.Fatalf("CancelPlacement(interrupt) 失败：%v", err)
	}
	placementReceive(t, liveCancelSeen, "interrupt cancel 到达旧 runner")
	if !result.Cancelled || result.TurnID != live.TurnID || result.Outcome != "confirmed" {
		t.Fatalf("CancelPlacement(interrupt) receipt = %+v，要 cancelled live=%q outcome=confirmed", result, live.TurnID)
	}
	placementAssertIDs(t, result.CancelledQueued, []string{followup.TurnID, queued.TurnID}, "interrupt CancelledQueued")
	placementAssertIDs(t, result.StillQueued, nil, "interrupt StillQueued")
	placementExpectEvent(t, r, "finish", sid, "live")
	placementExpectNoEvent(t, r, "interrupt 清掉的队列项不得运行")
	waitBusy(t, p, sid, false)

	placementMustSubmit(t, p, sid, "after", "turn")
	placementExpectEvent(t, r, "start", sid, "after")
	close(afterRelease)
	placementExpectEvent(t, r, "finish", sid, "after")
	placementExpectNoEvent(t, r, "新轮之后也不得补跑 interrupt 清掉的队列")
}

func TestCancelPlacementTurnIDWithdrawsOnlyQueuedItem(t *testing.T) {
	liveRelease := make(chan struct{})
	liveCancelSeen := make(chan struct{})
	secondRelease := make(chan struct{})
	fourthRelease := make(chan struct{})
	r := newPlacementRunner(map[string]placementRunControl{
		"live":   {release: liveRelease, cancelSeen: liveCancelSeen},
		"second": {release: secondRelease},
		"third":  {release: make(chan struct{})},
		"fourth": {release: fourthRelease},
	})
	p := newPromptService(r)
	const sid = "placement-cancel-one-queued"
	live := placementMustSubmit(t, p, sid, "live", "turn")
	liveStart := placementExpectEvent(t, r, "start", sid, "live")
	second := placementMustSubmit(t, p, sid, "second", "followup")
	third := placementMustSubmit(t, p, sid, "third", "followup")
	fourth := placementMustSubmit(t, p, sid, "fourth", "queue")
	placementAssertDistinct(t, live.TurnID, second.TurnID, third.TurnID, fourth.TurnID)

	result, err := p.CancelPlacement(sid, "interrupt", third.TurnID)
	if err != nil {
		t.Fatalf("按 queued turnID 撤销失败：%v", err)
	}
	if result.Outcome != "confirmed" {
		t.Fatalf("按 queued turnID 撤销 outcome=%q，要 confirmed", result.Outcome)
	}
	placementAssertIDs(t, result.CancelledQueued, []string{third.TurnID}, "单撤 CancelledQueued")
	placementAssertIDs(t, result.StillQueued, []string{second.TurnID, fourth.TurnID}, "单撤 StillQueued")
	if liveStart.ctx.Err() != nil {
		t.Fatalf("只撤 queued 项却取消了 live turn：%v", liveStart.ctx.Err())
	}
	select {
	case <-liveCancelSeen:
		t.Fatal("只撤 queued 项不得请求 live runner 取消")
	case <-time.After(placementQuietAssert):
	}
	placementExpectNoEvent(t, r, "live 未正常返回前剩余队列不得启动")

	close(liveRelease)
	placementExpectEvent(t, r, "finish", sid, "live")
	placementExpectEvent(t, r, "start", sid, "second")
	close(secondRelease)
	placementExpectEvent(t, r, "finish", sid, "second")
	placementExpectEvent(t, r, "start", sid, "fourth")
	close(fourthRelease)
	placementExpectEvent(t, r, "finish", sid, "fourth")
	placementExpectNoEvent(t, r, "被指定撤销的 third 不得僵尸运行")
}

func TestPlacementDeleteBarrierQueueClearsAndPreventsZombies(t *testing.T) {
	liveRelease := make(chan struct{})
	liveCancelSeen := make(chan struct{})
	liveCancelRelease := make(chan struct{})
	afterRelease := make(chan struct{})
	r := newPlacementRunner(map[string]placementRunControl{
		"live":       {release: liveRelease, cancelSeen: liveCancelSeen, cancelRelease: liveCancelRelease},
		"old-queued": {release: make(chan struct{})},
		"old-queue":  {release: make(chan struct{})},
		"after":      {release: afterRelease},
	})
	p := newPromptService(r)
	p.cancelWait = 2 * time.Second
	const sid = "placement-delete-barrier"
	placementMustSubmit(t, p, sid, "live", "turn")
	placementExpectEvent(t, r, "start", sid, "live")
	placementMustSubmit(t, p, sid, "old-queued", "followup")
	placementMustSubmit(t, p, sid, "old-queue", "queue")

	releaseDelete, err := p.beginDelete(sid)
	if err != nil {
		t.Fatalf("beginDelete 失败：%v", err)
	}
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- cancelServeTurn(nil, p, sid) }()
	placementReceive(t, liveCancelSeen, "delete 请求取消 live runner")
	placementExpectNoEvent(t, r, "delete 等旧 runner 收工时不得启动 queued 项")
	if result, err := p.SubmitPlacement(context.Background(), sid, "during-delete", "followup"); !errors.Is(err, ErrBusy) || result.Accepted {
		t.Fatalf("delete barrier 中不得接收/排队新 prompt：result=%+v err=%v", result, err)
	}

	close(liveCancelRelease)
	if err := placementReceive(t, cancelDone, "delete 等旧 runner 收工"); err != nil {
		t.Fatalf("delete 取消旧轮失败：%v", err)
	}
	placementExpectEvent(t, r, "finish", sid, "live")
	placementExpectNoEvent(t, r, "delete barrier 未释放前旧队列不得运行")
	releaseDelete()
	placementExpectNoEvent(t, r, "delete barrier 释放后旧队列也不得僵尸运行")

	placementMustSubmit(t, p, sid, "after", "turn")
	placementExpectEvent(t, r, "start", sid, "after")
	close(afterRelease)
	placementExpectEvent(t, r, "finish", sid, "after")
	placementExpectNoEvent(t, r, "delete 后新轮结束不得补跑任何旧队列项")
}

func TestPlacementQueuesAreSessionLocal(t *testing.T) {
	aLiveRelease := make(chan struct{})
	aLiveCancelSeen := make(chan struct{})
	aNextRelease := make(chan struct{})
	bLiveRelease := make(chan struct{})
	bNextRelease := make(chan struct{})
	r := newPlacementRunner(map[string]placementRunControl{
		"a-live": {release: aLiveRelease, cancelSeen: aLiveCancelSeen},
		"a-next": {release: aNextRelease},
		"b-live": {release: bLiveRelease},
		"b-next": {release: bNextRelease},
	})
	p := newPromptService(r)
	p.cancelWait = 2 * time.Second
	const sidA = "placement-session-a"
	const sidB = "placement-session-b"
	aLive := placementMustSubmit(t, p, sidA, "a-live", "turn")
	placementExpectEvent(t, r, "start", sidA, "a-live")
	placementMustSubmit(t, p, sidB, "b-live", "turn")
	bLiveStart := placementExpectEvent(t, r, "start", sidB, "b-live")
	aNext := placementMustSubmit(t, p, sidA, "a-next", "followup")
	bNext := placementMustSubmit(t, p, sidB, "b-next", "followup")

	result, err := p.CancelPlacement(sidA, "user", "")
	if err != nil {
		t.Fatalf("取消 A 失败：%v", err)
	}
	if !result.Cancelled || result.TurnID != aLive.TurnID {
		t.Fatalf("A cancel receipt = %+v，要 live=%q", result, aLive.TurnID)
	}
	placementAssertIDs(t, result.StillQueued, []string{aNext.TurnID}, "A cancel 不得混入 B 的 StillQueued")
	if bLiveStart.ctx.Err() != nil {
		t.Fatalf("取消 A 不得取消 B：%v", bLiveStart.ctx.Err())
	}
	placementReceive(t, aLiveCancelSeen, "A live 观察取消")
	placementExpectEvent(t, r, "finish", sidA, "a-live")
	placementExpectEvent(t, r, "start", sidA, "a-next")
	placementExpectNoEvent(t, r, "A 的队列推进不得启动 B 的 queued 项")

	close(aNextRelease)
	placementExpectEvent(t, r, "finish", sidA, "a-next")
	placementExpectNoEvent(t, r, "A 队列耗尽不得消费 B 队列")
	close(bLiveRelease)
	placementExpectEvent(t, r, "finish", sidB, "b-live")
	startB := placementExpectEvent(t, r, "start", sidB, "b-next")
	if startB.turnID != bNext.TurnID {
		t.Fatalf("B queued turnID=%q，receipt=%q", startB.turnID, bNext.TurnID)
	}
	close(bNextRelease)
	placementExpectEvent(t, r, "finish", sidB, "b-next")
}

func TestCancelPlacementStaleTurnIDDoesNotCancelReplacement(t *testing.T) {
	firstRelease := make(chan struct{})
	secondRelease := make(chan struct{})
	r := newPlacementRunner(map[string]placementRunControl{
		"first":  {release: firstRelease},
		"second": {release: secondRelease},
	})
	p := newPromptService(r)
	const sid = "placement-stale-cancel"
	first := placementMustSubmit(t, p, sid, "first", "turn")
	placementExpectEvent(t, r, "start", sid, "first")
	close(firstRelease)
	placementExpectEvent(t, r, "finish", sid, "first")
	second := placementMustSubmit(t, p, sid, "second", "turn")
	secondStart := placementExpectEvent(t, r, "start", sid, "second")

	result, err := p.CancelPlacement(sid, "interrupt", first.TurnID)
	if !errors.Is(err, ErrNoSuchTurn) || result.Cancelled {
		t.Fatalf("stale turnID 必须 ErrNoSuchTurn 且不 cancelled：result=%+v err=%v", result, err)
	}
	if secondStart.ctx.Err() != nil || p.currentTurnID(sid) != second.TurnID {
		t.Fatalf("stale cancel 误杀 replacement：ctx=%v current=%q", secondStart.ctx.Err(), p.currentTurnID(sid))
	}
	close(secondRelease)
	placementExpectEvent(t, r, "finish", sid, "second")
}

func TestNowPlacementConcurrentRetractionAndFailureRetiresOnce(t *testing.T) {
	oldRelease := make(chan struct{})
	r := newPlacementRunner(map[string]placementRunControl{"old": {release: oldRelease}})
	p := newPromptService(r)
	interruptSeen := make(chan struct{})
	interruptRelease := make(chan struct{})
	interruptErr := errors.New("controlled interrupt failure")
	p.interrupt = func(string) error {
		close(interruptSeen)
		<-interruptRelease
		return interruptErr
	}
	const sid = "placement-now-retire-once"
	placementMustSubmit(t, p, sid, "old", "turn")
	placementExpectEvent(t, r, "start", sid, "old")

	type submitResult struct {
		result transport.PromptResult
		err    error
	}
	nowDone := make(chan submitResult, 1)
	go func() {
		result, err := p.SubmitPlacement(context.Background(), sid, "now", "now")
		nowDone <- submitResult{result: result, err: err}
	}()
	placementReceive(t, interruptSeen, "now 进入取消等待")
	p.mu.Lock()
	pending := p.interrupting[sid]
	p.mu.Unlock()
	if pending == nil {
		t.Fatal("now 等待期间必须有 interrupt reservation")
	}
	if _, err := p.CancelPlacement(sid, "interrupt", pending.id); err != nil {
		t.Fatalf("并发撤回 now reservation 失败：%v", err)
	}
	close(interruptRelease)
	got := placementReceive(t, nowDone, "now 取消失败结果")
	if !errors.Is(got.err, interruptErr) || got.result.Accepted {
		t.Fatalf("now 取消失败必须原样返回且不 accepted：result=%+v err=%v", got.result, got.err)
	}

	close(oldRelease)
	placementExpectEvent(t, r, "finish", sid, "old")
	placementExpectNoEvent(t, r, "已撤回且失败的 now 不得运行或 double close")
	waitBusy(t, p, sid, false)
}

func (e placementRunEvent) String() string {
	return fmt.Sprintf("%s(%s/%s/%s)", e.kind, e.sessionID, e.turnID, e.text)
}

package main

// H13 pending cancel：审批还挂着的时候客户端发 `session.cancel`。
//
// 这个文件钉的是四件事（缺一条就不算收口）：
//
//	① 那个挂着的审批要**按拒绝结算**（不是超时、不是静默丢弃）；
//	② 界面要看得到明确的 `approval/cancel`（契约白名单里的那个词，不自创）；
//	③ 取消之后超时那条 goroutine 必须闭嘴——子进程每轮的 requestId 都从 1 重新编，
//	   它两分钟后会把一行过期应答写进**下一个子进程**的 stdin；
//	④ 取消不能把会话搞死：第二轮 prompt 还得起得来，而且取消返回时上一轮必须已经收工
//	   （否则第二轮的子进程会抢到死进程留下的会话写锁）。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dshgo/internal/transport"
)

// turnRunnerFunc 让"一轮跑什么"写成函数字面量（这个文件里要好几份不同的）。
type turnRunnerFunc func(ctx context.Context, sessionID, turnID, text string) error

func (f turnRunnerFunc) Run(ctx context.Context, sessionID, turnID, text string) error {
	return f(ctx, sessionID, turnID, text)
}

// waitFrames 等到 emitted 至少攒够 n 帧，返回快照。
// 超时就 Fatal：**没有结论不等于通过**。
func waitFrames(t *testing.T, e *emitLog, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.len() >= n {
			return e.all()
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等 %d 帧超时，只有 %v", n, e.all())
	return nil
}

// waitLines 等到 stdin 至少写了 n 行。
func waitLines(t *testing.T, f *fakeStdin, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.count() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等子进程应答超时：只写了 %d 行", f.count())
}

// hasType 判断帧序列里有没有某个 type。
func hasType(types []string, want string) bool {
	for _, s := range types {
		if s == want {
			return true
		}
	}
	return false
}

// ① ② 取消要把挂着的审批按**拒绝**结算，并推 `approval/cancel`。
func TestBrokerCancelRejectsPendingApproval(t *testing.T) {
	stdin := &fakeStdin{}
	emitted := &emitLog{}
	b := newApprovalBroker(stdin, func(sid, typ string, data json.RawMessage) {
		emitted.push(typ)
	})

	b.Watch(strings.NewReader(approvalRequestTag+
		`{"requestId":"perm-c1","sessionId":"s1","tool":"write","args":{"path":"x.txt"}}`+"\n"), nil)
	if got := waitFrames(t, emitted, 1); got[0] != "approval/request" {
		t.Fatalf("该先推 approval/request，实际 %v", got)
	}

	if n := b.CancelSession("s1"); n != 1 {
		t.Fatalf("cancel 该结算 1 条挂起审批，实际 %d（挂起审批没被取消）", n)
	}

	// 界面上要看得到明确的一帧：契约白名单里叫 approval/cancel。
	got := waitFrames(t, emitted, 2)
	if got[1] != "approval/cancel" {
		t.Fatalf("cancel 之后该推 approval/cancel，实际 %v", got)
	}

	// 拒绝必须**写回**子进程：只把记录删掉，子进程会一直等到超时，
	// 而这段时间界面已经看不到这条问题了。
	waitLines(t, stdin, 1)
	line := stdin.wrote(0)
	if !strings.Contains(line, approvalResponseTag) ||
		!strings.Contains(line, `"allow":false`) || !strings.Contains(line, "perm-c1") {
		t.Fatalf("取消必须按拒绝写回子进程，实际 %q", line)
	}

	// 取消了就不该再重放，也不该还能被回答。
	if pending := b.Pending("s1"); len(pending) != 0 {
		t.Fatalf("取消之后不该再重放，实际 %d 条", len(pending))
	}
	if err := b.Resolve("perm-c1", "allow_once"); err != transport.ErrNoSuchRequest {
		t.Fatalf("取消之后再答要 404（ErrNoSuchRequest），实际 %v", err)
	}
}

// 取消只结算**这个会话**的审批，别把别的会话一起拒掉。
func TestBrokerCancelOnlyTouchesItsSession(t *testing.T) {
	emitted := &emitLog{}
	b := newApprovalBroker(&fakeStdin{}, func(sid, typ string, data json.RawMessage) {
		emitted.push(typ)
	})
	b.Watch(strings.NewReader(
		approvalRequestTag+`{"requestId":"perm-a","sessionId":"s1","tool":"write"}`+"\n"+
			approvalRequestTag+`{"requestId":"perm-b","sessionId":"s2","tool":"bash"}`+"\n"), nil)
	waitFrames(t, emitted, 2)

	if n := b.CancelSession("s1"); n != 1 {
		t.Fatalf("只该取消 s1 的那一条，实际 %d 条", n)
	}
	if pending := b.Pending("s2"); len(pending) != 1 {
		t.Fatalf("s2 的审批被连累了：还剩 %d 条", len(pending))
	}
	if err := b.Resolve("perm-b", "allow_once"); err != nil {
		t.Fatalf("s2 的审批仍该可答：%v", err)
	}
}

// ③ 取消之后，超时那条 goroutine 必须闭嘴。
//
// 为什么这条最要紧：每轮都会起一个**新子进程**，它的 requestId 又从 perm-1 开始编。
// 取消时只删表不通知，两分钟后那行 `allow:false` 会写进新子进程的 stdin，
// 命中它这一轮某个真实请求——用户看到的是"我明明没答，它自己拒了"。
func TestBrokerCancelStopsStaleTimeout(t *testing.T) {
	stdin := &fakeStdin{}
	emitted := &emitLog{}
	b := newApprovalBroker(stdin, func(sid, typ string, data json.RawMessage) {
		emitted.push(typ)
	})
	b.timeout = 40 * time.Millisecond

	b.Watch(strings.NewReader(approvalRequestTag+
		`{"requestId":"perm-c3","sessionId":"s1","tool":"write"}`+"\n"), nil)
	waitFrames(t, emitted, 1)
	if n := b.CancelSession("s1"); n != 1 {
		t.Fatalf("该取消 1 条，实际 %d", n)
	}

	// 下一轮起来了：换一根新的 stdin（每轮一个新子进程）。
	next := &fakeStdin{}
	b.useStdin(next)

	// 等过原来的超时点：该到的"过期应答"就是这时候会来。
	time.Sleep(200 * time.Millisecond)

	if next.count() != 0 {
		t.Fatalf("取消后超时把应答写进了下一个子进程：%q", next.wrote(0))
	}
	if hasType(emitted.all(), "approval/timeout") {
		t.Fatalf("已经取消的审批不该再推 approval/timeout：%v", emitted.all())
	}
}

// ④ 取消要同时做完两件事：审批按拒绝结算 + 这一轮让位（第二轮立刻能起）。
func TestCancelTurnRejectsApprovalAndFreesSession(t *testing.T) {
	emitted := &emitLog{}
	stdin := &fakeStdin{}
	b := newApprovalBroker(stdin, func(sid, typ string, data json.RawMessage) {
		emitted.push(typ)
	})

	var calls atomic.Int32
	started := make(chan struct{}, 1)
	run := turnRunnerFunc(func(ctx context.Context, _, _, _ string) error {
		if calls.Add(1) == 1 {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		return nil // 第二轮：立刻收工，验的是"起得来"
	})
	p := newPromptService(run)
	if _, err := p.Submit(context.Background(), "s1", "写个文件"); err != nil {
		t.Fatalf("第一轮该起得来：%v", err)
	}
	<-started

	b.Watch(strings.NewReader(approvalRequestTag+
		`{"requestId":"perm-c4","sessionId":"s1","tool":"write"}`+"\n"), nil)
	waitFrames(t, emitted, 1)

	if err := cancelTurn(b, p, "s1"); err != nil {
		t.Fatalf("cancelTurn 失败：%v", err)
	}
	if !hasType(emitted.all(), "approval/cancel") {
		t.Fatalf("取消之后界面该收到 approval/cancel，实际 %v", emitted.all())
	}
	if p.Busy("s1") {
		t.Fatal("取消之后还留着 busy——第二轮会被 409 挡住")
	}
	if _, err := p.Submit(context.Background(), "s1", "再来一次"); err != nil {
		t.Fatalf("取消之后第二轮起不来（会话被搞死了）：%v", err)
	}
	p.Cancel("s1")
}

// ④ 取消返回时，上一轮必须**真的收工**。
//
// 这一条不是洁癖：子进程被 TerminateProcess 杀掉时没有 defer 可跑，
// 会话写锁留在磁盘上；那份锁是在 loopRunner 里按 pid 清掉的。
// Cancel 若在收工前就返回，第二轮的子进程会去抢死进程留下的锁，直接报
// "会话正被另一个进程写入"——界面上就是"取消一次，这个会话再也发不出去了"。
func TestCancelWaitsForTurnToFinish(t *testing.T) {
	var finished atomic.Bool
	started := make(chan struct{}, 1)
	run := turnRunnerFunc(func(ctx context.Context, _, _, _ string) error {
		started <- struct{}{}
		<-ctx.Done()
		// 模拟子进程退出 + 补 turn/end + 清写锁的那点收尾时间。
		time.Sleep(150 * time.Millisecond)
		finished.Store(true)
		return ctx.Err()
	})
	p := newPromptService(run)
	if _, err := p.Submit(context.Background(), "s1", "写个文件"); err != nil {
		t.Fatal(err)
	}
	<-started

	if err := p.Cancel("s1"); err != nil {
		t.Fatalf("Cancel 失败：%v", err)
	}
	if !finished.Load() {
		t.Fatal("Cancel 在上一轮收工之前就返回了——下一轮会撞上死进程留下的会话写锁")
	}
}

// pending approval 已按 dfd73da 结算，不代表卡住的工具已经收工。
// cancelTurn 会为“审批已拒 + 轮次恰好结束”吞 ErrNoSuchTurn；serve 入口必须
// 识别同一 turnID 仍在活动表里的情形，恢复超时错误而不是假报取消完成。
func TestCancelServeTurnTimeoutWithPendingApprovalKeepsLiveState(t *testing.T) {
	emitted := &emitLog{}
	stdin := &fakeStdin{}
	b := newApprovalBroker(stdin, func(sid, typ string, data json.RawMessage) {
		emitted.push(typ)
	})
	started := make(chan struct{})
	releaseTool := make(chan struct{})
	p := newPromptService(turnRunnerFunc(func(ctx context.Context, _, _, _ string) error {
		close(started)
		<-ctx.Done()
		<-releaseTool
		return ctx.Err()
	}))
	p.cancelWait = 20 * time.Millisecond
	if _, err := p.Submit(context.Background(), "s1", "写个文件"); err != nil {
		t.Fatal(err)
	}
	<-started
	b.Watch(strings.NewReader(approvalRequestTag+
		`{"requestId":"perm-cancel-timeout","sessionId":"s1","tool":"write"}`+"\n"), nil)
	waitFrames(t, emitted, 1)

	err := cancelServeTurn(b, p, "s1")
	if !errors.Is(err, ErrCancelTimeout) {
		t.Fatalf("pending 已拒但工具未收工时应报 ErrCancelTimeout，实际 %v", err)
	}
	if !hasType(emitted.all(), "approval/cancel") {
		t.Fatalf("pending approval 没有按原语义推 approval/cancel：%v", emitted.all())
	}
	if !p.Busy("s1") {
		t.Fatal("pending cancel 超时后 live turn 被清成空闲")
	}
	close(releaseTool)
	waitBusy(t, p, "s1", false)
}

// 清写锁只认**刚退出的那个 pid**：别人的锁删掉就等于把"同一份会话只有一个写入者"拆了。
func TestDropStaleLockOnlyForExitedChild(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.v4.jsonl.zstd")
	lock := path + ".lock"
	if err := os.WriteFile(lock, []byte("pid=4242\nat=2026-11-04T19:00:00+08:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := dropStaleLock(path, 9999); err != nil {
		t.Fatalf("不匹配的 pid 不该报错：%v", err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("别人的锁被删了：%v", err)
	}

	if err := dropStaleLock(path, 4242); err != nil {
		t.Fatalf("删自己的锁失败：%v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("刚退出的子进程留下的锁还在（下一轮会撞锁）：%v", err)
	}
}

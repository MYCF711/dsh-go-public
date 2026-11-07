package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
)

// 这一份测试直接钉 turnCloser 本身：它是"一条 turn 只有一条 turn/end"的那道闸。
//
// 上面 turn_end_test.go 从真实入口（run / ACP）证明闭合；这里把闸门的三个
// 性质单独钉死，避免以后有人"顺手"把它改成两条路各自 Append：
//   - 幂等：写过就不再写（重复会让消费者数错轮次）；
//   - 分类：取消是 aborted，其余失败是 error，绝不 completed；
//   - panic：展开时也要闭合，而且 panic 必须原样抛回。

func newD1Log(t *testing.T) *session.Log {
	t.Helper()
	home := t.TempDir()
	l, err := session.CreateSession(home, "/tmp/d1-closer", "d1-closer", "default")
	if err != nil {
		t.Fatal(err)
	}
	// t.TempDir 在 Windows 上要等文件句柄关掉才能删；Cleanup 后进先出，
	// 这里注册得比 TempDir 晚，所以先执行。
	t.Cleanup(func() { _ = l.Close() })
	if err := l.Append("turn/start", session.TurnStartData(1)); err != nil {
		t.Fatal(err)
	}
	return l
}

func mustReadV4(t *testing.T, path string) *v4.Log {
	t.Helper()
	back, err := v4.ReadFile(path)
	if err != nil {
		t.Fatalf("读日志 %s 失败: %v", path, err)
	}
	return back
}

// TestTurnCloserWritesExactlyOneEnd：EventTurnEnd 写了之后，出口兜底不许再写。
func TestTurnCloserWritesExactlyOneEnd(t *testing.T) {
	l := newD1Log(t)
	tc := newTurnCloser(l, 1)
	if err := tc.End("stop"); err != nil {
		t.Fatal(err)
	}
	if err := tc.Exit(context.Background(), nil); err != nil {
		t.Fatalf("出口兜底在已闭合时不该报错: %v", err)
	}
	d1AssertClosed(t, l.Path, "completed", "")
}

// TestTurnCloserExitClassifiesFailures：失败路径的 kind 必须是 error，且带上原因。
func TestTurnCloserExitClassifiesFailures(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		kind string
		has  string
	}{
		{"普通错误", context.Background(), errors.New("模型调用失败: 502"), "error", "502"},
		{"步数用满", context.Background(), errors.New("turn 用满了 1 步仍未收敛（模型一直在请求工具）"), "error", "用满"},
		{"取消", canceledCtx(t), nil, "aborted", "取消"},
		{"超时", deadlineCtx(t), nil, "aborted", "超时"},
		{"错误本身是取消", context.Background(), context.Canceled, "aborted", "取消"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newD1Log(t)
			tc := newTurnCloser(l, 1)
			if err := tc.Exit(c.ctx, c.err); err != nil {
				t.Fatal(err)
			}
			d1AssertClosed(t, l.Path, c.kind, c.has)
		})
	}
}

// canceledCtx / deadlineCtx 造一个"已经结束"的 ctx（不靠 sleep）。
func canceledCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func deadlineCtx(t *testing.T) context.Context {
	t.Helper()
	// 截止时间放在过去：ctx 一造出来就是 DeadlineExceeded。
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	t.Cleanup(cancel)
	return ctx
}

// TestTurnCloserPanicStillCloses：panic 展开时补结局，且 panic 不被吞掉。
func TestTurnCloserPanicStillCloses(t *testing.T) {
	l := newD1Log(t)
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("panic 必须原样抛回：吞掉它等于把真 bug 变成一次普通失败")
			}
		}()
		tc := newTurnCloser(l, 1)
		defer func() {
			if r := recover(); r != nil {
				_ = tc.ExitOnPanic(r)
				panic(r)
			}
		}()
		panic("boom")
	}()
	d1AssertClosed(t, l.Path, "error", "panic")
}

// TestSettleTurnClosesDanglingOnce：serve 的补写只补没闭合的那一轮，且只补一次。
func TestSettleTurnClosesDanglingOnce(t *testing.T) {
	home := t.TempDir()
	sid := "d1-settle"
	l, err := session.CreateSession(home, "/tmp/d1-settle", sid, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append("turn/start", session.TurnStartData(1)); err != nil {
		t.Fatal(err)
	}
	if err := l.Append("user/message", session.UserMessage(nil)); err != nil {
		t.Fatal(err)
	}
	path := l.Path
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	r := &loopRunner{home: home}
	turn, err := r.settleTurn(context.Background(), sid, 0, errors.New("子进程崩了"))
	if err != nil {
		t.Fatalf("补写失败: %v", err)
	}
	if turn != 1 {
		t.Fatalf("补写的 turn = %d，期望 1", turn)
	}
	d1AssertClosed(t, path, "error", "子进程崩了")

	// 第二次：日志已闭合，不许再写第二条。
	turn, err = r.settleTurn(context.Background(), sid, 0, errors.New("再来一次"))
	if err != nil {
		t.Fatal(err)
	}
	if turn != 0 {
		t.Fatalf("已闭合的日志不该再补，实际补了 turn=%d", turn)
	}
	if c := d1ReadClosure(t, path); len(c.Ends) != 1 {
		t.Fatalf("补了两次：turn/end = %d 条", len(c.Ends))
	}
}

// TestSettleTurnClearsDeadChildLock：子进程被硬杀会留下写锁；
// 只有锁里记的 pid 就是刚退出的那个子进程时才允许清掉。
func TestSettleTurnClearsDeadChildLock(t *testing.T) {
	home := t.TempDir()
	sid := "d1-settle-lock"
	l, err := session.CreateSession(home, "/tmp/d1-settle-lock", sid, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append("turn/start", session.TurnStartData(1)); err != nil {
		t.Fatal(err)
	}
	path := l.Path
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	const childPID = 424242
	if err := os.WriteFile(path+".lock", []byte("pid=424242\nat=now\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &loopRunner{home: home}
	if turn, err := r.settleTurn(context.Background(), sid, childPID, errors.New("被取消")); err != nil || turn != 1 {
		t.Fatalf("应清掉已退出子进程的锁并补写：turn=%d err=%v", turn, err)
	}
	d1AssertClosed(t, path, "error", "被取消")
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("补写完成后锁文件应当已被释放，实际 stat err=%v", err)
	}
}

// TestSettleTurnRefusesForeignLock：锁属于别人（pid 不同）时不许碰。
func TestSettleTurnRefusesForeignLock(t *testing.T) {
	home := t.TempDir()
	sid := "d1-settle-foreign"
	l, err := session.CreateSession(home, "/tmp/d1-settle-foreign", sid, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append("turn/start", session.TurnStartData(1)); err != nil {
		t.Fatal(err)
	}
	path := l.Path
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".lock", []byte("pid=777\nat=now\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &loopRunner{home: home}
	if _, err := r.settleTurn(context.Background(), sid, 424242, errors.New("x")); err == nil {
		t.Fatal("锁属于别的进程时必须拒绝，不能删")
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("别人的锁被删了：%v", err)
	}
}

// TestLoopRunnerSettlesTurnWhenChildDies 是 serve 那条 call site 的证据：
// 子进程非零退出、日志里留着悬挂 turn 时，Run 必须补一条结局才返回。
//
// 子进程仍然是测试二进制（serve_loop_test.go 的 init 拦截 `run ...`）：
// 这里让它的 argv 落盘写不出去（目录不存在），于是它 exit(92)，
// loopRunner 拿到 werr != nil —— 正是硬杀/崩溃那条路。
func TestLoopRunnerSettlesTurnWhenChildDies(t *testing.T) {
	home := t.TempDir()
	sid := "d1-serve-settle"
	l, err := session.CreateSession(home, "/tmp/d1-serve", sid, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append("turn/start", session.TurnStartData(1)); err != nil {
		t.Fatal(err)
	}
	path := l.Path
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DSHD_MODEL_WIRE_ARGV", filepath.Join(t.TempDir(), "no-such-dir", "argv.json"))
	t.Setenv("DSHD_SERVE_NO_FOLLOW", "1")
	r := &loopRunner{home: home, policy: "deny", maxSteps: 2, model: "m"}
	if err := r.Run(context.Background(), sid, "turn-1", "hi"); err == nil {
		t.Fatal("子进程非零退出时 Run 必须报错（不许吞）")
	}
	d1AssertClosed(t, path, "error", "子进程")
}

// TestOpenTurnStateMachine 钉住"最后一个没闭合的 turn"的判定。
func TestOpenTurnStateMachine(t *testing.T) {
	home := t.TempDir()
	l, err := session.CreateSession(home, "/tmp/d1-open", "d1-open", "default")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	app := func(ev string, d any) {
		if err := l.Append(ev, d); err != nil {
			t.Fatal(err)
		}
	}
	app("turn/start", session.TurnStartData(1))
	app("turn/end", session.TurnEndData(1, "completed", ""))
	app("turn/start", session.TurnStartData(1))
	if got := openTurn(mustReadV4(t, l.Path)); got != 1 {
		t.Fatalf("第二轮开了没闭合，openTurn = %d，期望 1", got)
	}
	app("turn/end", session.TurnEndData(1, "aborted", "x"))
	if got := openTurn(mustReadV4(t, l.Path)); got != 0 {
		t.Fatalf("全部闭合后 openTurn = %d，期望 0", got)
	}
}

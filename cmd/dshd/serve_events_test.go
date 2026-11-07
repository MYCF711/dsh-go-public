package main

import (
	"errors"
	"strings"
	"testing"

	"dshgo/internal/transport"
)

// 失败必须推出去：prompt 已经回 200 了，这是界面唯一的消息来源。
func TestPublishTurnError(t *testing.T) {
	var got []transport.ServerFrame
	pub := func(topic string, f transport.ServerFrame) { got = append(got, f) }
	emitTurnFrame(transport.NewJournals(nil), pub, "a", "turn-1", errors.New("模型连不上"))
	if len(got) != 1 {
		t.Fatalf("该推一帧，推了 %d", len(got))
	}
	if got[0].Type != TurnError {
		t.Fatalf("type 要 %q，拿到 %q", TurnError, got[0].Type)
	}
	// 错误原文照抄，不许换成人话
	if s := string(got[0].Data); !strings.Contains(s, "模型连不上") {
		t.Fatalf("错误原文被改了: %s", s)
	}
	if got[0].Seq <= 0 {
		t.Fatalf("seq 必须 > 0，拿到 %d", got[0].Seq)
	}
}

func TestPublishTurnDone(t *testing.T) {
	var got []transport.ServerFrame
	emitTurnFrame(transport.NewJournals(nil),
		func(_ string, f transport.ServerFrame) { got = append(got, f) }, "a", "turn-2", nil)
	if len(got) != 1 || got[0].Type != TurnDone {
		t.Fatalf("拿到 %+v", got)
	}
}

// pub 为 nil 时不许 panic——serve 关掉时 pub 可能已经被置空。
func TestPublishTurnQuiet(t *testing.T) {
	emitTurnFrame(transport.NewJournals(nil), nil, "a", "t", errors.New("x"))
}

// 结局帧的 seq 必须接在事件之后（H14）：
// 以前它来自全局 serveTurnSeq，和事件帧用的 v4 seq 是两个域；
// 现在两者都在同一条 journal 里发号，所以"事件 1..3 之后是 4"是构造上成立的。
func TestTurnSeqContinuesFromEvents(t *testing.T) {
	js := transport.NewJournals(nil)
	var got []int
	pub := func(_ string, f transport.ServerFrame) { got = append(got, f.Seq) }

	emitFrame(js, pub, "a", "user/message", nil)      // 1
	emitFrame(js, pub, "a", "assistant/message", nil) // 2
	emitTurnFrame(js, pub, "a", "t1", nil)            // 3
	emitTurnFrame(js, pub, "a", "t2", nil)            // 4

	want := []int{1, 2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("帧数不对：%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("seq 应是 %v，实际 %v", want, got)
		}
	}
}

// 两条会话的结局帧各自从 1 开始——**这正是旧全局计数器做不到的**。
func TestTurnSeqIsPerSession(t *testing.T) {
	js := transport.NewJournals(nil)
	var a, b []int
	emitFrame(js, func(_ string, f transport.ServerFrame) { a = append(a, f.Seq) }, "sa", "text", nil)
	emitFrame(js, func(_ string, f transport.ServerFrame) { b = append(b, f.Seq) }, "sb", "text", nil)
	if a[0] != 1 || b[0] != 1 {
		t.Fatalf("两条会话第一帧都该是 1，实际 a=%v b=%v", a, b)
	}
}

// serve 无人值守：默认必须是 deny，不许默认 allow。
// 这条测试同时是 scripts/perm-gates.json 里 cmdServe 的登记凭据。
func TestServePolicyIsDeny(t *testing.T) {
	r := &loopRunner{home: "/tmp/x", policy: "deny", maxSteps: 6}
	if r.policy != "deny" {
		t.Fatalf("serve 默认策略必须是 deny，拿到 %q", r.policy)
	}
}

// 打到屏幕上的策略必须**等于**实际用的那个值。
// 打一句写死的话，和实际参数漂移之后，用户看到的就是假话。
func TestAnnouncedPolicyMatchesRunner(t *testing.T) {
	for _, p := range []string{"deny", "allow"} {
		r := &loopRunner{home: "/tmp/x", policy: p, maxSteps: 6}
		var buf strings.Builder
		announcePolicy(&buf, r.policy, r.maxSteps)
		if !strings.Contains(buf.String(), p) {
			t.Fatalf("播报里没有实际策略 %q：%s", p, buf.String())
		}
	}
}

// 步数也必须播报：不然"跑到一半停了"看起来像失败，其实是用满了上限。
func TestAnnouncedMaxSteps(t *testing.T) {
	var buf strings.Builder
	announcePolicy(&buf, "deny", 9)
	if !strings.Contains(buf.String(), "9") {
		t.Fatalf("播报里要有实际步数 9：%s", buf.String())
	}
}

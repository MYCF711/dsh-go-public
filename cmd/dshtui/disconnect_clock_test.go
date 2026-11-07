package main

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// startClockTurn 走真实 HTTP accepted 结算入口建立 running 状态；
// 测试不直接伪造 busy/start，避免计时测试与真实轮次脱节。
func startClockTurn(t *testing.T, a *app, id uint64, turnID string) {
	t.Helper()
	a.pendingSend = &pendingPrompt{id: id, sid: a.sid, text: "计时"}
	a.handlePromptResult(promptOutcome{
		id: id, sid: a.sid, text: "计时",
		ack: promptAck{Accepted: true, TurnID: turnID},
	})
	if !a.busy || a.activeTurnID != turnID {
		t.Fatalf("真实轮次未建立：busy=%v turn=%q", a.busy, a.activeTurnID)
	}
}

func newClockApp(sid string) (*app, *fakeClock) {
	a := newTestApp(120, 36)
	a.sid = sid
	clk := &fakeClock{t: time.Date(2026, 11, 4, 23, 0, 0, 0, time.UTC)}
	a.now = clk.now
	return a, clk
}

func TestDisconnectClockFreezesAndResumesAcrossConnectionGenerations(t *testing.T) {
	a, clk := newClockApp("session-A")
	startClockTurn(t, a, 1, "turn-A")
	a.handleConnectionEvent(connectionEvent{kind: wsOpen, sid: a.sid, subscription: 11, generation: 1})

	clk.advance(12 * time.Second)
	if row := statusLine(a); !strings.Contains(row, "运行中 12s") {
		t.Fatalf("连接时应累计 12s：%q", row)
	}

	// WS close：墙钟继续，但运行计时必须冻结，busy/turn 不变。
	a.handleConnectionEvent(connectionEvent{kind: wsDown, sid: a.sid, subscription: 11, generation: 1, err: io.EOF})
	clk.advance(90 * time.Second)
	if row := statusLine(a); !strings.Contains(row, "运行中 12s") || !strings.Contains(row, "断线暂停") {
		t.Fatalf("close 后断线墙钟被算入或暂停不可见：%q", row)
	}
	if !a.busy || a.activeTurnID != "turn-A" {
		t.Fatalf("close 伪改了真实轮次：busy=%v turn=%q", a.busy, a.activeTurnID)
	}

	// 新连接 open 清除旧 down，从 12s 继续，不能重置为 0。
	a.handleConnectionEvent(connectionEvent{kind: wsOpen, sid: a.sid, subscription: 11, generation: 2})
	clk.advance(7 * time.Second)
	if row := statusLine(a); !strings.Contains(row, "运行中 19s") || strings.Contains(row, "断线暂停") {
		t.Fatalf("重连后没有从累计值继续：%q", row)
	}

	// 第一代连接的迟到 close 不得暂停第二代连接。
	a.handleConnectionEvent(connectionEvent{kind: wsDown, sid: a.sid, subscription: 11, generation: 1, err: io.EOF})
	clk.advance(time.Second)
	if row := statusLine(a); !strings.Contains(row, "运行中 20s") || strings.Contains(row, "断线暂停") {
		t.Fatalf("迟到 close 污染了新连接：%q", row)
	}

	// read error 与后续重连失败都保持同一个冻结点，重复 down 不重复结算。
	a.handleConnectionEvent(connectionEvent{kind: wsDown, sid: a.sid, subscription: 11, generation: 2, err: errors.New("read failed")})
	a.handleConnectionEvent(connectionEvent{kind: wsDown, sid: a.sid, subscription: 11, generation: 2, err: errors.New("redial failed")})
	clk.advance(30 * time.Second)
	if row := statusLine(a); !strings.Contains(row, "运行中 20s") {
		t.Fatalf("read error/重连失败后计时未冻结：%q", row)
	}
	a.handleConnectionEvent(connectionEvent{kind: wsOpen, sid: a.sid, subscription: 11, generation: 3})
	clk.advance(5 * time.Second)
	if row := statusLine(a); !strings.Contains(row, "运行中 25s") {
		t.Fatalf("第二次重连后累计值错误：%q", row)
	}
}

func TestConnectionEventsAreScopedToSessionAndSubscription(t *testing.T) {
	a, clk := newClockApp("session-A")
	startClockTurn(t, a, 1, "turn-A")
	a.handleConnectionEvent(connectionEvent{kind: wsOpen, sid: a.sid, subscription: 11, generation: 1})
	clk.advance(8 * time.Second)
	a.handleConnectionEvent(connectionEvent{kind: wsDown, sid: a.sid, subscription: 11, generation: 1})

	// 模拟成功的 A→B 切换；新会话有自己的真实轮次与订阅代际。
	a.resetSessionSendState()
	a.sid = "session-B"
	startClockTurn(t, a, 2, "turn-B")
	a.handleConnectionEvent(connectionEvent{kind: wsOpen, sid: a.sid, subscription: 22, generation: 1})
	clk.advance(4 * time.Second)

	for _, ev := range []connectionEvent{
		{kind: wsDown, sid: "session-A", subscription: 11, generation: 99},
		{kind: wsDown, sid: "session-B", subscription: 11, generation: 99},
		{kind: wsOpen, sid: "session-A", subscription: 11, generation: 99},
	} {
		a.handleConnectionEvent(ev)
	}
	clk.advance(time.Second)
	if row := statusLine(a); !strings.Contains(row, "运行中 5s") || strings.Contains(row, "断线暂停") {
		t.Fatalf("A/B 切换后旧事件串入新会话：%q", row)
	}

	// 旧订阅即使 sid 相同也不能清掉当前订阅的 down。
	a.handleConnectionEvent(connectionEvent{kind: wsDown, sid: "session-B", subscription: 22, generation: 1})
	a.handleConnectionEvent(connectionEvent{kind: wsOpen, sid: "session-B", subscription: 11, generation: 99})
	if row := statusLine(a); !strings.Contains(row, "断线暂停") || !strings.Contains(row, "运行中 5s") {
		t.Fatalf("同 sid 旧订阅清除了当前 down：%q", row)
	}
	a.handleConnectionEvent(connectionEvent{kind: wsOpen, sid: "session-B", subscription: 22, generation: 2})
	clk.advance(3 * time.Second)
	if row := statusLine(a); !strings.Contains(row, "运行中 8s") {
		t.Fatalf("当前订阅恢复后累计错误：%q", row)
	}
}

func TestConnectionEventsDoNotMutateApprovalOrTurn(t *testing.T) {
	a, clk := newClockApp("session-A")
	startClockTurn(t, a, 1, "turn-real")
	a.pending.id = "approval-real"
	a.pending.title = "真实审批"
	a.handleConnectionEvent(connectionEvent{kind: wsOpen, sid: a.sid, subscription: 1, generation: 1})
	a.handleConnectionEvent(connectionEvent{kind: wsDown, sid: a.sid, subscription: 1, generation: 1})
	clk.advance(time.Minute)
	a.handleConnectionEvent(connectionEvent{kind: wsOpen, sid: a.sid, subscription: 1, generation: 2})
	if !a.busy || a.activeTurnID != "turn-real" || a.pending.id != "approval-real" || a.pending.title != "真实审批" {
		t.Fatalf("连接事件伪改 busy/审批/轮次：busy=%v turn=%q pending=%+v", a.busy, a.activeTurnID, a.pending)
	}
}

func TestDisconnectClockSnapshotsStayWithinTerminal(t *testing.T) {
	for _, size := range [][2]int{{120, 36}, {80, 24}} {
		a, clk := newClockApp("session-snapshot")
		a.scr.width, a.scr.height = size[0], size[1]
		a.model = strings.Repeat("很长的模型名", 20)
		a.effort = strings.Repeat("effort", 20)
		startClockTurn(t, a, 1, "turn-snapshot")
		a.handleConnectionEvent(connectionEvent{kind: wsOpen, sid: a.sid, subscription: 1, generation: 1})
		clk.advance(65 * time.Second)
		a.handleConnectionEvent(connectionEvent{kind: wsDown, sid: a.sid, subscription: 1, generation: 1})
		for i, row := range a.view() {
			if got := dispWidth(row); got > size[0] {
				t.Fatalf("%dx%d down 第 %d 行宽 %d：%q", size[0], size[1], i, got, row)
			}
		}
		if got := len(a.view()); got != size[1] {
			t.Fatalf("%dx%d down 行数=%d", size[0], size[1], got)
		}
		a.handleConnectionEvent(connectionEvent{kind: wsOpen, sid: a.sid, subscription: 1, generation: 2})
		for i, row := range a.view() {
			if got := dispWidth(row); got > size[0] {
				t.Fatalf("%dx%d open 第 %d 行宽 %d：%q", size[0], size[1], i, got, row)
			}
		}
	}
}

type fakeWSRead struct {
	text string
	err  error
}

type fakeSubscriptionConn struct {
	reads     chan fakeWSRead
	closed    chan struct{}
	closeOnce sync.Once
}

func newFakeSubscriptionConn() *fakeSubscriptionConn {
	return &fakeSubscriptionConn{reads: make(chan fakeWSRead, 4), closed: make(chan struct{})}
}

func (c *fakeSubscriptionConn) ReadText() (string, error) {
	select {
	case r := <-c.reads:
		return r.text, r.err
	case <-c.closed:
		return "", io.EOF
	}
}

func (c *fakeSubscriptionConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

type fakeDialResult struct {
	conn *fakeSubscriptionConn
	err  error
}

func nextConnectionEvent(t *testing.T, events <-chan connectionEvent) connectionEvent {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("连接事件流提前关闭")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("等待连接事件超时")
		return connectionEvent{}
	}
}

func TestSubscribeReportsOpenCloseReadErrorAndReconnectFailure(t *testing.T) {
	first := newFakeSubscriptionConn()
	second := newFakeSubscriptionConn()
	third := newFakeSubscriptionConn()
	dials := make(chan fakeDialResult, 4)
	dials <- fakeDialResult{conn: first}
	dials <- fakeDialResult{err: errors.New("reconnect failed")}
	dials <- fakeDialResult{conn: second}
	dials <- fakeDialResult{conn: third}
	c := &client{
		dialSubscription: func(string, int) (subscriptionConn, error) {
			r := <-dials
			if r.err != nil {
				return nil, r.err
			}
			return r.conn, nil
		},
		reconnectBackoff: func(int) time.Duration { return 0 },
	}
	_, events, stop, err := c.SubscribeWithEvents("session-A", 0)
	if err != nil {
		t.Fatalf("首次订阅失败：%v", err)
	}
	defer stop()

	ev := nextConnectionEvent(t, events)
	if ev.kind != wsOpen || ev.generation != 1 || ev.sid != "session-A" || ev.subscription == 0 {
		t.Fatalf("首次 open 事件错误：%+v", ev)
	}
	first.reads <- fakeWSRead{err: io.EOF}
	if ev = nextConnectionEvent(t, events); ev.kind != wsDown || ev.generation != 1 {
		t.Fatalf("close 未报告 down：%+v", ev)
	}
	if ev = nextConnectionEvent(t, events); ev.kind != wsDown || ev.generation != 1 || ev.err == nil {
		t.Fatalf("重连失败未报告 down：%+v", ev)
	}
	if ev = nextConnectionEvent(t, events); ev.kind != wsOpen || ev.generation != 2 {
		t.Fatalf("重连成功未报告第二代 open：%+v", ev)
	}
	second.reads <- fakeWSRead{err: errors.New("read failed")}
	if ev = nextConnectionEvent(t, events); ev.kind != wsDown || ev.generation != 2 {
		t.Fatalf("read error 未报告 down：%+v", ev)
	}
	if ev = nextConnectionEvent(t, events); ev.kind != wsOpen || ev.generation != 3 {
		t.Fatalf("第二次重连未报告第三代 open：%+v", ev)
	}
}

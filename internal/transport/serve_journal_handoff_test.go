package transport

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"
)

// replayHookConn 在第一条 journal 重放帧写入时制造一次真实的 Append+Publish。
// 旧实现此时尚未 Subscribe，新帧会永久落空；新实现必须把它接住且只交付一次。
type replayHookConn struct {
	*pipeConn
	once     sync.Once
	onReplay func()
}

func (c *replayHookConn) WriteServerFrame(f ServerFrame) error {
	if f.Seq == 1 && c.onReplay != nil {
		c.once.Do(c.onReplay)
	}
	return c.pipeConn.WriteServerFrame(f)
}

func TestServeConnReplaySubscribeHandoffLosesNothing(t *testing.T) {
	js := NewJournals(nil)
	j := js.Get("s-handoff")
	first := j.Append("tool/call", json.RawMessage(`{"callId":"call-1"}`), 101)

	h := NewHub()
	h.SetJournals(js)
	p := newPipe()
	conn := &replayHookConn{pipeConn: p}
	second := ServerFrame{}
	conn.onReplay = func() {
		second = j.Append("tool/result", json.RawMessage(`{"message":{"toolCallId":"call-1"}}`), 102)
		// 重复 Publish 模拟重放与实时两条来源同时撞上；订阅者仍只能看到一次。
		h.Publish("session:s-handoff", second)
		h.Publish("session:s-handoff", second)
	}
	done := make(chan error, 1)
	go func() { done <- ServeConn(conn, h) }()
	t.Cleanup(func() {
		p.closeIn()
		<-done
	})

	p.in <- ClientFrame{T: FrameSub, Topic: "session:s-handoff", SinceSeq: 0, JournalEpoch: j.Epoch()}
	got := []ServerFrame{p.next(t), p.next(t)}
	if got[0].Seq != first.Seq || got[1].Seq != second.Seq || got[1].Time != 102 {
		t.Fatalf("replay→sub 交接必须按序且不丢帧：got=%+v first=%+v second=%+v", got, first, second)
	}
	select {
	case extra := <-p.out:
		t.Fatalf("同一实时帧重复 Publish 不得重复交付：%+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestServeConnLiveOutOfOrderPublishFillsFromJournal(t *testing.T) {
	js := NewJournals(nil)
	j := js.Get("s-live-order")
	j.Append("turn/start", json.RawMessage(`{"turn":1}`), 100)
	h := NewHub()
	h.SetJournals(js)
	p := newPipe()
	done := make(chan error, 1)
	go func() { done <- ServeConn(p, h) }()
	t.Cleanup(func() {
		p.closeIn()
		<-done
	})
	p.in <- ClientFrame{T: FrameSub, Topic: "session:s-live-order", SinceSeq: 0, JournalEpoch: j.Epoch()}
	if got := p.next(t); got.Seq != 1 {
		t.Fatalf("首帧 = %+v", got)
	}
	// 等 replay 闸完全关闭，专门撞实时 Publish 的乱序，而不是交接缓冲排序。
	time.Sleep(50 * time.Millisecond)
	second := j.Append("tool/call", json.RawMessage(`{"callId":"call-order"}`), 101)
	third := j.Append("tool/result", json.RawMessage(`{"message":{"toolCallId":"call-order"}}`), 102)
	h.Publish("session:s-live-order", third)
	if got := p.next(t); got.Seq != second.Seq {
		t.Fatalf("先发 seq=3 时也必须从 journal 补 seq=2，实际 %+v", got)
	}
	if got := p.next(t); got.Seq != third.Seq {
		t.Fatalf("随后应是 seq=3，实际 %+v", got)
	}
	// 迟到的 seq=2 Publish 只是另一个补洞触发，不得再交付一份。
	h.Publish("session:s-live-order", second)
	select {
	case extra := <-p.out:
		t.Fatalf("迟到 Publish 不得造成重复：%+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestServeConnZeroWatermarkReplayHasSingleCopy(t *testing.T) {
	js := NewJournals(nil)
	j := js.Get("s-zero")
	for i := 1; i <= 3; i++ {
		j.Append("assistant/message", json.RawMessage(`{"text":"x"}`), int64(100+i))
	}
	h := NewHub()
	h.SetJournals(js)
	sub := &recordingSub{}
	conn := &scriptedConn{sub: sub, frames: []ClientFrame{
		{T: FrameSub, Topic: "session:s-zero", SinceSeq: 0, JournalEpoch: j.Epoch()},
	}}
	if err := ServeConn(conn, h); err != nil {
		t.Fatal(err)
	}
	var seqs []int
	for _, f := range sub.frames {
		seqs = append(seqs, f.Seq)
		if f.Time <= 0 {
			t.Fatalf("重放必须保留原始 time：%+v", f)
		}
	}
	if !reflect.DeepEqual(seqs, []int{1, 2, 3}) {
		t.Fatalf("零水位应恰好重放 1..3 各一次，实际 %v", seqs)
	}
}

func TestServeConnReplayDeduplicatesPendingAlreadyInJournal(t *testing.T) {
	js := NewJournals(nil)
	j := js.Get("s-pending")
	payload := json.RawMessage(`{"requestId":"perm-1","sessionId":"s-pending"}`)
	j.Append("approval/request", payload, 1234)
	h := NewHub()
	h.SetJournals(js)
	h.SetPendingReplay(func(string) []ServerFrame {
		return []ServerFrame{{Type: "approval/request", Data: payload}}
	})
	sub := &recordingSub{}
	conn := &scriptedConn{sub: sub, frames: []ClientFrame{
		{T: FrameSub, Topic: "session:s-pending", SinceSeq: 0, JournalEpoch: j.Epoch()},
	}}
	if err := ServeConn(conn, h); err != nil {
		t.Fatal(err)
	}
	if len(sub.frames) != 1 || sub.frames[0].Seq != 1 || string(sub.frames[0].Data) != string(payload) {
		t.Fatalf("同一 requestId 不得同时以 pending 与 journal 各出现一次：%+v", sub.frames)
	}
}

func TestServeConnPendingReplayWithoutJournalCopyHasTime(t *testing.T) {
	js := NewJournals(nil)
	j := js.Get("s-pending-time")
	payload := json.RawMessage(`{"requestId":"perm-time","sessionId":"s-pending-time"}`)
	h := NewHub()
	h.SetJournals(js)
	h.SetPendingReplay(func(string) []ServerFrame {
		return []ServerFrame{{Type: "approval/request", Data: payload}}
	})
	sub := &recordingSub{}
	conn := &scriptedConn{sub: sub, frames: []ClientFrame{
		{T: FrameSub, Topic: "session:s-pending-time", SinceSeq: 0, JournalEpoch: j.Epoch()},
	}}
	if err := ServeConn(conn, h); err != nil {
		t.Fatal(err)
	}
	if len(sub.frames) != 1 || sub.frames[0].Seq != 0 || sub.frames[0].Time <= 0 || string(sub.frames[0].Data) != string(payload) {
		t.Fatalf("pending 状态重放要保持 data/seq=0 且 time>0：%+v", sub.frames)
	}
}

func TestServeConnResetHasPositiveEnvelopeTime(t *testing.T) {
	js := NewJournals(nil)
	j := js.Get("s-reset-time")
	j.Append("turn/done", json.RawMessage(`{"turnId":"turn-1"}`), 10)
	h := NewHub()
	h.SetJournals(js)
	sub := &recordingSub{}
	conn := &scriptedConn{sub: sub, frames: []ClientFrame{{
		T: FrameSub, Topic: "session:s-reset-time", SinceSeq: 1, JournalEpoch: j.Epoch() + 1,
	}}}
	if err := ServeConn(conn, h); err != nil {
		t.Fatal(err)
	}
	if len(sub.frames) != 1 || sub.frames[0].Type != "session/reset" || sub.frames[0].Seq != 0 || sub.frames[0].Time <= 0 {
		t.Fatalf("reset 必须是无 seq 且 time>0 的控制帧：%+v", sub.frames)
	}
}

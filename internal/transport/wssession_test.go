package transport

import (
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

// pipeConn 用两个 channel 模拟一条连接。
type pipeConn struct {
	in       chan ClientFrame
	out      chan ServerFrame
	outClose chan struct{}
	once     sync.Once
}

func newPipe() *pipeConn {
	return &pipeConn{in: make(chan ClientFrame, 8), out: make(chan ServerFrame, 8), outClose: make(chan struct{})}
}

func (p *pipeConn) ReadClientFrame() (ClientFrame, error) {
	f, ok := <-p.in
	if !ok {
		return ClientFrame{}, io.EOF
	}
	return f, nil
}

func (p *pipeConn) WriteServerFrame(f ServerFrame) error {
	select {
	case p.out <- f:
		return nil
	case <-p.outClose:
		return io.ErrClosedPipe
	}
}

func (p *pipeConn) closeIn() { close(p.in) }

func (p *pipeConn) next(t *testing.T) ServerFrame {
	t.Helper()
	select {
	case f := <-p.out:
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("等不到服务端帧")
		return ServerFrame{}
	}
}

// sub 之后，该 topic 的事件要真的推过来。
func TestServeConnDeliversAfterSub(t *testing.T) {
	hub := NewHub()
	p := newPipe()
	done := make(chan error, 1)
	go func() { done <- ServeConn(p, hub) }()

	p.in <- ClientFrame{T: "sub", Topic: "session:a", SinceSeq: 0}
	// 等订阅生效：反复发，直到收到一帧
	deadline := time.Now().Add(2 * time.Second)
	sent := 1
	for {
		hub.Publish("session:a", ServerFrame{Seq: sent, Type: "x"})
		select {
		case f := <-p.out:
			if f.Seq != sent {
				t.Fatalf("收到 seq=%d，期望 %d", f.Seq, sent)
			}
			p.closeIn()
			if err := <-done; err != nil {
				t.Fatalf("正常关闭应当返回 nil，拿到 %v", err)
			}
			return
		case <-time.After(20 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("订阅之后一直收不到帧")
			}
			sent++
		}
	}
}

// 断连之后 Hub 里不许还留着这个订阅者——否则 Publish 会一直往死连接里写。
func TestServeConnUnsubscribesOnClose(t *testing.T) {
	hub := NewHub()
	p := newPipe()
	done := make(chan error, 1)
	go func() { done <- ServeConn(p, hub) }()

	p.in <- ClientFrame{T: "sub", Topic: "session:a"}
	time.Sleep(50 * time.Millisecond)
	p.closeIn()
	<-done

	// 摘干净了的话，这里不该 panic 也不会写进 p.out
	hub.Publish("session:a", ServerFrame{Seq: 1, Type: "x"})
	select {
	case f := <-p.out:
		t.Fatalf("连接都关了还收到 %+v", f)
	case <-time.After(50 * time.Millisecond):
	}
}

// 水位之前的历史已经滑出内存 journal 时，订阅必须明确要求重建基线。
//
// 容量丢最老帧之后，客户端带着 sinceSeq=0 来订阅，服务端只能从最老的保留帧开始推。
// 那一段**看不出缺口**：帧是连续的、seq 也在涨，客户端以为它拿到了完整历史——
// 这正是"有产出（帧）、但没有有效载荷（缺了中间那段）、而且没人报错"。
func TestServeConnSignalsGapAfterTrim(t *testing.T) {
	// 容量只有 3：写 5 帧，最早两帧已经被丢掉。
	js := &Journals{m: map[string]*Journal{"a": NewJournal(1, 3)}}
	j := js.Get("a")
	for i := 1; i <= 5; i++ {
		j.Append("assistant/text", json.RawMessage(`{"text":"x"}`), int64(i))
	}
	if frames, _, _ := j.Since(0, 0); len(frames) != 3 || frames[0].Seq != 3 {
		t.Fatalf("测试前提不成立：应只留 seq 3..5，实际 %d 帧、首帧 %d", len(frames), frames[0].Seq)
	}

	hub := NewHub()
	hub.SetJournals(js)
	p := newPipe()
	go ServeConn(p, hub)

	p.in <- ClientFrame{T: "sub", Topic: "session:a", SinceSeq: 0, JournalEpoch: j.Epoch()}
	f := p.next(t)
	if f.Type != "session/reset" {
		t.Fatalf("水位滑出内存时必须先发 session/reset，实际第一帧 %+v", f)
	}

	// reset 之后只接新帧：不该把保留的 4096 帧一股脑灌给客户端。
	select {
	case extra := <-p.out:
		t.Fatalf("reset 之后不该再重放旧帧，收到 %+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
	p.closeIn()
}

// 坏帧不许当 ping 放过：第一帧坏了，连接立刻结束。
func TestServeConnClosesOnBadFrame(t *testing.T) {
	hub := NewHub()
	p := newPipe()
	done := make(chan error, 1)
	go func() { done <- ServeConn(p, hub) }()
	p.in <- ClientFrame{T: "subscribe"} // 未知类型
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("坏帧应当返回错误")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("坏帧之后连接没结束")
	}
}

package transport

import (
	"errors"
	"sync"
	"testing"
)

type fakeSub struct {
	mu     sync.Mutex
	got    []int // 收到的 seq
	failOn int   // seq 等于它时返回 error
}

func (f *fakeSub) Push(fr ServerFrame) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOn != 0 && fr.Seq == f.failOn {
		return errors.New("连接坏了")
	}
	f.got = append(f.got, fr.Seq)
	return nil
}

func (f *fakeSub) seqs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.got...)
}

// sinceSeq 是重连的语义：只发 > sinceSeq 的帧。
func TestHubRespectsSinceSeq(t *testing.T) {
	h := NewHub()
	s := &fakeSub{}
	h.Subscribe("session:a", 5, s)
	h.Publish("session:a", ServerFrame{Seq: 4, Type: "x"})
	h.Publish("session:a", ServerFrame{Seq: 5, Type: "x"})
	h.Publish("session:a", ServerFrame{Seq: 6, Type: "x"})

	if got := s.seqs(); len(got) != 1 || got[0] != 6 {
		t.Fatalf("只该收到 seq 6，收到 %v", got)
	}
}

// 重连时重复投递比丢帧更难查——所以晚到的旧帧必须丢掉。
func TestHubDropsLateOldFrames(t *testing.T) {
	h := NewHub()
	s := &fakeSub{}
	h.Subscribe("session:a", 10, s)
	h.Publish("session:a", ServerFrame{Seq: 3, Type: "backfill"})
	if got := s.seqs(); len(got) != 0 {
		t.Fatalf("旧帧必须丢，收到 %v", got)
	}
}

// 只发给订了这个 topic 的人。
func TestHubIsolatesTopics(t *testing.T) {
	h := NewHub()
	a, b := &fakeSub{}, &fakeSub{}
	h.Subscribe("session:a", 0, a)
	h.Subscribe("session:b", 0, b)
	h.Publish("session:a", ServerFrame{Seq: 1, Type: "x"})
	if len(a.seqs()) != 1 || len(b.seqs()) != 0 {
		t.Fatalf("a=%v b=%v", a.seqs(), b.seqs())
	}
}

// Push 报错 = 连接坏了，要从所有 topic 上摘掉，而不是继续往里塞。
func TestHubDropsBrokenSubscriber(t *testing.T) {
	h := NewHub()
	bad, good := &fakeSub{failOn: 1}, &fakeSub{}
	h.Subscribe("session:a", 0, bad)
	h.Subscribe("session:b", 0, bad)
	h.Subscribe("session:a", 0, good)

	h.Publish("session:a", ServerFrame{Seq: 1, Type: "x"})
	h.Publish("session:b", ServerFrame{Seq: 2, Type: "x"})
	h.Publish("session:a", ServerFrame{Seq: 3, Type: "x"})

	if good.seqs() == nil {
		t.Fatal("好的订阅者不该被连累")
	}
	if got := bad.seqs(); len(got) != 0 {
		t.Fatalf("坏订阅者不该收到任何帧，收到 %v", got)
	}
}

func TestHubUnsubscribe(t *testing.T) {
	h := NewHub()
	s := &fakeSub{}
	h.Subscribe("session:a", 0, s)
	h.Unsubscribe("session:a", s)
	h.Publish("session:a", ServerFrame{Seq: 1, Type: "x"})
	h.Unsubscribe("session:a", s) // 再摘一次不许 panic
	if len(s.seqs()) != 0 {
		t.Fatal("摘了就不该再收到")
	}
}

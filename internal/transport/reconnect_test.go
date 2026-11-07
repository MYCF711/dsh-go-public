package transport

import (
	"testing"
	"time"
)

// 重连契约的宿主侧那一半：带 sinceSeq 订阅 → 只收到更晚的帧。
//
// 这条测的是**界面重连时的真实用法**（不是 hub 单测）：
// sub{sinceSeq:5} 之后，Publish 1..8，只该收到 6/7/8。
func TestReconnectWithSinceSeqGetsOnlyNewer(t *testing.T) {
	hub := NewHub()
	p := newPipe()
	done := make(chan error, 1)
	go func() { done <- ServeConn(p, hub) }()

	p.in <- ClientFrame{T: "sub", Topic: "session:a", SinceSeq: 5}
	time.Sleep(100 * time.Millisecond) // 等订阅生效

	for i := 1; i <= 8; i++ {
		hub.Publish("session:a", ServerFrame{Seq: i, Type: "x"})
	}

	var got []int
	deadline := time.After(2 * time.Second)
	for len(got) < 3 {
		select {
		case f := <-p.out:
			got = append(got, f.Seq)
		case <-deadline:
			t.Fatalf("只收到 %v，应当收到 6/7/8", got)
		}
	}
	if len(got) != 3 || got[0] != 6 || got[1] != 7 || got[2] != 8 {
		t.Fatalf("重连后收到的应当是 6/7/8，实际 %v", got)
	}
}

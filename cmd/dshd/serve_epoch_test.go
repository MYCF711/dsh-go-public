package main

import "testing"

// 两次"启动"生成的 epoch 不能有交集。
//
// 真机事故：原先 epoch 是进程内自增（1,2,3…），宿主重启后同一会话又拿到 1；
// 客户端带旧水位 {sinceSeq:1, journalEpoch:1} 重订时世代"匹配"，
// 服务端不发 session/reset，hub 又把 seq=1 当旧帧丢掉——8 秒 0 帧，
// 而 history 里明明有那条事件。客户端无报错、无 gap，界面表现是"发出去没反应"。
func TestEpochsFromDifferentRunsDoNotCollide(t *testing.T) {
	a := newEpochFunc()
	b := newEpochFunc()

	seen := make(map[int64]bool)
	for i := 0; i < 1000; i++ {
		v := a()
		if seen[v] {
			t.Fatalf("同一次运行里 epoch 重复：%d", v)
		}
		seen[v] = true
	}
	for i := 0; i < 1000; i++ {
		if seen[b()] {
			t.Fatal("第二次运行给出了第一次运行已经用过的 epoch——重启后就会撞号")
		}
	}
}

// 同一个发生器必须单调递增：客户端靠它比较新旧。
func TestEpochIsMonotonic(t *testing.T) {
	f := newEpochFunc()
	prev := f()
	for i := 0; i < 100; i++ {
		cur := f()
		if cur <= prev {
			t.Fatalf("epoch 没递增：%d → %d", prev, cur)
		}
		prev = cur
	}
}

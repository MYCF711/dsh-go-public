package transport

import (
	"fmt"
	"sync"
	"testing"
)

// 并发压力：没有 -race 也要有人去撞它。
//
// 本机没有 C 工具链（`gcc`/`clang`/`cl` 都没有，CGO_ENABLED=0），所以
// `go test -race` 从来跑不起来——这在 AGENTS §5 里明确就是"未验证"。
// 这个测试是替代品，**但只替代得了一部分**：它抓得到死锁、丢帧、seq 错乱、
// 并发修改 map 导致的 panic；抓不到"没有可见症状的数据竞争"。
//
// 判据：每个会话的 seq 必须正好是 1..N 且无重复、无缺口；
// 订阅者在取消订阅后不许再收到帧。
func TestConcurrentJournalAndHubInvariants(t *testing.T) {
	const sessions, perSession, readers = 4, 200, 4

	js := NewJournals(nil)
	h := NewHub()

	var wg sync.WaitGroup

	// 写：每个会话一个写入者，各写 perSession 帧。
	for s := 0; s < sessions; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			j := js.Get(fmt.Sprintf("s%d", s))
			for i := 0; i < perSession; i++ {
				j.Append("text", nil, int64(i))
				h.Publish(fmt.Sprintf("session:s%d", s), ServerFrame{Type: "text"})
			}
		}(s)
	}

	// 读：并发地取 since，不许 panic、不许拿到非法 seq。
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perSession; i++ {
				j := js.Get("s0")
				frames, _, _ := j.Since(0, 0)
				prev := 0
				for _, f := range frames {
					if f.Seq <= prev {
						t.Errorf("seq 没有严格递增：%d 之后是 %d", prev, f.Seq)
						return
					}
					prev = f.Seq
				}
			}
		}()
	}

	// 订阅/退订：退订之后不许再收到帧。
	wg.Add(1)
	go func() {
		defer wg.Done()
		sub := &countingSub{}
		h.Subscribe("session:s1", 0, sub)
		h.Unsubscribe("session:s1", sub)
		before := sub.count
		h.Publish("session:s1", ServerFrame{Type: "text"})
		if sub.count != before {
			t.Errorf("退订后还收到了帧")
		}
	}()

	wg.Wait()

	// 每个会话的 seq 必须是 1..perSession。
	for s := 0; s < sessions; s++ {
		frames, last, _ := js.Get(fmt.Sprintf("s%d", s)).Since(0, 0)
		if len(frames) != perSession || last != perSession {
			t.Fatalf("s%d：应 %d 帧、水位 %d，实际 %d 帧 / 水位 %d",
				s, perSession, perSession, len(frames), last)
		}
		for i, f := range frames {
			if f.Seq != i+1 {
				t.Fatalf("s%d 第 %d 帧 seq=%d（应为 %d）", s, i, f.Seq, i+1)
			}
		}
	}
}

type countingSub struct {
	mu    sync.Mutex
	count int
}

func (c *countingSub) Push(ServerFrame) error {
	c.mu.Lock()
	c.count++
	c.mu.Unlock()
	return nil
}

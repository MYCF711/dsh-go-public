package transport

import (
	"fmt"
	"testing"
)

// msgs 造 n 条 Seq 从 1 到 n 的消息，Text 里带上 seq，方便断言"拿到的到底是哪几条"。
func msgs(n int) []Message {
	out := make([]Message, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, Message{Seq: i, Role: "user", Text: fmt.Sprintf("m%d", i)})
	}
	return out
}

// seqs 把一页的 seq 抽出来，断言时比这个列表，比比 Text 更能锁住"顺序与范围"。
func seqs(p Page) []int {
	out := make([]int, 0, len(p.Messages))
	for _, m := range p.Messages {
		out = append(out, m.Seq)
	}
	return out
}

func eqInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 1. PageTail 取最后 N 条，且因为还有更早的，Truncated=true。
func TestSelectPageTailReturnsNewestAndTruncated(t *testing.T) {
	all := msgs(10)

	p := SelectPage(all, PageTail, 0, 3)

	if want := []int{8, 9, 10}; !eqInts(seqs(p), want) {
		t.Fatalf("PageTail 应取最后 3 条 %v，实际拿到 %v", want, seqs(p))
	}
	if !p.Truncated {
		t.Fatal("会话有 10 条、只要 3 条，还有 7 条更早的没给，Truncated 必须是 true")
	}
	if p.Total != 10 {
		t.Fatalf("Total 应是会话总条数 10，实际 %d", p.Total)
	}
	if p.OldestSeq != 8 || p.NewestSeq != 10 {
		t.Fatalf("OldestSeq/NewestSeq 应是 8/10，实际 %d/%d", p.OldestSeq, p.NewestSeq)
	}
	if p.Messages[0].Text != "m8" || p.Messages[2].Text != "m10" {
		t.Fatalf("正文没跟着 seq 一起切：%q .. %q", p.Messages[0].Text, p.Messages[2].Text)
	}
}

// 2. PageTail 会话比 limit 短 → 全给，Truncated=false（"更早的"一条都不缺）。
func TestSelectPageTailShorterThanLimitGivesAll(t *testing.T) {
	all := msgs(3)

	p := SelectPage(all, PageTail, 0, 10)

	if want := []int{1, 2, 3}; !eqInts(seqs(p), want) {
		t.Fatalf("会话只有 3 条、limit=10，应该全给 %v，实际 %v", want, seqs(p))
	}
	if p.Truncated {
		t.Fatal("3 条全给了，没有任何更早的被截掉，Truncated 必须是 false")
	}
	if p.Total != 3 {
		t.Fatalf("Total 应是 3，实际 %d", p.Total)
	}
	if p.OldestSeq != 1 || p.NewestSeq != 3 {
		t.Fatalf("OldestSeq/NewestSeq 应是 1/3，实际 %d/%d", p.OldestSeq, p.NewestSeq)
	}
}

// 3. PageSince 只回 Seq > sinceSeq 的；Truncated 恒为 false，包括 limit 截了尾巴的时候。
func TestSelectPageSinceReturnsStrictlyAfterSeq(t *testing.T) {
	all := msgs(10)

	p := SelectPage(all, PageSince, 7, 0)
	if want := []int{8, 9, 10}; !eqInts(seqs(p), want) {
		t.Fatalf("sinceSeq=7 应回到 %v，实际 %v", want, seqs(p))
	}
	if p.Truncated {
		t.Fatal("补洞永远没有\"更早的没给\"这回事，Truncated 必须是 false")
	}
	if p.Total != 10 {
		t.Fatalf("Total 是会话总条数 10（不是本页条数），实际 %d", p.Total)
	}
	if p.OldestSeq != 8 || p.NewestSeq != 10 {
		t.Fatalf("旧新 seq 应是 8/10，实际 %d/%d", p.OldestSeq, p.NewestSeq)
	}

	// sinceSeq 恰好等于某条消息的 seq 时，那条本身不能被包含进来（是严格大于）。
	p = SelectPage(all, PageSince, 2, 0)
	if want := []int{3, 4, 5, 6, 7, 8, 9, 10}; !eqInts(seqs(p), want) {
		t.Fatalf("sinceSeq=2 必须是严格大于，应回 %v，实际 %v", want, seqs(p))
	}
	if got := p.Messages[0].Seq; got == 2 {
		t.Fatal("Seq=2 那条被包含进来了：边界必须是严格大于")
	}

	// limit 把补洞结果截短时，Truncated 仍然必须是 false。
	p = SelectPage(all, PageSince, 0, 4)
	if want := []int{1, 2, 3, 4}; !eqInts(seqs(p), want) {
		t.Fatalf("limit=4 应只回 %v，实际 %v", want, seqs(p))
	}
	if p.Truncated {
		t.Fatal("PageSince 被 limit 截短也不许报 Truncated：那是首屏的语义，混进来界面就会误判")
	}
}

// 4. PageSince 的 sinceSeq 超过最大 seq → 空页，两个 seq 都是 0。
func TestSelectPageSinceBeyondNewestIsEmpty(t *testing.T) {
	all := msgs(5)

	p := SelectPage(all, PageSince, 99, 0)

	if len(p.Messages) != 0 {
		t.Fatalf("sinceSeq=99 比最大 seq=5 还大，应该没有新消息，实际拿到 %v", seqs(p))
	}
	if p.Messages == nil {
		t.Fatal("空页的 Messages 应是空切片而不是 nil：JSON 会变成 null，客户端要多写一条分支")
	}
	if p.OldestSeq != 0 || p.NewestSeq != 0 {
		t.Fatalf("空页的 OldestSeq/NewestSeq 必须是 0/0，实际 %d/%d", p.OldestSeq, p.NewestSeq)
	}
	if p.Total != 5 {
		t.Fatalf("没新消息不代表没历史，Total 仍是 5，实际 %d", p.Total)
	}
	if p.Truncated {
		t.Fatal("空页的 Truncated 必须是 false")
	}
}

// 5. 空会话 → 空页、Total=0、不 panic（两种模式都验）。
func TestSelectPageEmptySession(t *testing.T) {
	for _, mode := range []PageMode{PageTail, PageSince} {
		p := SelectPage(nil, mode, 0, 10)

		if len(p.Messages) != 0 {
			t.Fatalf("mode=%d 空会话不该有消息，实际 %v", mode, seqs(p))
		}
		if p.Total != 0 {
			t.Fatalf("mode=%d 空会话 Total 应为 0，实际 %d", mode, p.Total)
		}
		if p.Truncated {
			t.Fatalf("mode=%d 空会话 Truncated 应为 false", mode)
		}
		if p.OldestSeq != 0 || p.NewestSeq != 0 {
			t.Fatalf("mode=%d 空页 seq 应为 0/0，实际 %d/%d", mode, p.OldestSeq, p.NewestSeq)
		}
	}
}

// 6. limit <= 0 表示不限：PageTail 全给，且 Truncated=false。
func TestSelectPageNonPositiveLimitMeansUnlimited(t *testing.T) {
	all := msgs(6)

	for _, limit := range []int{0, -1, -100} {
		p := SelectPage(all, PageTail, 0, limit)

		if want := []int{1, 2, 3, 4, 5, 6}; !eqInts(seqs(p), want) {
			t.Fatalf("limit=%d 表示不限，PageTail 应全给 %v，实际 %v", limit, want, seqs(p))
		}
		if p.Truncated {
			t.Fatalf("limit=%d 全给了，Truncated 必须是 false", limit)
		}
		if p.OldestSeq != 1 || p.NewestSeq != 6 {
			t.Fatalf("limit=%d OldestSeq/NewestSeq 应是 1/6，实际 %d/%d", limit, p.OldestSeq, p.NewestSeq)
		}

		p = SelectPage(all, PageSince, 4, limit)
		if want := []int{5, 6}; !eqInts(seqs(p), want) {
			t.Fatalf("limit=%d 表示不限，PageSince sinceSeq=4 应全给 %v，实际 %v", limit, want, seqs(p))
		}
	}
}

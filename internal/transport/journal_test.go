package transport

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

// H14：一条会话一条 journal，seq 单调、不相干会话互不影响。
//
// 这条测试要能在「全局计数器」的旧实现上红：旧代码里两个会话共用一个
// serveTurnSeq，A 发三帧、B 发一帧，B 的第一帧会是 4——客户端按 seq 判断
// 断号时就会永远认为 B 缺了 1..3。
func TestJournalSeqIsPerSession(t *testing.T) {
	js := NewJournals(nil)
	a, b := js.Get("session-a"), js.Get("session-b")

	for i := 1; i <= 3; i++ {
		if f := a.Append("text", json.RawMessage(`{"t":"a"}`), int64(i)); f.Seq != i {
			t.Fatalf("A 第 %d 帧的 seq 应是 %d，实际 %d", i, i, f.Seq)
		}
	}
	if f := b.Append("text", json.RawMessage(`{"t":"b"}`), 10); f.Seq != 1 {
		t.Fatalf("B 的第一帧 seq 应是 1（各自独立），实际 %d", f.Seq)
	}
	if a.LastSeq() != 3 || b.LastSeq() != 1 {
		t.Fatalf("水位不对：A=%d B=%d", a.LastSeq(), b.LastSeq())
	}
}

// H14：Since 只给 seq > sinceSeq 的帧，且不抽字段（原样回 ServerFrame）。
func TestJournalSinceReturnsRawFrames(t *testing.T) {
	j := NewJournal(7, 0)
	j.Append("assistant/message", json.RawMessage(`{"message":{"content":[{"type":"reasoning","text":"想了"}]}}`), 100)
	j.Append("tool/call", json.RawMessage(`{"name":"bash"}`), 101)
	j.Append("turn/done", json.RawMessage(`{}`), 102)

	frames, last, epoch := j.Since(0, 0)
	if len(frames) != 3 {
		t.Fatalf("sinceSeq=0 应返回 3 帧，实际 %d", len(frames))
	}
	if frames[0].Seq != 1 || frames[2].Seq != 3 {
		t.Fatalf("帧序不对：%d..%d", frames[0].Seq, frames[2].Seq)
	}
	if last != 3 || epoch != 7 {
		t.Fatalf("水位/世代不对：last=%d epoch=%d", last, epoch)
	}
	// reasoning 必须原样在帧里——抽过 text 的投影会把它丢掉。
	if got := string(frames[0].Data); got != `{"message":{"content":[{"type":"reasoning","text":"想了"}]}}` {
		t.Fatalf("history 不该改写载荷，实际 %s", got)
	}
	// 按 seq 取区间也要对：跳过第 1 帧后应只剩 2、3。
	tail, _, _ := j.Since(1, 0)
	if len(tail) != 2 || tail[0].Seq != 2 {
		t.Fatalf("sinceSeq=1 应剩 2 帧且从 seq=2 起，实际 %d 帧", len(tail))
	}
}

// H14：limit 生效，且不会因为 limit 超过容量而越界。
func TestJournalSinceRespectsLimit(t *testing.T) {
	j := NewJournal(1, 10)
	for i := 0; i < 5; i++ {
		j.Append("text", nil, int64(i))
	}
	frames, _, _ := j.Since(0, 2)
	if len(frames) != 2 || frames[0].Seq != 1 || frames[1].Seq != 2 {
		t.Fatalf("limit=2 应返回前两帧，实际 %d 帧", len(frames))
	}
}

// H14：容量上限会丢最老的帧——补洞是有限度的，超出就该重新 load。
func TestJournalDropsOldestBeyondCapacity(t *testing.T) {
	j := NewJournal(1, 3)
	for i := 1; i <= 5; i++ {
		j.Append("text", nil, int64(i))
	}
	frames, last, _ := j.Since(0, 0)
	if len(frames) != 3 {
		t.Fatalf("容量 3 应只留 3 帧，实际 %d", len(frames))
	}
	if frames[0].Seq != 3 || last != 5 {
		t.Fatalf("应留最新的 3/4/5，实际首帧 %d、last %d", frames[0].Seq, last)
	}
}

// H14：世代号不匹配时，客户端必须能看出"老水位不可用"。
func TestJournalResetBumpsEpoch(t *testing.T) {
	j := NewJournal(1, 0)
	j.Append("text", nil, 1)
	j.Append("text", nil, 2)
	old := j.Epoch()

	newEpoch := j.Reset(2)
	if newEpoch == old {
		t.Fatalf("重置后世代号必须变：%d → %d", old, newEpoch)
	}
	if j.LastSeq() != 0 {
		t.Fatalf("重置后水位应归零，实际 %d", j.LastSeq())
	}
	if frames, _, _ := j.Since(0, 0); len(frames) != 0 {
		t.Fatalf("重置后不该还有旧帧，实际 %d 帧", len(frames))
	}
}

// H14：并发追加不能撞号，也不能少于应发数量。
//
// 这条替代不了 -race，但能在"锁放错位置"时立刻红。
func TestJournalAppendIsConcurrencySafe(t *testing.T) {
	j := NewJournal(1, 0)
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			j.Append("text", json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)), int64(i))
		}(i)
	}
	wg.Wait()

	if j.LastSeq() != n {
		t.Fatalf("应发 %d 个号，实际 %d", n, j.LastSeq())
	}
	frames, _, _ := j.Since(0, 0)
	seen := map[int]bool{}
	for _, f := range frames {
		if seen[f.Seq] {
			t.Fatalf("seq %d 重复", f.Seq)
		}
		seen[f.Seq] = true
	}
	if len(seen) != n {
		t.Fatalf("应有 %d 个不同 seq，实际 %d", n, len(seen))
	}
}

package transport

import (
	"errors"
	"testing"
)

// fakeSource 是内存里的 Source：msgs 原样返回，err 非 nil 时先报错。
// 记下被问过的 sessionID，用来验"空 id 根本没去问宿主"。
type fakeSource struct {
	msgs   []Message
	err    error
	asked  []string
	nCalls int
}

func (f *fakeSource) Messages(sessionID string) ([]Message, error) {
	f.nCalls++
	f.asked = append(f.asked, sessionID)
	if f.err != nil {
		return nil, f.err
	}
	return f.msgs, nil
}

// tenMessages 造 seq 1..10 的十条消息，Text 带上 seq 方便断言拿到的是哪几条。
func tenMessages() []Message {
	all := make([]Message, 0, 10)
	for i := 1; i <= 10; i++ {
		all = append(all, Message{Seq: i, Role: "user", Text: "m" + itoa(i)})
	}
	return all
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func seqsOf(ms []Message) []int {
	out := make([]int, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Seq)
	}
	return out
}

func equalInts(a, b []int) bool {
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

// 1. session.load 首屏：10 条里取最新 3 条 → 8,9,10，Total 仍是 10，且说明被截断。
func TestLoadSessionReturnsNewestPage(t *testing.T) {
	src := &fakeSource{msgs: tenMessages()}

	got, err := LoadSession(src, "s1", 3)
	if err != nil {
		t.Fatalf("LoadSession 不该出错: %v", err)
	}
	if want := []int{8, 9, 10}; !equalInts(seqsOf(got.Messages), want) {
		t.Fatalf("limit=3 应拿到 seq %v，实际 %v", want, seqsOf(got.Messages))
	}
	if got.Total != 10 {
		t.Fatalf("Total 应是全量 10（不是本页 3），实际 %d", got.Total)
	}
	if !got.Truncated {
		t.Fatalf("还有更早的 7 条没给，Truncated 必须为 true")
	}
	// 载荷也要对：不能只是 seq 对、正文串了。
	if got.Messages[0].Text != "m8" || got.Messages[2].Text != "m10" {
		t.Fatalf("正文与 seq 不匹配: %q..%q", got.Messages[0].Text, got.Messages[2].Text)
	}
}

// 2. session.history 补洞：sinceSeq=7 → 只要 8,9,10，且补洞不存在"被截断"。
func TestHistorySessionReturnsSince(t *testing.T) {
	src := &fakeSource{msgs: tenMessages()}

	p, err := HistorySession(src, "s1", 7, 0)
	if err != nil {
		t.Fatalf("HistorySession 不该出错: %v", err)
	}
	if want := []int{8, 9, 10}; !equalInts(seqsOf(p.Messages), want) {
		t.Fatalf("sinceSeq=7 应拿到 seq %v，实际 %v", want, seqsOf(p.Messages))
	}
	if p.Truncated {
		t.Fatalf("补洞没有\"更早的没给\"这回事，Truncated 必须为 false")
	}
	if p.Total != 10 {
		t.Fatalf("Total 应仍是全量 10，实际 %d", p.Total)
	}
	if p.OldestSeq != 8 || p.NewestSeq != 10 {
		t.Fatalf("OldestSeq/NewestSeq 应是 8/10，实际 %d/%d", p.OldestSeq, p.NewestSeq)
	}
}

// 3. 客户端已经追平（sinceSeq 是最后一条）→ 空页，但 Total 仍要如实报 10。
func TestHistorySessionAtTailIsEmptyPage(t *testing.T) {
	src := &fakeSource{msgs: tenMessages()}

	p, err := HistorySession(src, "s1", 10, 0)
	if err != nil {
		t.Fatalf("HistorySession 不该出错: %v", err)
	}
	if len(p.Messages) != 0 {
		t.Fatalf("sinceSeq=10 已在末尾，应返回空页，实际 %v", seqsOf(p.Messages))
	}
	if p.Messages == nil {
		t.Fatalf("空页的 Messages 应是空切片而不是 nil（JSON 会变成 null，客户端得判空）")
	}
	if p.Total != 10 {
		t.Fatalf("空页也要如实报 Total=10，实际 %d", p.Total)
	}
	if p.Truncated {
		t.Fatalf("空页不该是 Truncated，实际 true")
	}
	if p.OldestSeq != 0 || p.NewestSeq != 0 {
		t.Fatalf("空页的 OldestSeq/NewestSeq 应是 0/0，实际 %d/%d", p.OldestSeq, p.NewestSeq)
	}
}

// 4. 边角：空 sessionID 与 nil Source 都必须报错，且空 id 不许打到宿主那里。
func TestSourceRejectsEmptySessionAndNil(t *testing.T) {
	src := &fakeSource{msgs: tenMessages()}

	if _, err := LoadSession(src, "", 3); !errors.Is(err, ErrNoSuchSession) {
		t.Fatalf("LoadSession 空 sessionID 应返回 ErrNoSuchSession，实际 %v", err)
	}
	if _, err := HistorySession(src, "", 0, 0); !errors.Is(err, ErrNoSuchSession) {
		t.Fatalf("HistorySession 空 sessionID 应返回 ErrNoSuchSession，实际 %v", err)
	}
	if src.nCalls != 0 {
		t.Fatalf("空 sessionID 不该去问宿主取数，实际问了 %d 次（%v）", src.nCalls, src.asked)
	}

	// nil Source：要报错，不能 panic。
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("nil Source 不该 panic，实际 panic: %v", r)
			}
		}()
		if _, err := LoadSession(nil, "s1", 3); err == nil {
			t.Fatalf("LoadSession 传 nil Source 应报错，实际 nil")
		}
		if _, err := HistorySession(nil, "s1", 0, 0); err == nil {
			t.Fatalf("HistorySession 传 nil Source 应报错，实际 nil")
		}
	}()
}

// 5. 宿主的错误必须原样上讲——换成空结果会让客户端以为"这个会话真的没内容"。
func TestSourceErrorsPropagate(t *testing.T) {
	sentinel := errors.New("磁盘读不了: 坏帧")

	for _, tc := range []struct {
		name string
		call func(Source) error
	}{
		{"LoadSession", func(s Source) error { _, err := LoadSession(s, "s1", 3); return err }},
		{"HistorySession", func(s Source) error { _, err := HistorySession(s, "s1", 0, 0); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(&fakeSource{err: sentinel})
			if err == nil {
				t.Fatalf("%s 在 Source 报错时必须也报错，不能吐空结果", tc.name)
			}
			if !errors.Is(err, sentinel) {
				t.Fatalf("%s 应原样返回宿主错误（哨兵），实际 %v", tc.name, err)
			}
		})
	}
}

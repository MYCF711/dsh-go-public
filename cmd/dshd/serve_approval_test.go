package main

import (
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/internal/transport"
)

// fakeStdin 记录父进程写回子进程的每一行。
//
// **必须带锁**：broker 的超时/回答是在自己的 goroutine 里写回，
// 而测试在主 goroutine 里读。`go test -race` 会把这种夹具竞争报成 DATA RACE——
// 2026-11-03 装上 C 工具链第一次跑 -race 就是这里红的。
// 夹具的竞争虽然不在产品代码里，但它会淹没真正的竞争，必须先清掉。
type fakeStdin struct {
	mu    sync.Mutex
	lines []string
}

func (f *fakeStdin) Write(p []byte) (int, error) {
	f.mu.Lock()
	f.lines = append(f.lines, string(p))
	f.mu.Unlock()
	return len(p), nil
}

// wrote 返回写回来的第 n 行（n 从 0 起），没有则返回空串。
func (f *fakeStdin) wrote(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n < len(f.lines) {
		return f.lines[n]
	}
	return ""
}

// count 是已经写回的行数。
func (f *fakeStdin) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.lines)
}

func (f *fakeStdin) Close() error { return nil }

// emitLog 是测试里收集"推了哪些帧"的线程安全容器。
// broker 在它自己的 goroutine 里推帧，测试在主 goroutine 里读——
// 裸切片/裸 int 在 -race 下都是 DATA RACE（第一次跑 -race 就红了）。
type emitLog struct {
	mu    sync.Mutex
	types []string
}

func (e *emitLog) push(t string) {
	e.mu.Lock()
	e.types = append(e.types, t)
	e.mu.Unlock()
}

func (e *emitLog) len() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.types)
}

func (e *emitLog) first() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.types) == 0 {
		return ""
	}
	return e.types[0]
}

func (e *emitLog) at(i int) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i >= len(e.types) {
		return ""
	}
	return e.types[i]
}

func (e *emitLog) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.types))
	copy(out, e.types)
	return out
}

// counter 是线程安全的计数器（测试里替代裸 int 的 asked）。
type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) inc() { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *counter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// H13：界面的"允许"必须真的传回子进程。
//
// 这条链路最短的一环是"决定写回去了没有"——写不回去，子进程读不到就按拒绝处理，
// 用户点了允许却什么都没发生，而且**没有任何报错**。
func TestBrokerResolveWritesAllowBack(t *testing.T) {
	stdin := &fakeStdin{}
	emitted := &emitLog{}
	b := newApprovalBroker(stdin, func(sid, typ string, data json.RawMessage) {
		emitted.push(typ)
	})

	line := approvalRequestTag + `{"requestId":"perm-1","sessionId":"s1","tool":"write","args":{"path":"a.txt"}}`
	b.Watch(strings.NewReader(line+"\n"), nil)

	// 等到 approval/request 推出去
	deadline := time.Now().Add(2 * time.Second)
	for emitted.len() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if emitted.len() == 0 || emitted.first() != "approval/request" {
		t.Fatalf("该先推 approval/request，实际 %v", emitted.all())
	}

	if err := b.Resolve("perm-1", "allow_once"); err != nil {
		t.Fatalf("Resolve 失败：%v", err)
	}
	// 等写回
	deadline = time.Now().Add(2 * time.Second)
	for stdin.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if stdin.count() == 0 {
		t.Fatalf("决定没写回子进程——这条链就是断的")
	}
	got := stdin.wrote(0)
	if !strings.HasPrefix(got, approvalResponseTag) || !strings.Contains(got, `"allow":true`) {
		t.Fatalf("写回的行不对：%q", got)
	}
}

// H13：拒绝也要写回 false，不能只是"不写"。
//
// 不写的话子进程读不到会自己按拒绝处理——看起来对，但界面上分不清
// "用户点了拒绝"和"宿主根本没回"，两种情况的日志会一样。
func TestBrokerResolveWritesRejectBack(t *testing.T) {
	stdin := &fakeStdin{}
	b := newApprovalBroker(stdin, func(string, string, json.RawMessage) {})
	line := approvalRequestTag + `{"requestId":"perm-2","sessionId":"s1","tool":"write"}`
	b.Watch(strings.NewReader(line+"\n"), nil)

	if err := b.Resolve("perm-2", "reject_once"); err != nil {
		t.Fatalf("Resolve 失败：%v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for stdin.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if stdin.count() == 0 || !strings.Contains(stdin.wrote(0), `"allow":false`) {
		t.Fatalf("拒绝也要写回 false，实际 %d 行", stdin.count())
	}
}

// H13：认不出的 optionId → ErrBadOption（400），不能当"允许"也不能沉默。
func TestBrokerRejectsUnknownOption(t *testing.T) {
	b := newApprovalBroker(&fakeStdin{}, nil)
	if err := b.Resolve("whatever", "yolo"); err != transport.ErrBadOption {
		t.Fatalf("未知 optionId 要 ErrBadOption，实际 %v", err)
	}
}

// H13：答过一次之后再答同一条 → ErrNoSuchRequest（404）。
func TestBrokerDoubleResolveIs404(t *testing.T) {
	b := newApprovalBroker(&fakeStdin{}, func(string, string, json.RawMessage) {})
	line := approvalRequestTag + `{"requestId":"perm-3","sessionId":"s1","tool":"write"}`
	b.Watch(strings.NewReader(line+"\n"), nil)
	if err := b.Resolve("perm-3", "allow_once"); err != nil {
		t.Fatalf("第一次该成功：%v", err)
	}
	if err := b.Resolve("perm-3", "allow_once"); err != transport.ErrNoSuchRequest {
		t.Fatalf("第二次要 ErrNoSuchRequest，实际 %v", err)
	}
}

// H13：超时按**拒绝**结算，并推 approval/timeout（让界面知道是超时，不是有人拒绝）。
func TestBrokerTimeoutDenies(t *testing.T) {
	stdin := &fakeStdin{}
	var mu sync.Mutex
	var types []string
	b := newApprovalBroker(stdin, func(sid, typ string, data json.RawMessage) {
		mu.Lock()
		types = append(types, typ)
		mu.Unlock()
	})
	b.timeout = 30 * time.Millisecond

	line := approvalRequestTag + `{"requestId":"perm-4","sessionId":"s1","tool":"bash"}`
	b.Watch(strings.NewReader(line+"\n"), nil)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(types) >= 2 && stdin.count() > 0
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(types) < 2 || types[1] != "approval/timeout" {
		t.Fatalf("超时该推 approval/timeout，实际 %v", types)
	}
	if stdin.count() == 0 || !strings.Contains(stdin.wrote(0), `"allow":false`) {
		t.Fatalf("超时必须按拒绝写回，实际 %d 行", stdin.count())
	}
}

// H13：审批超时必须**报给界面**。
//
// 界面不报这个数就只能显示"等你批准"：人离开一会儿回来，命令已经被拒了，
// 屏幕上却什么都没变——分不清"还没人答"和"早就超时了"。
//
// 断言两件事：① 报的时长就是真正计时用的那个数（不是另编的常量）；
// ② 带绝对截止（Unix 毫秒）——重放时客户端要按**原来**的截止算剩余。
func TestBrokerEmitsTimeoutInPayload(t *testing.T) {
	var mu sync.Mutex
	var got json.RawMessage
	b := newApprovalBroker(&fakeStdin{}, func(sid, typ string, data json.RawMessage) {
		if typ != "approval/request" {
			return
		}
		mu.Lock()
		got = append(json.RawMessage(nil), data...)
		mu.Unlock()
	})
	b.timeout = 45 * time.Second

	before := time.Now()
	line := approvalRequestTag + `{"requestId":"perm-t","sessionId":"s1","tool":"bash"}`
	b.Watch(strings.NewReader(line+"\n"), nil)

	waitUntil := time.Now().Add(2 * time.Second)
	var raw json.RawMessage
	for {
		mu.Lock()
		raw = append(json.RawMessage(nil), got...)
		mu.Unlock()
		if len(raw) > 0 {
			break
		}
		if time.Now().After(waitUntil) {
			t.Fatal("没推出 approval/request")
		}
		time.Sleep(5 * time.Millisecond)
	}

	var p struct {
		RequestID  string `json:"requestId"`
		TimeoutMs  int64  `json:"timeoutMs"`
		DeadlineMs int64  `json:"deadlineMs"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("负载不是合法 JSON：%v", err)
	}
	if p.TimeoutMs != 45000 {
		t.Fatalf("timeoutMs 应等于真正计时用的 b.timeout（45000），实际 %d", p.TimeoutMs)
	}
	// 截止时刻应当在"发出时刻 + 45s"附近；差几秒以上说明算错了。
	if d := time.UnixMilli(p.DeadlineMs).Sub(before); d < 44*time.Second || d > 46*time.Second {
		t.Fatalf("deadlineMs 与发出时刻相差 %v，应在 45s 附近", d)
	}
}

var _ = io.EOF

// H13：断线重连时要把**还挂着**的审批重放出来。
//
// 不重放的话，客户端重连后界面上什么都没有，而子进程还在那边等——
// 看起来就是"卡住了"，且没有任何线索说明它在等一个从没显示过的问题。
func TestBrokerReplaysPendingApproval(t *testing.T) {
	b := newApprovalBroker(&fakeStdin{}, func(string, string, json.RawMessage) {})
	line := approvalRequestTag + `{"requestId":"perm-9","sessionId":"s9","tool":"write"}`
	b.Watch(strings.NewReader(line+"\n"), nil)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(b.Pending("s9")) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	pending := b.Pending("s9")
	if len(pending) != 1 {
		t.Fatalf("该有 1 条挂起，实际 %d", len(pending))
	}
	if !strings.Contains(string(pending[0]), "perm-9") {
		t.Fatalf("重放的负载里要有 requestId：%s", pending[0])
	}

	// 别的会话不该看到它。
	if got := b.Pending("other"); len(got) != 0 {
		t.Fatalf("会话之间串了：%d", len(got))
	}

	// 答过之后就不再重放。
	if err := b.Resolve("perm-9", "allow_once"); err != nil {
		t.Fatalf("Resolve 失败：%v", err)
	}
	if got := b.Pending("s9"); len(got) != 0 {
		t.Fatalf("答过之后不该再重放，实际 %d 条", len(got))
	}
}

// H13：allow_always 必须真的记住——否则界面给了这个选项、用户点了，
// 下次同一个工具还被问一遍，**选项在、语义空**比没这个选项更糟。
func TestBrokerAllowAlwaysSkipsNextAsk(t *testing.T) {
	stdin := &fakeStdin{}
	var asked counter
	b := newApprovalBroker(stdin, func(string, string, json.RawMessage) { asked.inc() })

	first := approvalRequestTag + `{"requestId":"p1","sessionId":"s1","tool":"write"}`
	b.Watch(strings.NewReader(first+"\n"), nil)
	deadline := time.Now().Add(2 * time.Second)
	for asked.get() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := b.Resolve("p1", "allow_always"); err != nil {
		t.Fatalf("Resolve 失败：%v", err)
	}

	// 同一会话、同一工具：不该再问
	second := approvalRequestTag + `{"requestId":"p2","sessionId":"s1","tool":"write"}`
	b.Watch(strings.NewReader(second+"\n"), nil)
	time.Sleep(50 * time.Millisecond)
	if asked.get() != 1 {
		t.Fatalf("第二次不该再问界面，问的次数=%d", asked.get())
	}

	// 但换个工具还得问
	third := approvalRequestTag + `{"requestId":"p3","sessionId":"s1","tool":"bash"}`
	b.Watch(strings.NewReader(third+"\n"), nil)
	deadline = time.Now().Add(2 * time.Second)
	for asked.get() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if asked.get() != 2 {
		t.Fatalf("换工具必须重新问，问的次数=%d", asked.get())
	}

	// 换个会话也还得问（不跨会话放行）
	fourth := approvalRequestTag + `{"requestId":"p4","sessionId":"s2","tool":"write"}`
	b.Watch(strings.NewReader(fourth+"\n"), nil)
	deadline = time.Now().Add(2 * time.Second)
	for asked.get() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if asked.get() != 3 {
		t.Fatalf("换会话必须重新问，问的次数=%d", asked.get())
	}
}

// allow_once 不许留下记忆：点一次就该只放行那一次。
func TestBrokerAllowOnceDoesNotStick(t *testing.T) {
	var asked counter
	b := newApprovalBroker(&fakeStdin{}, func(string, string, json.RawMessage) { asked.inc() })
	b.Watch(strings.NewReader(approvalRequestTag+`{"requestId":"q1","sessionId":"s1","tool":"write"}`+"\n"), nil)
	deadline := time.Now().Add(2 * time.Second)
	for asked.get() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := b.Resolve("q1", "allow_once"); err != nil {
		t.Fatalf("Resolve 失败：%v", err)
	}
	b.Watch(strings.NewReader(approvalRequestTag+`{"requestId":"q2","sessionId":"s1","tool":"write"}`+"\n"), nil)
	deadline = time.Now().Add(2 * time.Second)
	for asked.get() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if asked.get() != 2 {
		t.Fatalf("allow_once 之后下一次仍必须问，问的次数=%d", asked.get())
	}
}

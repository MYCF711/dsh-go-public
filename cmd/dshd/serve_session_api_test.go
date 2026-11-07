package main

// 官方 session.* 方法的**宿主侧**测试：证明这些方法读的是真东西。
//
// 分两层（两半都由本任务交付，Lead 接线只是把两半接起来）：
//
//	internal/transport/handlers_session_official_test.go  官方 zod 契约的字段与形状
//	本文件                                                真实 v4 / 真实队列 / 真实搜索
//
// 为什么不 POST /api/session.page：那 15 个 case 在 server.go 里，本轮写作用域
// 不允许改它。这里直接测 Lead 要接上去的那两个对象（host 能力 + 包装后的 Source），
// 接线正确性由编译期接口断言钉住（见文件底部 var 块）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dshgo/internal/session"
	"dshgo/internal/spi"
	"dshgo/internal/transport"
)

// officialTestSession 造一份**真实** v4 会话：turn/start → user → assistant →
// tool/call → tool/result → turn/end → user → assistant。
type officialTestSession struct {
	home string
	id   string
	cwd  string
	// 关键 seq（供断言用）。
	turnStartSeq int64
	userSeq      int64
	assistantSeq int64
	toolSeq      int64
	lastSeq      int64
}

func newOfficialTestSession(t *testing.T, home, id string) officialTestSession {
	t.Helper()
	cwd := filepath.Join(home, "workspace-"+id)
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	lg, err := session.CreateSession(home, cwd, id, "standard")
	if err != nil {
		t.Fatal(err)
	}
	out := officialTestSession{home: home, id: id, cwd: cwd}

	appendAndTrack := func(kind string, data any) int64 {
		t.Helper()
		if err := lg.Append(kind, data); err != nil {
			t.Fatal(err)
		}
		return lg.Seq() - 1
	}

	out.turnStartSeq = appendAndTrack("turn/start", session.TurnStartData(1))
	out.userSeq = appendAndTrack("user/message",
		session.UserMessage([]spi.Block{{Type: "text", Text: "please look for the NeedLE token"}}))
	out.assistantSeq = appendAndTrack("assistant/message",
		session.AssistantMessageEvent(1, 1, []map[string]any{session.TextBlock("found it")}, "stop", nil))
	appendAndTrack("tool/call", session.ToolCallEvent(1, 1, "call-1", "bash", `{"cmd":"ls"}`))
	out.toolSeq = appendAndTrack("tool/result",
		session.ToolResultEvent(1, 1, "call-1", []spi.Block{{Type: "text", Text: "a.txt"}}, false, cwd, nil))
	appendAndTrack("turn/end", session.TurnEndData(1, "stop", ""))
	appendAndTrack("user/message",
		session.UserMessage([]spi.Block{{Type: "text", Text: "second prompt"}}))
	out.lastSeq = appendAndTrack("assistant/message",
		session.AssistantMessageEvent(2, 1, []map[string]any{session.TextBlock("second answer")}, "stop", nil))

	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

// officialTestHost 组装"生产那一套"：真 sessionSource + 新包装 + 可选 prompts。
func officialTestHost(t *testing.T, home string, prompts *promptService) *officialSessionHost {
	t.Helper()
	src := newSessionSource(home, "")
	return &officialSessionHost{src: src, home: home, prompts: prompts}
}

func TestOfficialSessionHostDurableEventsAreRealV4(t *testing.T) {
	home := t.TempDir()
	fixture := newOfficialTestSession(t, home, "official-events")
	host := officialTestHost(t, home, nil)

	events, logEnd, err := host.DurableEvents(fixture.id)
	if err != nil {
		t.Fatalf("DurableEvents: %v", err)
	}
	if logEnd != fixture.lastSeq {
		t.Fatalf("logEnd=%d，真实日志末端应该是 %d", logEnd, fixture.lastSeq)
	}
	if len(events) != int(fixture.lastSeq)+1 {
		t.Fatalf("事件条数=%d，应该有 %d 条", len(events), fixture.lastSeq+1)
	}
	for i, ev := range events {
		if ev.Seq != int64(i) {
			t.Fatalf("events[%d].seq=%d，v4 seq 必须从 0 连续", i, ev.Seq)
		}
		if ev.Time <= 0 {
			t.Fatalf("events[%d].time 必须是真实墙钟时间：%d", i, ev.Time)
		}
		if len(ev.Data) == 0 {
			t.Fatalf("events[%d].data 不能为空（官方必填）", i)
		}
	}
	// 类型必须是**原样**的 v4 事件类型，不是投影出来的角色名。
	wantTypes := []string{"turn/start", "user/message", "assistant/message", "tool/call", "tool/result", "turn/end", "user/message", "assistant/message"}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Fatalf("events[%d].type=%q，应为 %q", i, events[i].Type, want)
		}
	}
	// 这条会话不存在时必须回 ErrNoSuchSession（transport 据此回 404）。
	if _, _, err := host.DurableEvents("session-does-not-exist"); !errors.Is(err, transport.ErrNoSuchSession) {
		t.Fatalf("不存在的会话必须回 ErrNoSuchSession，实际 %v", err)
	}
}

func TestOfficialSessionHostLiveStateAndRealQueueRemoval(t *testing.T) {
	home := t.TempDir()
	release := make(chan struct{})
	runner := &officialBlockingRunner{release: release}
	prompts := newPromptService(runner)
	t.Cleanup(func() {
		close(release)
		_ = prompts.Cancel("s-1")
	})
	host := officialTestHost(t, home, prompts)

	// 还没起轮子：不 active、不 running、队列空。
	live, err := host.LiveState("s-1")
	if err != nil {
		t.Fatal(err)
	}
	if live.Running || len(live.QueueIDs) != 0 {
		t.Fatalf("空闲会话不该有 live/queue：%+v", live)
	}

	first, err := prompts.SubmitPlacement(context.Background(), "s-1", "第一轮", string(transport.PlacementTurn))
	if err != nil {
		t.Fatalf("起第一轮：%v", err)
	}
	waitForOfficial(t, func() bool {
		live, _ := host.LiveState("s-1")
		return live.Running
	})
	// 第一轮在跑，第二次提交只能进真实 pending 队列。
	queued, err := prompts.SubmitPlacement(context.Background(), "s-1", "第二轮", string(transport.PlacementFollowup))
	if err != nil {
		t.Fatalf("排队第二轮：%v", err)
	}
	live, err = host.LiveState("s-1")
	if err != nil {
		t.Fatal(err)
	}
	if !live.Running || !live.Active {
		t.Fatalf("有 live turn 时 active/running 必须为真：%+v", live)
	}
	if len(live.QueueIDs) != 1 || live.QueueIDs[0] != queued.TurnID {
		t.Fatalf("QueueIDs=%v，应该是真实排队项 %q", live.QueueIDs, queued.TurnID)
	}

	// 撤掉排队项：真的从队列里消失，但**不许**顺手取消正在跑的那一轮。
	if err := host.RemoveQueueItem("s-1", queued.TurnID); err != nil {
		t.Fatalf("RemoveQueueItem: %v", err)
	}
	live, err = host.LiveState("s-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(live.QueueIDs) != 0 {
		t.Fatalf("撤掉之后队列必须为空：%v", live.QueueIDs)
	}
	if !live.Running {
		t.Fatal("撤排队项不许取消当前轮")
	}
	// 再撤一次：已经不在队列里，必须报"没有这一项"，不能假装成功。
	if err := host.RemoveQueueItem("s-1", queued.TurnID); !errors.Is(err, transport.ErrNoSuchQueueItem) {
		t.Fatalf("重复撤同一项必须 ErrNoSuchQueueItem，实际 %v", err)
	}
	// 当前轮的 id 不是排队项：同样不许被 updateQueue 干掉。
	if err := host.RemoveQueueItem("s-1", first.TurnID); !errors.Is(err, transport.ErrNoSuchQueueItem) {
		t.Fatalf("对 live turn 用 remove 必须拒绝，实际 %v", err)
	}
	if !prompts.Busy("s-1") {
		t.Fatal("拒绝之后 live turn 必须还在")
	}
}

// officialBlockingRunner 的 Run 一直挂着，直到 release 关闭或 ctx 取消。
type officialBlockingRunner struct {
	release chan struct{}
	started atomic.Int32
}

func (b *officialBlockingRunner) Run(ctx context.Context, _, _, _ string) error {
	b.started.Add(1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.release:
		return nil
	}
}

func TestOfficialSessionHostSearchFindsRealText(t *testing.T) {
	home := t.TempDir()
	hit := newOfficialTestSession(t, home, "official-search-hit")
	miss := newOfficialPlainSession(t, home, "official-search-miss", "completely different words")

	host := officialTestHost(t, home, nil)
	items, hasMore, err := host.Search("needle", 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if hasMore {
		t.Fatalf("只有一个会话命中，hasMore 必须 false")
	}
	var found *transport.OfficialSearchItem
	for i := range items {
		if items[i].SessionID == hit.id {
			found = &items[i]
		}
		if items[i].SessionID == miss.id {
			t.Fatalf("没有 needle 的会话不该命中：%v", items[i])
		}
	}
	if found == nil {
		t.Fatalf("必须命中真实正文里的 NeedLE（大小写不敏感）：%v", items)
	}
	if !strings.Contains(strings.ToLower(found.Snippet), "needle") {
		t.Fatalf("snippet 必须包含命中正文：%q", found.Snippet)
	}
	if n := len([]rune(found.Snippet)); n > 240+1 {
		// +1 允许官方裁剪时补的省略号。
		t.Fatalf("snippet 超过官方 240 code point：%d", n)
	}

	// 搜不到是空结果，不是错误。
	items, hasMore, err = host.Search("no-such-token-anywhere", 20)
	if err != nil {
		t.Fatalf("搜不到不该是错误：%v", err)
	}
	if len(items) != 0 || hasMore {
		t.Fatalf("搜不到必须空结果：%v hasMore=%v", items, hasMore)
	}
}

// newOfficialPlainSession 造一条只有一问一答的真实会话（语料可控）。
func newOfficialPlainSession(t *testing.T, home, id, text string) officialTestSession {
	t.Helper()
	cwd := filepath.Join(home, "workspace-"+id)
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	lg, err := session.CreateSession(home, cwd, id, "standard")
	if err != nil {
		t.Fatal(err)
	}
	if err := lg.Append("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: text}})); err != nil {
		t.Fatal(err)
	}
	if err := lg.Append("assistant/message",
		session.AssistantMessageEvent(1, 1, []map[string]any{session.TextBlock("ok")}, "stop", nil)); err != nil {
		t.Fatal(err)
	}
	last := lg.Seq() - 1
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	return officialTestSession{home: home, id: id, cwd: cwd, lastSeq: last}
}

// 包装必须**保留**生产 source 的全部可选能力。
func TestOfficialSessionOptionsPreserveSourceCapabilities(t *testing.T) {
	home := t.TempDir()
	fixture := newOfficialTestSession(t, home, "official-wrap")
	wrapped := withOfficialSessionOptions(home, transport.Options{Source: newSessionSource(home, "")}, nil)

	if _, ok := wrapped.Source.(transport.OfficialSessionHost); !ok {
		t.Fatal("包装后的 Source 必须实现 transport.OfficialSessionHost")
	}
	// 这几个类型断言决定 session.load / history 的真实性与性能；丢掉任何一个
	// 都会静默退化成"先投影全部再截尾"或字段变空。
	for _, probe := range []struct {
		name string
		ok   func(any) bool
	}{
		{"TailSourcer", func(v any) bool { _, ok := v.(transport.TailSourcer); return ok }},
		{"SeqSourcer", func(v any) bool { _, ok := v.(transport.SeqSourcer); return ok }},
		{"UsageSourcer", func(v any) bool { _, ok := v.(transport.UsageSourcer); return ok }},
		{"WindowSourcer", func(v any) bool { _, ok := v.(transport.WindowSourcer); return ok }},
		{"ModelSourcer", func(v any) bool { _, ok := v.(transport.ModelSourcer); return ok }},
		{"EffortSourcer", func(v any) bool { _, ok := v.(transport.EffortSourcer); return ok }},
		{"EffortSelectionSourcer", func(v any) bool { _, ok := v.(transport.EffortSelectionSourcer); return ok }},
		{"GoalSourcer", func(v any) bool { _, ok := v.(transport.GoalSourcer); return ok }},
		{"PermissionSourcer", func(v any) bool { _, ok := v.(transport.PermissionSourcer); return ok }},
		{"CwdSourcer", func(v any) bool { _, ok := v.(transport.CwdSourcer); return ok }},
	} {
		if !probe.ok(wrapped.Source) {
			t.Fatalf("包装之后丢了 %s——session.load/history 会静默退化", probe.name)
		}
	}
	// 包装后依然能真的读出这条会话（不是只通过类型断言）。
	host := wrapped.Source.(transport.OfficialSessionHost)
	events, logEnd, err := host.DurableEvents(fixture.id)
	if err != nil {
		t.Fatalf("包装后的宿主读不到真会话：%v", err)
	}
	if logEnd != fixture.lastSeq || len(events) == 0 {
		t.Fatalf("包装后的宿主读到 logEnd=%d events=%d", logEnd, len(events))
	}
}

// 非生产 Source（测试假源、第三方宿主）一律原样返回，保持 501 闭合分派。
func TestOfficialSessionOptionsLeaveForeignSourceUntouched(t *testing.T) {
	foreign := &foreignSource{}
	opt := withOfficialSessionOptions(t.TempDir(), transport.Options{Source: foreign}, nil)
	if opt.Source != foreign {
		t.Fatal("不是 *sessionSource 时不许替换 Source")
	}
	if _, ok := opt.Source.(transport.OfficialSessionHost); ok {
		t.Fatal("外来 Source 不该凭空获得官方能力")
	}
}

// foreignSource 是最小 Source 实现（只有 Messages）。
type foreignSource struct{}

func (foreignSource) Messages(string) ([]transport.Message, error) { return nil, nil }

// waitForOfficial 等到条件成立（最多 2s），否则让测试失败。
func waitForOfficial(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}

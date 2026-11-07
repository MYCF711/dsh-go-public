package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// view 是纯函数，所以布局可以被断言——TUI 最难查的 bug 就是布局，
// 藏在绘制调用里就没法测了。
func newTestApp(w, h int) *app {
	s := &Screen{width: w, height: h}
	// 旧布局用例默认钉在日序号 0（bold）；每日轮换用例会显式替换 now。
	date := time.Date(1970, 1, 1, 12, 0, 0, 0, time.Local)
	return &app{scr: s, sid: "session-abcdef12", now: func() time.Time { return date }}
}

// 每屏必须**恰好**是终端高度：多一行会顶掉滚屏，少一行底部会残留上一帧。
func TestViewFillsExactlyTerminalHeight(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 30}, {40, 10}} {
		a := newTestApp(size[0], size[1])
		got := a.view()
		if len(got) != size[1] {
			t.Fatalf("%dx%d：view 给了 %d 行，应为 %d", size[0], size[1], len(got), size[1])
		}
		for i, ln := range got {
			if w := len([]rune(ln)); w > size[0] {
				t.Fatalf("第 %d 行宽 %d 超过终端宽度 %d", i, w, size[0])
			}
		}
	}
}

// 正文很多时只显示尾部：人关心的是最后发生了什么。
func TestViewShowsTailOfTranscript(t *testing.T) {
	a := newTestApp(80, 12)
	for i := 0; i < 100; i++ {
		a.push("行 " + string(rune('A'+i%26)))
	}
	got := a.view()
	if !strings.Contains(strings.Join(got, "\n"), "行 ") {
		t.Fatal("正文没显示")
	}
	// 最上面那几行不该是第 1 条（尾部截取）
	if strings.Contains(got[0], "开始") {
		t.Fatal("似乎显示了头部而不是尾部")
	}
}

// 输入行必须显示当前输入（含中文）。
func TestViewShowsInput(t *testing.T) {
	a := newTestApp(80, 12)
	a.input = []rune("你好 dsh")
	got := a.view()
	if !strings.Contains(got[len(got)-1], "你好 dsh") {
		t.Fatalf("输入行没显示：%q", got[len(got)-1])
	}
}

// 帧映射：assistant/message 的两种落法都要能取到正文。
func TestHandleFrameTextBothShapes(t *testing.T) {
	for _, payload := range []string{
		`{"text":"直接给 text"}`,
		`{"message":{"content":[{"type":"text","text":"包在 message 里"}]}}`,
		`{"content":[{"type":"text","text":"顶层 content"}]}`,
	} {
		a := newTestApp(80, 12)
		a.handleFrame(ServerFrame{Type: "assistant/message", Data: json.RawMessage(payload)})
		if !strings.Contains(strings.Join(a.lines, "\n"), "text") &&
			!strings.Contains(strings.Join(a.lines, "\n"), "包在") &&
			!strings.Contains(strings.Join(a.lines, "\n"), "顶层") {
			t.Fatalf("%s 没被映射成正文：%v", payload, a.lines)
		}
	}
}

// 工具帧要显示成一行卡片摘要，而不是整段 JSON。
func TestHandleFrameToolCard(t *testing.T) {
	a := newTestApp(80, 20)
	a.handleFrame(ServerFrame{Type: "tool/call", Data: json.RawMessage(`{"name":"bash","title":"列出目录"}`)})
	a.handleFrame(ServerFrame{Type: "tool/result", Data: json.RawMessage(`{"summary":"退出码 0，输出 12 字节"}`)})
	joined := strings.Join(a.lines, "\n")
	if !strings.Contains(joined, "bash") || !strings.Contains(joined, "列出目录") {
		t.Fatalf("工具卡没画出来：%s", joined)
	}
	if !strings.Contains(joined, "退出码 0") {
		t.Fatalf("结果摘要没画出来：%s", joined)
	}
	if a.tools != 1 {
		t.Fatalf("工具计数应为 1，实际 %d", a.tools)
	}
}

// 未知命令要明确说不支持，不能静默吞掉——否则人以为命令生效了。
func TestUnknownCommandSaysSo(t *testing.T) {
	a := newTestApp(80, 12)
	a.command("/nope")
	if a.err == "" || !strings.Contains(a.err, "还不支持") {
		t.Fatalf("未知命令没报：%q", a.err)
	}
}

// /clear 清屏；/help 有输出。
func TestSlashCommands(t *testing.T) {
	a := newTestApp(80, 12)
	a.push("一些内容")
	a.command("/help")
	if len(a.lines) == 0 {
		t.Fatal("/help 没有输出")
	}
	a.command("/clear")
	if len(a.lines) != 0 {
		t.Fatalf("/clear 之后还剩 %d 行", len(a.lines))
	}
}

// 差分渲染：上一帧相同的行必须**跳过**，否则每帧都在重画整屏。
//
// 这条以前是**假测试**：它把"哪些行变了"的算术在测试里自己算了一遍，
// 而 `Screen.Render` 根本没做差分（`prev` 只是声明了没人用）——
// 也就是说，它测的是测试自己的循环，渲染器坏成什么样它都是绿的。
// 现在改成调真的 Render，并断言"没变的行只有光标定位、没有清行与重写"。
func TestRenderSkipsUnchangedRows(t *testing.T) {
	var buf strings.Builder
	scr := newScreenTo(&buf, 20, 4)

	scr.Render([]string{"a", "b", "c", "d"})
	if got := strings.Count(buf.String(), "\x1b[2K"); got != 4 {
		t.Fatalf("首帧没有上一帧可比，4 行都要画，实际清行 %d 次", got)
	}

	buf.Reset()
	scr.Render([]string{"a", "b", "c", "CHANGED"})
	out := buf.String()
	if got := strings.Count(out, "\x1b[2K"); got != 1 {
		t.Fatalf("只改了最后一行，应当只清 1 行，实际 %d：%q", got, out)
	}
	// 未变的三行仍要被定位到（否则光标会落在错的行上，画到别处去）
	for _, want := range []string{"\x1b[1;1H", "\x1b[2;1H", "\x1b[3;1H", "\x1b[4;1H"} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少光标定位 %q：%q", want, out)
		}
	}
	if !strings.Contains(out, "CHANGED") {
		t.Fatalf("变了的那行必须重写：%q", out)
	}
}

// 状态行：未知用量显示 "?"，不要显示 0 —— "不知道"和"没有"是两件事。
func TestFmtTokensDistinguishesUnknownFromZero(t *testing.T) {
	if got := fmtTokens(0); got != "?" {
		t.Fatalf("0 应显示 ?，实际 %q", got)
	}
	if got := fmtTokens(1234); got != "1.2k" {
		t.Fatalf("1234 应显示 1.2k，实际 %q", got)
	}
	if got := fmtTokens(999); got != "999" {
		t.Fatalf("999 应原样显示，实际 %q", got)
	}
}

// 状态行要带上 ctx 与 tps（有数据时），没数据时不许编。
func TestViewStatusShowsCtxAndTPS(t *testing.T) {
	a := newTestApp(100, 12)
	a.ctxTokens = 12345
	a.tps = 42.5
	got := strings.Join(a.view(), "\n")
	if !strings.Contains(got, "ctx 12.3k") {
		t.Fatalf("状态行没显示上下文用量：%s", got)
	}
	if !strings.Contains(got, "tok/s") {
		t.Fatalf("状态行没显示速率：%s", got)
	}

	b := newTestApp(100, 12)
	if strings.Contains(strings.Join(b.view(), "\n"), "tok/s") {
		t.Fatal("没有速率数据时不该显示 tok/s")
	}
}

// /resume：列出来的序号必须能选中对应会话；越界/非数字不许误切。
func TestResumePickingByNumber(t *testing.T) {
	a := newTestApp(100, 14)
	a.pickFrom = []sessionRow{
		{ID: "session-11111111", Title: "第一个"},
		{ID: "session-22222222", Title: "第二个"},
	}

	// 越界：不该切，但也要取消选择状态
	if a.pick("9") {
		t.Fatal("越界序号不该被认为已消费")
	}
	if a.pickFrom != nil {
		t.Fatal("越界之后应当取消选择状态")
	}

	// 非数字：不消费
	a.pickFrom = []sessionRow{{ID: "session-22222222"}}
	if a.pick("你好") {
		t.Fatal("非数字不该被当成选择")
	}
	if a.pickFrom != nil {
		t.Fatal("输入非数字后应当取消选择状态")
	}
}

// 导出：角色必须保留（导出的东西多半是回看"谁说了什么"），
// 且序号要带上——不然回看时对不上原始会话。
func TestRenderMarkdownKeepsRolesAndSeq(t *testing.T) {
	md := renderMarkdown([]message{
		{Seq: 1, Role: "user", Text: "你好"},
		{Seq: 2, Role: "assistant", Text: "在的"},
		{Seq: 3, Role: "tool", Text: "输出"},
	})
	for _, want := range []string{"你", "助手", "工具", "#1", "#2", "#3", "你好", "在的"} {
		if !strings.Contains(md, want) {
			t.Fatalf("导出内容缺 %q：%s", want, md)
		}
	}
}

// 收到审批请求：必须把问题摆到屏幕上，并把状态行切成"等你批准"。
// 不处理它 = 这一轮永远卡住，而屏幕看起来毫无变化。
func TestApprovalRequestIsShown(t *testing.T) {
	a := newTestApp(120, 16)
	a.handleFrame(ServerFrame{Type: "approval/request", Data: json.RawMessage(
		`{"requestId":"req-1","toolCall":{"title":"bash · rm -rf build/"}}`)})

	if a.pending.id != "req-1" {
		t.Fatalf("没记住 requestId：%q", a.pending.id)
	}
	joined := strings.Join(a.lines, "\n")
	if !strings.Contains(joined, "需要你批准") || !strings.Contains(joined, "rm -rf") {
		t.Fatalf("审批内容没显示：%s", joined)
	}
	status := a.view()[len(a.view())-2]
	if !strings.Contains(status, "等你批准") {
		t.Fatalf("状态行没切成等待态：%s", status)
	}
}

// 有审批在等时，y/a/n 是给审批的，不许被当成普通输入吃掉。
func TestApprovalKeysAreNotSwallowed(t *testing.T) {
	a := newTestApp(120, 16)
	var got []string
	a.approve = func(reqID, opt string) error {
		got = append(got, reqID+":"+opt)
		return nil
	}
	for _, k := range []rune{'y', 'a', 'n'} {
		a.input = nil
		a.pending.id = "req-1"
		a.handleRune(k)
		if len(a.input) != 0 {
			t.Fatalf("%q 被当成输入塞进了输入行", k)
		}
	}
	want := []string{"req-1:allow_once", "req-1:allow_always", "req-1:reject_once"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("按键→选项映射不对：%v", got)
	}
}

// 没有审批在等时，y/a/n 就是普通字符。
func TestNormalKeysStillTypeWhenNoApproval(t *testing.T) {
	a := newTestApp(120, 16)
	a.handleRune('y')
	if string(a.input) != "y" {
		t.Fatalf("没有审批时 y 应当正常输入，实际 %q", string(a.input))
	}
}

// ---------------------------------------------------------------- 审批倒计时

// fakeClock 是可控时钟。倒计时不能靠真等待来验：真等待既慢又会抖
// （CI 上一卡，断言"还剩 12s"就红了），而且验不出"秒数到底有没有变小"。
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newApprovalApp 建一个带假时钟的 app：起点固定，便于断言"还剩几秒"。
func newApprovalApp(w, h int) (*app, *fakeClock) {
	a := newTestApp(w, h)
	clk := &fakeClock{t: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)}
	a.now = clk.now
	return a, clk
}

// approvalFrame 造一条**带超时字段**的 approval/request（宿主现在会发这两个）。
// deadlineMs 用绝对时刻：重放时它不会被重新算成满额。
func approvalFrame(reqID, title string, deadline time.Time, total time.Duration) ServerFrame {
	return ServerFrame{Type: "approval/request", Data: json.RawMessage(fmt.Sprintf(
		`{"requestId":%q,"sessionId":"s1","timeoutMs":%d,"deadlineMs":%d,"toolCall":{"title":%q}}`,
		reqID, total.Milliseconds(), deadline.UnixMilli(), title))}
}

// statusLine 取状态行（倒数第二行：最后一行是输入行）。
func statusLine(a *app) string {
	got := a.view()
	return got[len(got)-2]
}

// 1. 收到审批 → 状态行带剩余秒数。
//
// 没有这个数的时候，人离开一会儿回来只看到"命令被拒了"，屏幕上什么都没变；
// 而屏幕上什么都没有的那段时间，宿主其实一直在倒计时。
func TestApprovalCountdownShowsRemainingSeconds(t *testing.T) {
	a, clk := newApprovalApp(120, 16)
	a.handleFrame(approvalFrame("req-1", "bash · rm -rf build/", clk.t.Add(30*time.Second), 30*time.Second))

	status := statusLine(a)
	if !strings.Contains(status, "等你批准（30s）") {
		t.Fatalf("状态行没显示剩余秒数：%q", status)
	}
	// 倒计时不能把标题或按键提示挤没：看不见怎么答比看不见标题更糟。
	if !strings.Contains(status, "rm -rf build/") || !strings.Contains(status, "[y]") ||
		!strings.Contains(status, "[n]") {
		t.Fatalf("标题或按键提示被挤掉了：%q", status)
	}
}

// 2. 时间推进 → 秒数变小；≤5s 换成更强提示（`!! 还有 3s`）。
func TestApprovalCountdownTicksDownAndWarns(t *testing.T) {
	a, clk := newApprovalApp(120, 16)
	a.handleFrame(approvalFrame("req-1", "bash · rm", clk.t.Add(30*time.Second), 30*time.Second))

	clk.advance(18 * time.Second)
	a.tickStatus() // 生产路径：主循环每 250ms 调一次
	if s := statusLine(a); !strings.Contains(s, "等你批准（12s）") {
		t.Fatalf("18s 之后应剩 12s：%q", s)
	}

	clk.advance(7 * time.Second) // 还剩 5s：进告警档
	a.tickStatus()
	if s := statusLine(a); !strings.Contains(s, "!! 还有 5s") {
		t.Fatalf("≤5s 应换成更强提示：%q", s)
	}
	clk.advance(2 * time.Second)
	a.tickStatus()
	if s := statusLine(a); !strings.Contains(s, "!! 还有 3s") {
		t.Fatalf("剩余 3s 应显示 !! 还有 3s：%q", s)
	}
	if a.pending.id != "req-1" {
		t.Fatalf("还没到点就不该清掉 pending：%q", a.pending.id)
	}
}

// 3. 到 0 → 显示"已超时，按拒绝处理"并清掉 pending；y/a/n 不再被当成应答。
//
// 第二条尤其重要：pending 没清干净的话，用户按 y 只会拿到 404，
// 而屏幕上看起来他明明还能答——这正是"莫名其妙被拒"的另一种形态。
func TestApprovalExpiresAtZero(t *testing.T) {
	a, clk := newApprovalApp(120, 16)
	var answered []string
	a.approve = func(reqID, opt string) error {
		answered = append(answered, reqID+":"+opt)
		return nil
	}
	a.handleFrame(approvalFrame("req-1", "bash · rm -rf build/", clk.t.Add(10*time.Second), 10*time.Second))

	clk.advance(10 * time.Second)
	a.tickStatus()

	if a.pending.id != "" || !a.pending.deadline.IsZero() {
		t.Fatalf("超时后 pending 应清干净：id=%q deadline=%v", a.pending.id, a.pending.deadline)
	}
	if s := statusLine(a); !strings.Contains(s, "已超时，按拒绝处理") {
		t.Fatalf("状态行没说超时：%q", s)
	}
	// 正文也要留一行：状态行会被下一个事件顶掉，人回来时要能查到被拒的是哪条。
	joined := strings.Join(a.lines, "\n")
	if !strings.Contains(joined, "审批超时") || !strings.Contains(joined, "rm -rf build/") {
		t.Fatalf("正文没留下超时的那一条：\n%s", joined)
	}

	a.input = nil
	a.handleRune('y')
	if len(answered) != 0 {
		t.Fatalf("超时之后不该再发出应答：%v", answered)
	}
	if string(a.input) != "y" {
		t.Fatalf("超时之后 y 应退回普通输入，实际 %q", string(a.input))
	}
}

// 4. 收到 approval/answered → 倒计时立刻消失，且这一问不再被判超时。
func TestApprovalAnsweredClearsCountdown(t *testing.T) {
	a, clk := newApprovalApp(120, 16)
	a.handleFrame(approvalFrame("req-1", "bash · rm -rf build/", clk.t.Add(30*time.Second), 30*time.Second))
	if !strings.Contains(statusLine(a), "等你批准（30s）") {
		t.Fatalf("前置：应当先有倒计时：%q", statusLine(a))
	}

	a.handleFrame(ServerFrame{Type: "approval/answered",
		Data: json.RawMessage(`{"requestId":"req-1","optionId":"allow_once"}`)})

	if a.pending.id != "" || !a.pending.deadline.IsZero() {
		t.Fatalf("答过之后 pending 没清干净：id=%q deadline=%v", a.pending.id, a.pending.deadline)
	}
	if s := statusLine(a); strings.Contains(s, "等你批准") || strings.Contains(s, "还有") {
		t.Fatalf("答过之后状态行还挂着倒计时：%q", s)
	}

	// 已经答过的这一问，时钟再怎么走也不该冒出"已超时"。
	clk.advance(time.Minute)
	a.tickStatus()
	if s := statusLine(a); strings.Contains(s, "已超时") {
		t.Fatalf("答过的审批不该再判超时：%q", s)
	}
}

// 5. 收到 approval/timeout 帧 → 立刻清掉倒计时，而且只说一次。
func TestApprovalTimeoutFrameClearsCountdown(t *testing.T) {
	a, clk := newApprovalApp(120, 16)
	a.handleFrame(approvalFrame("req-1", "bash · rm", clk.t.Add(30*time.Second), 30*time.Second))

	a.handleFrame(ServerFrame{Type: "approval/timeout",
		Data: json.RawMessage(`{"requestId":"req-1","sessionId":"s1"}`)})

	if a.pending.id != "" || !a.pending.deadline.IsZero() {
		t.Fatalf("收到 approval/timeout 后 pending 应清干净：id=%q", a.pending.id)
	}
	if s := statusLine(a); !strings.Contains(s, "已超时，按拒绝处理") {
		t.Fatalf("状态行没说超时：%q", s)
	}

	// 宿主那一帧和本地 tick 不该各记一行（重复的"超时"会让人以为拒了两次）。
	before := len(a.lines)
	a.tickStatus()
	if len(a.lines) != before {
		t.Fatalf("重复记了超时行：%d → %d", before, len(a.lines))
	}
}

// 5b. 上一问超时的提示不许"继承"到下一问。
//
// 不清的话，用户答完新的一问，状态行会突然冒出"审批已超时"——
// 看起来像刚答的那条超时了，而这正是让人不敢信任审批提示的那类错觉。
func TestApprovalTimeoutNoteDoesNotLeakIntoNextQuestion(t *testing.T) {
	a, clk := newApprovalApp(120, 16)
	a.handleFrame(approvalFrame("req-1", "bash · rm", clk.t.Add(10*time.Second), 10*time.Second))
	clk.advance(10 * time.Second)
	a.tickStatus()
	if !strings.Contains(statusLine(a), "已超时") {
		t.Fatalf("前置：应先看到超时：%q", statusLine(a))
	}

	a.approve = func(string, string) error { return nil }
	a.handleFrame(approvalFrame("req-2", "write · a.txt", clk.t.Add(30*time.Second), 30*time.Second))
	a.handleRune('y') // 答掉下一问

	if s := statusLine(a); strings.Contains(s, "已超时") {
		t.Fatalf("答完下一问后状态行还挂着上一问的超时提示：%q", s)
	}
}

// 6. 老宿主没有超时字段 → 明说"倒计时不可用"，**不许编一个假秒数**，也不许自行判超时。
func TestApprovalWithoutTimeoutShowsNoFakeCountdown(t *testing.T) {
	a, _ := newApprovalApp(120, 16)
	// 老宿主的形状：只有 requestId / toolCall（当前 HEAD 之前就是这样）
	a.handleFrame(ServerFrame{Type: "approval/request", Data: json.RawMessage(
		`{"requestId":"req-1","toolCall":{"title":"bash · rm -rf build/"}}`)})

	if a.pending.id != "req-1" {
		t.Fatalf("没记住 requestId：%q", a.pending.id)
	}
	s := statusLine(a)
	if !strings.Contains(s, "倒计时不可用") {
		t.Fatalf("宿主没报超时时应明说倒计时不可用：%q", s)
	}
	if strings.Contains(s, "还有") || strings.Contains(s, "（0s）") {
		t.Fatalf("不该编出一个假秒数：%q", s)
	}

	// 没有时限就不该自己判超时：清了 pending 等于凭空拒掉一次真实请求。
	a.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	a.tickStatus()
	if a.pending.id != "req-1" {
		t.Fatal("没有时限时不该自行判超时")
	}
}

// 7. 只有时长、没有绝对截止时也能量出倒计时（向后兼容中间形态的宿主）。
func TestApprovalCountdownFromDurationOnly(t *testing.T) {
	a, _ := newApprovalApp(120, 16)
	a.handleFrame(ServerFrame{Type: "approval/request", Data: json.RawMessage(
		`{"requestId":"req-1","timeoutMs":15000,"toolCall":{"title":"bash · ls"}}`)})
	if s := statusLine(a); !strings.Contains(s, "等你批准（15s）") {
		t.Fatalf("只有 timeoutMs 时也应按它算剩余：%q", s)
	}
}

// 8. 断线重连重放的是**原来那条负载**：截止时刻不能被重算成满额。
//
// 重放时若按"现在 + 时长"重算，屏幕上说还有 30s，而宿主 10s 后就拒了——
// 那比不显示倒计时更坏，因为它主动给了个错的预期。
func TestApprovalReplayKeepsOriginalDeadline(t *testing.T) {
	a, clk := newApprovalApp(120, 16)
	deadline := clk.t.Add(30 * time.Second)
	a.handleFrame(approvalFrame("req-1", "bash · rm", deadline, 30*time.Second))

	// 断线 20 秒
	clk.advance(20 * time.Second)
	// 宿主重放同一条（截止时刻还是当初那个）
	a.handleFrame(approvalFrame("req-1", "bash · rm", deadline, 30*time.Second))

	if s := statusLine(a); !strings.Contains(s, "等你批准（10s）") {
		t.Fatalf("重放应按原截止时刻算剩 10s，而不是满额 30s：%q", s)
	}
}

// 9. 时钟不一致不许**替宿主提前拒掉**：刚收到的请求按绝对时刻已经"过期"时，
// 只可能是两边时钟不一样，退回按时长算——提前判超时等于把这一问从用户手里拿走，
// 而宿主那边明明还挂着等。
func TestApprovalClockSkewDoesNotPreemptivelyDeny(t *testing.T) {
	a, clk := newApprovalApp(120, 16)
	// 客户端时钟比宿主快 10 分钟：这条的 deadline 落在过去
	a.handleFrame(approvalFrame("req-1", "bash · rm", clk.t.Add(-10*time.Minute), 30*time.Second))

	if a.pending.id != "req-1" {
		t.Fatalf("不该把这一问清掉：%q", a.pending.id)
	}
	if s := statusLine(a); !strings.Contains(s, "等你批准（30s）") {
		t.Fatalf("时钟不一致时应退回按时长算，而不是立刻说超时：%q", s)
	}
}

// ---------------------------------------------------------------- 三种结算：屏幕上必须说清是哪一种

// approvalCancelFrame 造一条宿主取消结算的帧。
//
// 契约里 `approval/cancel` 的负载与 `approval/request` **同形**（宿主
// serve_approval.go 把当初那条 payload 原样再发一次），所以这里也带 toolCall.title：
// 界面要靠它说清取消的是哪一问。
func approvalCancelFrame(reqID, title string) ServerFrame {
	return ServerFrame{Type: "approval/cancel", Data: json.RawMessage(fmt.Sprintf(
		`{"requestId":%q,"sessionId":"s1","toolCall":{"title":%q}}`, reqID, title))}
}

// bodyLineWith 找正文里第一条含 sub 的行；没有就返回空串。
func bodyLineWith(a *app, sub string) string {
	for _, ln := range a.lines {
		if strings.Contains(ln, sub) {
			return ln
		}
	}
	return ""
}

// 10. answered：按 y 之后正文留下的是「→ allow_once」——不是超时，也不是取消。
func TestApprovalAnsweredLeavesAnsweredWords(t *testing.T) {
	a, clk := newApprovalApp(120, 16)
	a.approve = func(string, string) error { return nil }
	a.handleFrame(approvalFrame("req-1", "bash · rm -rf build/", clk.t.Add(30*time.Second), 30*time.Second))

	a.handleRune('y')
	// 宿主把这次结算回推一帧（契约白名单里有 approval/answered）：
	// 同一次结算不许因此多出一行别的说法。
	a.handleFrame(ServerFrame{Type: "approval/answered",
		Data: json.RawMessage(`{"requestId":"req-1","optionId":"allow_once"}`)})

	line := bodyLineWith(a, "→ allow_once")
	if line == "" {
		t.Fatalf("应答后正文没留下选择：\n%s", strings.Join(a.lines, "\n"))
	}
	for _, bad := range []string{"超时", "取消"} {
		if strings.Contains(line, bad) {
			t.Fatalf("answered 被说成了「%s」：%q", bad, line)
		}
	}
	if s := statusLine(a); strings.Contains(s, "等你批准") || strings.Contains(s, "已超时") {
		t.Fatalf("答过之后状态行应回到普通状态：%q", s)
	}
}

// 11. timeout：正文说的是「审批超时……已按拒绝处理」。
func TestApprovalTimeoutLeavesTimeoutWords(t *testing.T) {
	a, clk := newApprovalApp(120, 16)
	a.handleFrame(approvalFrame("req-1", "bash · rm -rf build/", clk.t.Add(30*time.Second), 30*time.Second))
	a.handleFrame(ServerFrame{Type: "approval/timeout",
		Data: json.RawMessage(`{"requestId":"req-1","sessionId":"s1"}`)})

	line := bodyLineWith(a, "审批超时")
	if line == "" || !strings.Contains(line, "rm -rf build/") {
		t.Fatalf("正文没记下超时的那一条：\n%s", strings.Join(a.lines, "\n"))
	}
	if strings.Contains(line, "取消") {
		t.Fatalf("timeout 被说成了取消：%q", line)
	}
	if s := statusLine(a); !strings.Contains(s, "已超时，按拒绝处理") {
		t.Fatalf("状态行没说超时：%q", s)
	}
}

// 12. cancel：必须说清**是宿主取消了这一轮**，并收掉倒计时与按键提示。
//
// 这一条是 H13 的尾巴：宿主按拒绝结算并取消轮次后推 approval/cancel，
// 界面落到 default 分支什么都不做——屏幕上那个气泡还挂着「等你批准」，
// 按 y 只会拿到 404。即使气泡收掉，只说「审批结束」也分不清三种结算。
func TestApprovalCancelSaysHostCancelled(t *testing.T) {
	a, clk := newApprovalApp(120, 16)
	var answered []string
	a.approve = func(reqID, opt string) error {
		answered = append(answered, reqID+":"+opt)
		return nil
	}
	a.handleFrame(approvalFrame("req-1", "bash · rm -rf build/", clk.t.Add(30*time.Second), 30*time.Second))
	if !strings.Contains(statusLine(a), "等你批准（30s）") {
		t.Fatalf("前置：应当先有倒计时：%q", statusLine(a))
	}

	a.handleFrame(approvalCancelFrame("req-1", "bash · rm -rf build/"))

	if a.pending.id != "" || !a.pending.deadline.IsZero() {
		t.Fatalf("收到 approval/cancel 后倒计时/按键应立刻收掉：id=%q deadline=%v",
			a.pending.id, a.pending.deadline)
	}
	line := bodyLineWith(a, "取消")
	if line == "" {
		t.Fatalf("正文没留下「被取消」那一行：\n%s", strings.Join(a.lines, "\n"))
	}
	if !strings.Contains(line, "宿主") || !strings.Contains(line, "session.cancel") {
		t.Fatalf("没说是谁取消的：%q", line)
	}
	if !strings.Contains(line, "rm -rf build/") {
		t.Fatalf("没说取消的是哪一问：%q", line)
	}
	for _, bad := range []string{"审批超时", "已按拒绝处理", "→ allow"} {
		if strings.Contains(line, bad) {
			t.Fatalf("取消被说成了别的结算（%q）：%q", bad, line)
		}
	}
	if s := statusLine(a); strings.Contains(s, "等你批准") || strings.Contains(s, "还有") ||
		strings.Contains(s, "已超时") || strings.Contains(s, "[y]") {
		t.Fatalf("取消后状态行应回到普通状态：%q", s)
	}

	// 这一问已经不在宿主那边了：y 必须退回普通输入，不许再发应答去拿 404。
	a.input = nil
	a.handleRune('y')
	if len(answered) != 0 {
		t.Fatalf("取消之后不该再发出应答：%v", answered)
	}
	if string(a.input) != "y" {
		t.Fatalf("取消之后 y 应退回普通输入，实际 %q", string(a.input))
	}
}

// 13. cancel 与 timeout **互不覆盖**：先取消，再来一条晚到的超时帧，
// 屏幕上不许把「取消」改回「超时」。
//
// 帧序真的会这样出现：取消那一刻，宿主那条超时 goroutine 正要醒；
// 断线重放时也会把旧帧再送一遍。只要界面还认 pending，晚到的那条就会
// 把结论改掉——用户看到的是一句和事实相反的说明。
func TestApprovalCancelIsNotOverwrittenByLateTimeout(t *testing.T) {
	a, clk := newApprovalApp(120, 16)
	a.handleFrame(approvalFrame("req-1", "bash · rm -rf build/", clk.t.Add(30*time.Second), 30*time.Second))
	a.handleFrame(approvalCancelFrame("req-1", "bash · rm -rf build/"))
	before := append([]string(nil), a.lines...)

	clk.advance(2 * time.Minute)
	a.handleFrame(ServerFrame{Type: "approval/timeout",
		Data: json.RawMessage(`{"requestId":"req-1","sessionId":"s1"}`)})
	a.tickStatus()

	joined := strings.Join(a.lines, "\n")
	if strings.Contains(joined, "审批超时") || strings.Contains(joined, "已按拒绝处理") {
		t.Fatalf("晚到的超时帧把「已取消」改成了「超时」：\n%s", joined)
	}
	if len(a.lines) != len(before) {
		t.Fatalf("晚到的超时帧又记了一行：%d → %d\n%s", len(before), len(a.lines), joined)
	}
	if n := strings.Count(joined, "审批已取消"); n != 1 {
		t.Fatalf("取消只该说一次，实际 %d 次：\n%s", n, joined)
	}
	if s := statusLine(a); strings.Contains(s, "已超时") {
		t.Fatalf("状态行被晚到的超时帧改了：%q", s)
	}
}

// 14. 三种结算在屏幕上必须**长得不一样**：同一句「审批结束了」会让人
// 分不清是自己答的、超时了、还是被取消了——而这三种后续动作完全不同。
func TestApprovalThreeSettlementsReadDifferently(t *testing.T) {
	a1, c1 := newApprovalApp(120, 16)
	a1.approve = func(string, string) error { return nil }
	a1.handleFrame(approvalFrame("req-1", "bash · rm", c1.t.Add(30*time.Second), 30*time.Second))
	a1.handleRune('y')

	a2, c2 := newApprovalApp(120, 16)
	a2.handleFrame(approvalFrame("req-1", "bash · rm", c2.t.Add(30*time.Second), 30*time.Second))
	a2.handleFrame(ServerFrame{Type: "approval/timeout",
		Data: json.RawMessage(`{"requestId":"req-1"}`)})

	a3, c3 := newApprovalApp(120, 16)
	a3.handleFrame(approvalFrame("req-1", "bash · rm", c3.t.Add(30*time.Second), 30*time.Second))
	a3.handleFrame(approvalCancelFrame("req-1", "bash · rm"))

	lines := []string{
		bodyLineWith(a1, "→"),
		bodyLineWith(a2, "审批超时"),
		bodyLineWith(a3, "审批已取消"),
	}
	for i, ln := range lines {
		if ln == "" {
			t.Fatalf("第 %d 种结算在正文里没有一行说明：%q", i+1, ln)
		}
	}
	for i := 0; i < len(lines); i++ {
		for j := i + 1; j < len(lines); j++ {
			if lines[i] == lines[j] {
				t.Fatalf("第 %d 与第 %d 种结算在屏幕上分不开：%q", i+1, j+1, lines[i])
			}
		}
	}
}

// ---------------------------------------------------------------- 工具卡折叠

// 默认折叠：tool/call 那一行必须还在，tool/result 的内容不许出现在 view() 里。
// 内容本身仍然要记下来——折叠是显示层的事，不是把历史丢掉。
func TestToolResultCollapsedByDefault(t *testing.T) {
	a := newTestApp(100, 20)
	a.handleFrame(ServerFrame{Type: "tool/call", Data: json.RawMessage(`{"name":"bash","title":"列出目录"}`)})
	a.handleFrame(ServerFrame{Type: "tool/result", Data: json.RawMessage(`{"summary":"退出码 0，输出 12 字节"}`)})

	if a.showToolResults {
		t.Fatal("默认应当是折叠的")
	}
	if !strings.Contains(strings.Join(a.lines, "\n"), "退出码 0") {
		t.Fatal("结果行应当被记下来，展开时要能立刻显示")
	}
	screen := strings.Join(a.view(), "\n")
	if !strings.Contains(screen, "bash") || !strings.Contains(screen, "列出目录") {
		t.Fatalf("折叠后工具卡那一行必须还在：\n%s", screen)
	}
	if strings.Contains(screen, "退出码 0") {
		t.Fatalf("默认折叠时结果内容不该出现在屏幕上：\n%s", screen)
	}
}

// Ctrl+T 展开：已有的结果行立刻出现；再按一次消失。
func TestToggleToolResults(t *testing.T) {
	a := newTestApp(100, 20)
	a.handleFrame(ServerFrame{Type: "tool/call", Data: json.RawMessage(`{"name":"bash","title":"列出目录"}`)})
	a.handleFrame(ServerFrame{Type: "tool/result", Data: json.RawMessage(`{"summary":"退出码 0，输出 12 字节"}`)})

	if strings.Contains(strings.Join(a.view(), "\n"), "退出码 0") {
		t.Fatal("折叠状态下不该看到结果")
	}

	a.handleRune(20) // Ctrl+T
	if !a.showToolResults {
		t.Fatal("Ctrl+T 应当切到展开")
	}
	if !strings.Contains(strings.Join(a.view(), "\n"), "退出码 0") {
		t.Fatalf("展开后结果行没出现：\n%s", strings.Join(a.view(), "\n"))
	}

	a.handleRune(20) // Ctrl+T 再来一次
	if a.showToolResults {
		t.Fatal("再按一次应当切回折叠")
	}
	if strings.Contains(strings.Join(a.view(), "\n"), "退出码 0") {
		t.Fatal("折叠后结果行应当再次消失")
	}

	// /tools 是同一个动作的可打字入口
	a.command("/tools")
	if !a.showToolResults {
		t.Fatal("/tools 应当也能展开")
	}
}

// 错误结果在折叠状态下**必须照样显示**：折叠是省地方，不是把失败藏起来。
// 两种落法（顶层 isError / message.isError）都要认。
func TestErrorResultVisibleWhenCollapsed(t *testing.T) {
	cases := []string{
		`{"isError":true,"summary":"bash: 没有这个文件"}`,
		`{"message":{"isError":true,"content":[{"type":"text","text":"拒绝执行 rm"}]}}`,
	}
	for _, payload := range cases {
		a := newTestApp(100, 20)
		a.handleFrame(ServerFrame{Type: "tool/call", Data: json.RawMessage(`{"name":"bash","title":"rm"}`)})
		a.handleFrame(ServerFrame{Type: "tool/result", Data: json.RawMessage(payload)})

		if a.showToolResults {
			t.Fatal("这条用例要在折叠状态下验")
		}
		joined := strings.Join(a.view(), "\n")
		if !strings.Contains(joined, "ERROR") {
			t.Fatalf("%s：错误结果被折叠藏起来了：\n%s", payload, joined)
		}
		if !strings.Contains(joined, "没有这个文件") && !strings.Contains(joined, "拒绝执行") {
			t.Fatalf("%s：错误正文没显示：\n%s", payload, joined)
		}
	}
}

// 折叠/展开/搜索都不能破坏"每屏恰好高度行"的契约。
func TestViewHeightStableAcrossFoldAndSearch(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 30}, {40, 10}} {
		w, h := size[0], size[1]
		a := newTestApp(w, h)
		for i := 0; i < 40; i++ {
			a.push("正文 " + string(rune('a'+i%26)))
		}
		a.handleFrame(ServerFrame{Type: "tool/call", Data: json.RawMessage(`{"name":"bash","title":"ls"}`)})
		a.handleFrame(ServerFrame{Type: "tool/result", Data: json.RawMessage(`{"summary":"ok"}`)})
		a.handleFrame(ServerFrame{Type: "tool/result", Data: json.RawMessage(`{"isError":true,"summary":"失败"}`)})

		check := func(stage string) {
			got := a.view()
			if len(got) != h {
				t.Fatalf("%dx%d/%s：view 给了 %d 行，应为 %d", w, h, stage, len(got), h)
			}
			for i, ln := range got {
				if n := len([]rune(ln)); n > w {
					t.Fatalf("%dx%d/%s：第 %d 行宽 %d 超过 %d", w, h, stage, i, n, w)
				}
			}
		}
		check("折叠")
		a.handleRune(20)
		check("展开")
		a.handleRune(20)
		check("再折叠")
		a.handleRune(6)
		for _, r := range "正文" {
			a.handleRune(r)
		}
		check("搜索中")
		a.handleSpecial("esc")
		check("退出搜索")
	}
}

// ---------------------------------------------------------------- 会话内搜索

// 搜索：进搜索模式 → 输入关键词 → 状态行报命中数；Esc 退出后不再显示。
// 命中数只算**显示得出来的行**：被折叠的工具结果不参与，否则数字和屏幕对不上。
func TestSearchCountsMatchesAndEscClears(t *testing.T) {
	a := newTestApp(100, 20)
	a.push("第一行 有 needle 在这里")
	a.push("第二行 没有关键词")
	a.push("第三行 也有 Needle") // 大小写不同也要命中

	a.handleFrame(ServerFrame{Type: "tool/call", Data: json.RawMessage(`{"name":"bash","title":"grep"}`)})
	a.handleFrame(ServerFrame{Type: "tool/result", Data: json.RawMessage(`{"summary":"needle 在第 7 行"}`)})

	a.handleRune(6) // Ctrl+F
	if !a.searching {
		t.Fatal("Ctrl+F 应当进入搜索模式")
	}
	if got := a.view(); !strings.HasPrefix(got[len(got)-1], "/ ") {
		t.Fatalf("搜索时输入行前缀应当是 /：%q", got[len(got)-1])
	}
	for _, r := range "needle" {
		a.handleRune(r)
	}
	if a.searchQuery != "needle" {
		t.Fatalf("查询串没跟上输入：%q", a.searchQuery)
	}

	got := a.view()
	status := got[len(got)-2]
	if !strings.Contains(status, "匹配 2 处") {
		t.Fatalf("状态行没报命中数（应为 2 处，折叠的结果行不算）：%q", status)
	}

	a.handleSpecial("esc")
	if a.searching || a.searchQuery != "" || len(a.input) != 0 {
		t.Fatalf("Esc 之后搜索状态没清干净：searching=%v query=%q input=%q",
			a.searching, a.searchQuery, string(a.input))
	}
	after := a.view()
	if strings.Contains(after[len(after)-2], "匹配") {
		t.Fatalf("退出搜索后状态行不该还留着命中数：%q", after[len(after)-2])
	}
	if !strings.HasPrefix(after[len(after)-1], markerUser+" ") {
		t.Fatalf("退出搜索后输入行前缀应回到 %s：%q", markerUser, after[len(after)-1])
	}
}

// 搜索串不是消息：Enter 只退出搜索，不许把它当 prompt 发出去。
func TestSearchEnterDoesNotSend(t *testing.T) {
	a := newTestApp(100, 20)
	a.push("一些内容")
	a.handleRune(6)
	for _, r := range "内容" {
		a.handleRune(r)
	}
	a.handleRune(13) // Enter
	if a.searching || a.searchQuery != "" {
		t.Fatal("Enter 应当退出搜索并清空查询")
	}
	if strings.Contains(strings.Join(a.lines, "\n"), "› 内容") {
		t.Fatal("搜索串被当成消息发出去了")
	}
}

// 超过 maxLines 裁剪之后，meta 必须仍然和 lines 对齐——
// 错位的后果是"折叠藏错行"，屏幕上看起来只是少了一行，极难查。
func TestTrimKeepsMetaAligned(t *testing.T) {
	a := newTestApp(100, 12)
	call := ServerFrame{Type: "tool/call", Data: json.RawMessage(`{"name":"bash","title":"ls"}`)}
	result := ServerFrame{Type: "tool/result", Data: json.RawMessage(`{"summary":"ok"}`)}
	for i := 0; i < 600; i++ {
		a.handleFrame(call)
		a.handleFrame(result)
	}
	if len(a.lines) != maxLines || len(a.meta) != maxLines {
		t.Fatalf("裁剪后 lines=%d meta=%d，应都为 %d", len(a.lines), len(a.meta), maxLines)
	}
	if strings.Contains(strings.Join(a.view(), "\n"), "⎿ ok") {
		t.Fatal("折叠时结果行不该出现")
	}
	a.handleRune(20)
	if !strings.Contains(strings.Join(a.view(), "\n"), "⎿ ok") {
		t.Fatal("展开后结果行没出现——meta 很可能和 lines 错位了")
	}
}

// 键盘解码：ASCII 与 Ctrl 键必须真能发到 handleRune。
//
// 这条是**前置缺陷**的回归测试：旧解码器"等到续字节才解码"，
// 对多字节字符成立，但 ASCII 永远等不到续字节——于是字母、Ctrl+T、
// Ctrl+F、y/a/n（审批）、Ctrl+C（退出）在真机上按了全都没反应，
// 而单测直接调 handleRune，看不出这个问题。
//
// 方向键的期望在加滚动时**变了**：现在 ESC[A 认成 up，而不是让 Esc 兜底
// （旧行为是"按一次方向键等于清空输入行"）。ESC 本身仍然要在没有后续字节时
// 发出去，见 TestBareEscStaysEsc / TestEscTimeoutMeansBareEsc。
func TestReadKeysDecodesASCIIAndControlKeys(t *testing.T) {
	rch := make(chan rune, 64)
	sch := make(chan string, 8)
	readKeysFrom(strings.NewReader("ab中\x14\x06\x1b[A\n"), rch, sch)
	close(rch)
	close(sch)

	var got []rune
	for r := range rch {
		got = append(got, r)
	}
	want := []rune{'a', 'b', '中', 20, 6, '\n'}
	if string(got) != string(want) {
		t.Fatalf("解码结果 %q（%v），期望 %q（%v）", string(got), got, string(want), want)
	}

	var specials []string
	for s := range sch {
		specials = append(specials, s)
	}
	if strings.Join(specials, ",") != "up" {
		t.Fatalf("方向键应当只解出 up，实际 %v（Esc 也被顺带发了？）", specials)
	}
}

// 上下文进度条：知道上限才画百分比，不知道就只报数字。
// 只给一半信息（有分子没分母）的进度条比没有更糟——数字看着精确，其实分母是猜的。
func TestCtxPercent(t *testing.T) {
	cases := []struct {
		used, window int
		want         string
	}{
		{0, 0, "?"},                              // 什么都还不知道
		{12000, 0, "12.0k"},                      // 知道用了多少，不知道上限 → 只报数字
		{50000, 100000, "50.0k/100.0k 50.0%"},    // 两样都知道 → 读数（条画在 footer 第一行）
		{200000, 100000, "200.0k/100.0k 100.0%"}, // 超了也不报成 120%
	}
	for _, c := range cases {
		if got := ctxPercent(c.used, c.window); got != c.want {
			t.Fatalf("ctxPercent(%d,%d) = %q，期望 %q", c.used, c.window, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- 滚动

// fillLines 往里塞 n 行**带编号**的正文。编号是为了能断言"屏幕上第 k 行是第几条"——
// 只断言"有内容"的话，窗口错一行这种 bug 是看不出来的。
func fillLines(a *app, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ln := fmt.Sprintf("行 %03d", i)
		a.push(ln)
		out = append(out, ln)
	}
	return out
}

// 1. 默认贴底：显示的是最后几行，状态行不出现"回看中"。
func TestViewFollowsTailByDefault(t *testing.T) {
	a := newTestApp(80, 12)
	body := a.bodyHeight()
	all := fillLines(a, 100)

	got := a.view()
	for i := 0; i < body; i++ {
		want := all[len(all)-body+i]
		if got[i] != want {
			t.Fatalf("贴底时第 %d 行应为 %q，实际 %q", i, want, got[i])
		}
	}
	if a.scroll != 0 {
		t.Fatalf("没按过翻页，scroll 应为 0，实际 %d", a.scroll)
	}
	if status := got[len(got)-2]; strings.Contains(status, "回看中") {
		t.Fatalf("贴底时状态行不该说在回看：%q", status)
	}
}

//  2. PgUp 一屏：窗口整体往前挪**正好一屏**（新的一屏紧挨着旧的一屏：
//     新末行的下一行就是旧首行，既不重叠也不留缝），
//     状态行同时报出还剩多少行才回到底部。
func TestPageUpMovesBackOneScreen(t *testing.T) {
	a := newTestApp(80, 12)
	body := a.bodyHeight()
	all := fillLines(a, 100)

	before := a.view()
	a.handleSpecial("pgup")
	after := a.view()

	if after[0] == before[0] {
		t.Fatalf("PgUp 之后首行没变：%q", after[0])
	}
	if want := all[len(all)-body-1]; after[body-1] != want {
		t.Fatalf("新一屏的末行应为 %q，实际 %q", want, after[body-1])
	}
	if want := all[len(all)-body]; before[0] != want {
		t.Fatalf("旧一屏的首行应为 %q，实际 %q", want, before[0])
	}
	// 新末行（len-body-1）与旧首行（len-body）是两个相邻的行号：
	// 一屏正好接上，中间既不重也不漏。
	if want := all[len(all)-2*body]; after[0] != want {
		t.Fatalf("PgUp 之后首行应为 %q，实际 %q", want, after[0])
	}
	if a.scroll != body {
		t.Fatalf("PgUp 之后 scroll 应为 %d，实际 %d", body, a.scroll)
	}

	status := after[len(after)-2]
	// 报出的数必须就是本次 PgUp 跨过的一屏行数，而不是钉死旧 footer 的 9 行。
	if want := fmt.Sprintf("↑ 回看中（还有 %d 行）· PgDn/Ctrl+End 回到底部", body); !strings.Contains(status, want) {
		t.Fatalf("状态行没报回看与剩余行数（应为 %q）：%q", want, status)
	}
}

// 3. 既有契约不能破：任何滚动位置上 view() 都**恰好**是终端高度（宽度也不超）。
func TestViewHeightStableAtAnyScroll(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 30}, {40, 10}} {
		w, h := size[0], size[1]
		a := newTestApp(w, h)
		fillLines(a, 200)
		// 塞一行会被折叠的工具结果：过滤发生在取窗口**之前**，
		// 位置算错的话行数或宽度先崩。
		a.handleFrame(ServerFrame{Type: "tool/result", Data: json.RawMessage(`{"summary":"ok"}`)})

		check := func(stage string) {
			got := a.view()
			if len(got) != h {
				t.Fatalf("%dx%d/%s：view 给了 %d 行，应为 %d", w, h, stage, len(got), h)
			}
			for i, ln := range got {
				if n := len([]rune(ln)); n > w {
					t.Fatalf("%dx%d/%s：第 %d 行宽 %d 超过 %d", w, h, stage, i, n, w)
				}
			}
		}
		check("贴底")
		for i := 0; i < 12; i++ { // 12 屏足够越过顶部
			a.handleSpecial("pgup")
			check(fmt.Sprintf("PgUp #%d", i+1))
		}
		for i := 0; i < 12; i++ {
			a.handleSpecial("pgdn")
			check(fmt.Sprintf("PgDn #%d", i+1))
		}
		check("回到贴底")
	}
}

// 4. 回看中收到新帧：屏幕上那几行**不动**（不是自动跳到底）。
func TestNewFrameDoesNotDisturbScrollPosition(t *testing.T) {
	a := newTestApp(80, 16)
	body := a.bodyHeight()
	fillLines(a, 60)

	a.handleSpecial("pgup")
	before := a.view()
	scrollBefore := a.scroll

	// 一帧三行（正文两行 + 段末空行），全部可见 → 位置要跟着锚住
	a.handleFrame(ServerFrame{Type: "assistant/message",
		Data: json.RawMessage(`{"text":"新来的第一行\n新来的第二行"}`)})
	after := a.view()

	for i := 0; i < body; i++ {
		if before[i] != after[i] {
			t.Fatalf("回看位置被新帧打乱了：第 %d 行 %q → %q", i, before[i], after[i])
		}
	}
	if a.scroll != scrollBefore+3 {
		t.Fatalf("锚定后 scroll 应为 %d，实际 %d", scrollBefore+3, a.scroll)
	}
	if strings.Contains(strings.Join(after[:body], "\n"), "新来的") {
		t.Fatalf("回看中不该自动跳到底部：\n%s", strings.Join(after[:body], "\n"))
	}
}

// 4b. 回看中撞上 maxLines 裁头：窗口里那段内容仍然不动
// （scroll 是相对底部的距离，裁头不该换算成位移——算错的话内容会整体跳一行）。
func TestTrimDoesNotShiftScrolledView(t *testing.T) {
	a := newTestApp(80, 12)
	body := a.bodyHeight()
	for i := 0; i < maxLines; i++ {
		a.push(fmt.Sprintf("行 %04d", i))
	}
	a.handleSpecial("pgup")
	before := a.view()[:body]

	for i := 0; i < 10; i++ { // 每条都会裁掉最前面一行
		a.push(fmt.Sprintf("新 %04d", i))
	}
	after := a.view()[:body]
	for i := 0; i < body; i++ {
		if before[i] != after[i] {
			t.Fatalf("裁头之后回看位置偏移了：第 %d 行 %q → %q", i, before[i], after[i])
		}
	}
}

// 5. Ctrl+End 回到底部 = 恢复跟随：再来新帧就要显示最新的。
func TestCtrlEndResumesFollowing(t *testing.T) {
	a := newTestApp(80, 16)
	body := a.bodyHeight()
	all := fillLines(a, 60)

	a.handleSpecial("pgup")
	a.handleSpecial("pgup")
	a.handleSpecial("ctrl+end")
	if a.scroll != 0 {
		t.Fatalf("Ctrl+End 之后 scroll 应为 0，实际 %d", a.scroll)
	}
	got := a.view()
	if got[0] != all[len(all)-body] {
		t.Fatalf("Ctrl+End 之后应贴底，首行 %q 期望 %q", got[0], all[len(all)-body])
	}
	if strings.Contains(got[len(got)-2], "回看中") {
		t.Fatalf("贴底后状态行不该还说回看：%q", got[len(got)-2])
	}

	a.handleFrame(ServerFrame{Type: "assistant/message", Data: json.RawMessage(`{"text":"这是最新的"}`)})
	if !strings.Contains(strings.Join(a.view()[:body], "\n"), "这是最新的") {
		t.Fatalf("恢复跟随后新帧应当出现在屏幕上：\n%s", strings.Join(a.view()[:body], "\n"))
	}

	// Ctrl+Home 到最上面：首行就是第一行
	a.handleSpecial("ctrl+home")
	if a.view()[0] != all[0] {
		t.Fatalf("Ctrl+Home 之后首行应为 %q，实际 %q", all[0], a.view()[0])
	}
	if a.scroll != a.maxScroll() {
		t.Fatalf("Ctrl+Home 之后 scroll 应为上限 %d，实际 %d", a.maxScroll(), a.scroll)
	}
}

// 6. 内容比屏幕短：PgUp/↑ 都不改变显示，也没有"回看中"提示。
func TestScrollNoopWhenContentShorterThanScreen(t *testing.T) {
	a := newTestApp(80, 12)
	fillLines(a, 3)
	before := strings.Join(a.view(), "\n")

	a.handleSpecial("pgup")
	if a.scroll != 0 {
		t.Fatalf("内容不到一屏时不该滚得动，scroll=%d", a.scroll)
	}
	if strings.Join(a.view(), "\n") != before {
		t.Fatal("内容是同一批，屏幕不该有任何变化")
	}
	if strings.Contains(before, "回看中") {
		t.Fatal("内容不到一屏时不该显示回看提示")
	}

	a.handleSpecial("up") // 方向键同理
	if a.scroll != 0 || strings.Join(a.view(), "\n") != before {
		t.Fatalf("方向键在短内容上也不该动：scroll=%d", a.scroll)
	}
}

// 边界：滚到顶/底不越界——顶到第一行、底回跟随，按多少次都一样。
func TestScrollStopsAtTopAndBottom(t *testing.T) {
	a := newTestApp(80, 12)
	body := a.bodyHeight()
	all := fillLines(a, 40)

	for i := 0; i < 20; i++ {
		a.handleSpecial("pgup")
	}
	if a.scroll != a.maxScroll() {
		t.Fatalf("应当停在顶部 scroll=%d，实际 %d", a.maxScroll(), a.scroll)
	}
	if got := a.view(); got[0] != all[0] {
		t.Fatalf("到顶后首行应为 %q，实际 %q", all[0], got[0])
	}

	for i := 0; i < 20; i++ {
		a.handleSpecial("pgdn")
	}
	if a.scroll != 0 {
		t.Fatalf("应当停在底部 scroll=0，实际 %d", a.scroll)
	}
	if got := a.view(); got[0] != all[len(all)-body] {
		t.Fatalf("到底后首行应为 %q，实际 %q", all[len(all)-body], got[0])
	}
}

// 方向键（↑↓）单行滚动：一次一行，往下按回来必须回到原处。
func TestArrowKeysScrollOneLine(t *testing.T) {
	a := newTestApp(80, 12)
	body := a.bodyHeight()
	all := fillLines(a, 50)

	a.handleSpecial("up")
	if a.scroll != 1 {
		t.Fatalf("↑ 应当是单行滚动，scroll=%d", a.scroll)
	}
	if got := a.view(); got[0] != all[len(all)-body-1] {
		t.Fatalf("↑ 之后首行应为 %q，实际 %q", all[len(all)-body-1], got[0])
	}

	a.handleSpecial("down")
	if a.scroll != 0 {
		t.Fatalf("↓ 应当回到贴底，scroll=%d", a.scroll)
	}
	if got := a.view(); got[0] != all[len(all)-body] {
		t.Fatalf("↓ 之后首行应为 %q，实际 %q", all[len(all)-body], got[0])
	}
}

// 翻页键走的是 ESC 序列：**不许**顺带触发 Esc 把输入行清掉
// （"翻页把打了一半的话删了"比"翻页没反应"更难接受）。
func TestScrollKeysDoNotClearInput(t *testing.T) {
	a := newTestApp(80, 12)
	fillLines(a, 100)
	for _, r := range "打了一半" {
		a.handleRune(r)
	}

	rch := make(chan rune, 8)
	sch := make(chan string, 8)
	readKeysFrom(strings.NewReader("\x1b[5~\x1b[6~\x1b[1;5H\x1b[1;5F"), rch, sch)
	close(rch)
	close(sch)
	for s := range sch {
		a.handleSpecial(s)
	}
	if string(a.input) != "打了一半" {
		t.Fatalf("翻页把输入行清了：%q", string(a.input))
	}
	if a.scroll != 0 { // PgUp PgDn Ctrl+Home Ctrl+End 走完回到贴底
		t.Fatalf("四个键走完应当回到贴底，scroll=%d", a.scroll)
	}
}

// 解码：PgUp/PgDn/Ctrl+Home/Ctrl+End/↑/↓ 各认成名，一个都不许落进输入行。
func TestReadKeysMapsScrollKeys(t *testing.T) {
	rch := make(chan rune, 64)
	sch := make(chan string, 32)
	// 末尾那个孤立 ESC：输入结束前也要交代掉
	readKeysFrom(strings.NewReader("\x1b[5~\x1b[6~\x1b[1;5H\x1b[1;5F\x1b[B\x1b[A\x1b"), rch, sch)
	close(rch)
	close(sch)

	var specials []string
	for s := range sch {
		specials = append(specials, s)
	}
	want := "pgup,pgdn,ctrl+home,ctrl+end,down,up,esc"
	if got := strings.Join(specials, ","); got != want {
		t.Fatalf("按键解码 = %v，期望 %s", specials, want)
	}
	var runes []rune
	for r := range rch {
		runes = append(runes, r)
	}
	if len(runes) != 0 {
		t.Fatalf("转义序列漏进了输入行：%q", string(runes))
	}
}

// 孤立 ESC 必须还是 Esc：后面跟普通按键时，Esc 先发、那个键照常当输入。
func TestBareEscStaysEsc(t *testing.T) {
	rch := make(chan rune, 8)
	sch := make(chan string, 8)
	readKeysFrom(strings.NewReader("x\x1bQ"), rch, sch)
	close(rch)
	close(sch)

	var specials []string
	for s := range sch {
		specials = append(specials, s)
	}
	if strings.Join(specials, ",") != "esc" {
		t.Fatalf("ESC+普通键 应当只发一次 esc，实际 %v", specials)
	}
	var runes []rune
	for r := range rch {
		runes = append(runes, r)
	}
	if string(runes) != "xQ" {
		t.Fatalf("ESC 后面的普通键不该被吞：%q", string(runes))
	}
}

// scriptedSource 是"可控的超时"源：真机上的孤立 ESC 靠它复现
// （strings.Reader 永远读到 EOF，测不出这条路径）。
type scriptedSource struct {
	steps []byteStep
	i     int
}

type byteStep struct {
	c       byte
	timeout bool
	ok      bool
}

func (s *scriptedSource) next(bool) (byte, bool, bool) {
	if s.i >= len(s.steps) {
		return 0, false, false
	}
	st := s.steps[s.i]
	s.i++
	return st.c, st.timeout, st.ok
}

// ESC 之后等不到后续字节 = 人按的就是 Esc。这条钉的是**生产路径**：
// 没有超时的话，要么 Esc 永远不生效，要么 PgUp 被当成 Esc。
func TestEscTimeoutMeansBareEsc(t *testing.T) {
	src := &scriptedSource{steps: []byteStep{
		{c: 0x1b, ok: true},
		{timeout: true},
		{c: 'a', ok: true},
		{}, // 输入结束
	}}
	rch := make(chan rune, 8)
	sch := make(chan string, 8)
	decodeKeys(src, rch, sch)
	close(rch)
	close(sch)

	var specials []string
	for s := range sch {
		specials = append(specials, s)
	}
	if strings.Join(specials, ",") != "esc" {
		t.Fatalf("超时后应当发一次 esc，实际 %v", specials)
	}
	var runes []rune
	for r := range rch {
		runes = append(runes, r)
	}
	if string(runes) != "a" {
		t.Fatalf("超时之后的正常按键不该被吞：%q", string(runes))
	}
}

// 生产路径整条：stdin 的字节由 goroutine 泵进来，ESC 之后靠超时区分
// "孤立 ESC"与"转义序列前缀"。用真管道跑一遍——strings.Reader 走不到定时器。
func TestChanSourceOverPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败：%v", err)
	}
	defer r.Close()

	rch := make(chan rune, 8)
	sch := make(chan string, 8)
	done := make(chan struct{})
	// 等待窗口取得比 escTimeout 宽得多：这条验的是"到点要发 esc"，
	// 不是"50ms 这个数准不准"（机器一卡，紧的超时会让测试误红）。
	go func() {
		decodeKeys(&chanSource{ch: pumpBytes(r), gap: 300 * time.Millisecond}, rch, sch)
		close(done)
	}()

	// 1) 序列：ESC [ 5 ~ 在同一个窗口内到齐 → pgup，且不额外发 esc
	if _, err := w.WriteString("\x1b[5~"); err != nil {
		t.Fatalf("写管道失败：%v", err)
	}
	select {
	case s := <-sch:
		if s != "pgup" {
			t.Fatalf("管道里的 PgUp 解成了 %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("PgUp 没有被认出来")
	}

	// 2) 孤立 ESC：后面没有字节 → 超时 → esc
	if _, err := w.WriteString("\x1b"); err != nil {
		t.Fatalf("写管道失败：%v", err)
	}
	select {
	case s := <-sch:
		if s != "esc" {
			t.Fatalf("孤立 ESC 解成了 %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("孤立 ESC 没被超时认出来——真机上 Esc 会是死的")
	}

	// 3) 输入关掉：解码器必须退出，不能把 goroutine 和主循环一直挂着
	w.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("输入关闭后解码器没有退出")
	}
}

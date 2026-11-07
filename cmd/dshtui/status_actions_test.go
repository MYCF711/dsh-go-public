package main

import (
	"fmt"
	"strings"
	"testing"
)

// statusActionApp 只填界面已经拥有的真实状态；连接态也由真实事件入口绑定。
func statusActionApp(w, h int) *app {
	a := newTestApp(w, h)
	a.sid = "session-real-123"
	a.model = "model-A"
	a.effort = "high"
	a.ctxTokens = 12345
	a.ctxWindow = 100000
	a.tools = 2
	a.handleConnectionEvent(connectionEvent{kind: wsOpen, sid: a.sid, subscription: 7, generation: 1})
	return a
}

// driveStatusActionBytes 从真实终端字节进入既有解码器，再走生产事件入口。
// 不直接调用弹层方法，避免“测试自己算出坐标、实现却认不出鼠标”的假绿。
func driveStatusActionBytes(a *app, raw string) {
	runes := make(chan rune, 256)
	specials := make(chan string, 256)
	readKeysFrom(strings.NewReader(raw), runes, specials)
	close(runes)
	close(specials)
	for r := range runes {
		a.handleRune(r)
	}
	for k := range specials {
		a.handleSpecial(k)
	}
}

func statusActionRow(a *app) string {
	rows := a.view()
	return rows[len(rows)-2]
}

// statusActionX 把实际渲染出来的字段文字换成 1-based 终端列坐标。
func statusActionX(t *testing.T, a *app, rendered string, last bool) int {
	t.Helper()
	row := statusActionRow(a)
	at := strings.Index(row, rendered)
	if last {
		at = strings.LastIndex(row, rendered)
	}
	if at < 0 {
		t.Fatalf("状态行找不到点击字段 %q：%q", rendered, row)
	}
	return displayWidth(row[:at]) + displayWidth(rendered)/2 + 1
}

func statusActionClick(a *app, x, y int) {
	driveStatusActionBytes(a, fmt.Sprintf("\x1b[<0;%d;%dM", x, y))
}

func statusActionOpen(t *testing.T, a *app, rendered string, last bool) {
	t.Helper()
	x := statusActionX(t, a, rendered, last)
	statusActionClick(a, x, a.scr.height-1)
}

func TestStatusMouseRealBytesOpenEveryField(t *testing.T) {
	tests := []struct {
		field    string
		rendered string
		last     bool
		want     []string
	}{
		{field: "model", rendered: "model-A", want: []string{"model: model-A", "session id: session-real-123", "连接状态: 已连接", "session.load"}},
		{field: "effort", rendered: "high effort", want: []string{"effort: high", "model: model-A", "连接状态: 已连接", "session.load"}},
		{field: "context", rendered: "ctx 12.3k/100.0k 12.3%", want: []string{"contextTokens: 12345", "contextWindow: 100000", "连接状态: 已连接", "未知不填 0"}},
		{field: "tools", rendered: "工具 2", want: []string{"tool count: 2", "tool/call", "连接状态: 已连接"}},
		{field: "session", rendered: "会话 session-", last: true, want: []string{"session id: session-real-123", "model: model-A", "effort: high", "contextTokens: 12345", "contextWindow: 100000", "tool count: 2", "连接状态: 已连接"}},
	}
	for _, size := range [][2]int{{120, 36}, {80, 24}} {
		for _, tc := range tests {
			t.Run(fmt.Sprintf("%dx%d/%s", size[0], size[1], tc.field), func(t *testing.T) {
				a := statusActionApp(size[0], size[1])
				a.input = []rune("draft")
				statusActionOpen(t, a, tc.rendered, tc.last)
				screen := strings.Join(a.view(), "\n")
				if !strings.Contains(screen, "状态详情 · "+tc.field) {
					t.Fatalf("真实字节点击没打开 %s 弹层：\n%s", tc.field, screen)
				}
				for _, want := range tc.want {
					if !strings.Contains(screen, want) {
						t.Errorf("%s 弹层缺真实详情 %q：\n%s", tc.field, want, screen)
					}
				}
				if got := string(a.input); got != "draft" {
					t.Fatalf("鼠标序列漏进草稿：%q", got)
				}
			})
		}
	}
}

func TestStatusPopupRetoggleEscReleaseAndInput(t *testing.T) {
	a := statusActionApp(120, 36)
	a.input = []rune("draft")
	statusActionOpen(t, a, "model-A", false)
	x, y := statusActionX(t, a, "model-A", false), a.scr.height-1

	// release 不是第二次点击，不能把刚打开的面板收掉。
	driveStatusActionBytes(a, fmt.Sprintf("\x1b[<0;%d;%dm", x, y))
	if !strings.Contains(strings.Join(a.view(), "\n"), "状态详情 · model") {
		t.Fatal("左键 release 误关了弹层")
	}
	statusActionClick(a, x, y)
	if strings.Contains(strings.Join(a.view(), "\n"), "状态详情") {
		t.Fatal("再次点击同一字段没有关闭弹层")
	}
	if got := string(a.input); got != "draft" {
		t.Fatalf("点击改变了草稿：%q", got)
	}

	statusActionOpen(t, a, "high effort", false)
	driveStatusActionBytes(a, "z")
	if got := string(a.input); got != "draftz" {
		t.Fatalf("弹层打开时普通输入被吞：%q", got)
	}
	driveStatusActionBytes(a, "\x1b")
	if strings.Contains(strings.Join(a.view(), "\n"), "状态详情") {
		t.Fatal("Esc 没有关闭弹层")
	}
	if got := string(a.input); got != "draftz" {
		t.Fatalf("关闭弹层的 Esc 顺带清了草稿：%q", got)
	}

	statusActionOpen(t, a, "model-A", false)
	statusActionOpen(t, a, "high effort", false)
	screen := strings.Join(a.view(), "\n")
	if !strings.Contains(screen, "状态详情 · effort") || strings.Contains(screen, "状态详情 · model") {
		t.Fatalf("点击另一字段没有切换弹层：\n%s", screen)
	}
}

func TestStatusPopupUnknownDoesNotInventZero(t *testing.T) {
	for _, tc := range []struct {
		field    string
		rendered string
		want     []string
	}{
		{field: "model", rendered: "?", want: []string{"model: 未知", "连接状态: 未知"}},
		{field: "effort", rendered: "? effort", want: []string{"effort: 未知", "缺失或空值就是未知"}},
		{field: "context", rendered: "ctx ?", want: []string{"contextTokens: 未知", "contextWindow: 未知", "未知不填 0"}},
		{field: "tools", rendered: "工具 0", want: []string{"tool count: 0", "当前会话已接收"}},
		{field: "session", rendered: "会话 session-", want: []string{"model: 未知", "effort: 未知", "contextTokens: 未知", "contextWindow: 未知", "连接状态: 未知"}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			a := newTestApp(120, 36)
			a.sid = "session-unknown"
			statusActionOpen(t, a, tc.rendered, tc.field == "session")
			screen := strings.Join(a.view(), "\n")
			for _, want := range tc.want {
				if !strings.Contains(screen, want) {
					t.Errorf("未知详情缺 %q：\n%s", want, screen)
				}
			}
			for _, bad := range []string{"model: 0", "effort: 0", "contextTokens: 0", "contextWindow: 0", "连接状态: 0"} {
				if strings.Contains(screen, bad) {
					t.Errorf("把未知编成了 0：%q\n%s", bad, screen)
				}
			}
		})
	}

	a := newTestApp(120, 36)
	a.sid = "session-down"
	a.handleConnectionEvent(connectionEvent{kind: wsOpen, sid: a.sid, subscription: 1, generation: 1})
	a.handleConnectionEvent(connectionEvent{kind: wsDown, sid: a.sid, subscription: 1, generation: 1})
	statusActionOpen(t, a, "会话 session-", true)
	if screen := strings.Join(a.view(), "\n"); !strings.Contains(screen, "连接状态: 已断开，正在重连") {
		t.Fatalf("真实断线状态没进弹层：\n%s", screen)
	}
}

func TestStatusPopupSwitchSessionClosesAndCannotLeak(t *testing.T) {
	results := map[string]string{
		"session-A": `{"effort":"high","model":"model-A","contextTokens":111,"contextWindow":1000}`,
		"session-B": `{"effort":"low","model":"model-B","contextTokens":222,"contextWindow":2000}`,
	}
	srv := effortHTTPServer(t, func(sid string) string { return results[sid] })
	defer srv.Close()
	cl := &client{base: srv.URL, hc: srv.Client()}
	cl.dialSubscription = func(string, int) (subscriptionConn, error) {
		return newFakeSubscriptionConn(), nil
	}
	a := newTestApp(120, 36)
	a.cl = cl
	defer func() {
		if a.closeSubscription != nil {
			a.closeSubscription()
		}
	}()
	if err := a.switchTo(sessionRow{ID: "session-A", Title: "A"}); err != nil {
		t.Fatal(err)
	}
	statusActionOpen(t, a, "model-A", false)
	if !strings.Contains(strings.Join(a.view(), "\n"), "session id: session-A") {
		t.Fatal("前置：A 的详情没打开")
	}
	if err := a.switchTo(sessionRow{ID: "session-B", Title: "B"}); err != nil {
		t.Fatal(err)
	}
	screen := strings.Join(a.view(), "\n")
	if strings.Contains(screen, "状态详情") || strings.Contains(screen, "session id: session-A") {
		t.Fatalf("切会话后旧弹层串入 B：\n%s", screen)
	}
	statusActionOpen(t, a, "model-B", false)
	screen = strings.Join(a.view(), "\n")
	if !strings.Contains(screen, "session id: session-B") || !strings.Contains(screen, "model: model-B") ||
		strings.Contains(screen, "session id: session-A") {
		t.Fatalf("B 的新详情不正确：\n%s", screen)
	}
}

func TestStatusPopupPendingSendEscKeepsDraftAndBusyWorks(t *testing.T) {
	a := statusActionApp(120, 36)
	a.input = []rune("发送中的草稿")
	a.pendingSend = &pendingPrompt{id: 1, sid: a.sid, text: "发送中的草稿"}
	a.pendingEcho = "发送中的草稿"
	statusActionOpen(t, a, "model-A", false)
	driveStatusActionBytes(a, "\x1b")
	if strings.Contains(strings.Join(a.view(), "\n"), "状态详情") {
		t.Fatal("发送中 Esc 没关弹层")
	}
	if a.pendingSend == nil || string(a.input) != "发送中的草稿" {
		t.Fatalf("发送中关弹层破坏了只读草稿：pending=%+v input=%q", a.pendingSend, string(a.input))
	}

	a.handlePromptResult(promptOutcome{id: 1, sid: a.sid, text: "发送中的草稿", ack: promptAck{Accepted: true, TurnID: "turn-live"}})
	if !a.busy {
		t.Fatal("前置：accepted 后应进入真实 busy")
	}
	statusActionOpen(t, a, "high effort", false)
	if screen := strings.Join(a.view(), "\n"); !strings.Contains(screen, "状态详情 · effort") || !strings.Contains(screen, "effort: high") {
		t.Fatalf("运行中不能查看真实 effort：\n%s", screen)
	}
	a.handleFrame(ServerFrame{Type: "turn/done", Data: []byte(`{"turnId":"turn-live"}`)})
	if a.busy {
		t.Fatal("turn/done 后 busy 没结束")
	}
}

func TestStatusPopupApprovalScrollAndOSCStayIndependent(t *testing.T) {
	a := statusActionApp(120, 36)
	a.input = []rune("safe")
	statusActionOpen(t, a, "model-A", false)
	answered := ""
	a.approve = func(reqID, option string) error {
		answered = reqID + ":" + option
		return nil
	}
	a.handleFrame(ServerFrame{Type: "approval/request", Data: []byte(`{"requestId":"req-status","toolCall":{"title":"bash · ls"}}`)})
	a.handleRune('y')
	if answered != "req-status:allow_once" || string(a.input) != "safe" {
		t.Fatalf("弹层抢走审批输入：answered=%q input=%q", answered, string(a.input))
	}
	driveStatusActionBytes(a, "\x1b")
	if strings.Contains(strings.Join(a.view(), "\n"), "状态详情") {
		t.Fatal("审批后的 Esc 没先关闭弹层")
	}

	fillLines(a, 50)
	statusActionOpen(t, a, "model-A", false)
	driveStatusActionBytes(a, "\x1b[5~")
	if a.scroll == 0 {
		t.Fatal("弹层打开时 PgUp 被吞")
	}
	driveStatusActionBytes(a, "\x1b")
	fillLines(a, 0)

	// OSC 响应与点击共用 stdin：OSC 必须被吃掉，后续点击仍须完整命中。
	a = statusActionApp(120, 36)
	a.input = []rune("safe")
	x := statusActionX(t, a, "model-A", false)
	driveStatusActionBytes(a, fmt.Sprintf("\x1b]11;rgb:0102/0304/0506\x07\x1b[<0;%d;%dM", x, a.scr.height-1))
	if !strings.Contains(strings.Join(a.view(), "\n"), "状态详情 · model") || string(a.input) != "safe" {
		t.Fatalf("OSC 后点击或草稿隔离失败：input=%q\n%s", string(a.input), strings.Join(a.view(), "\n"))
	}
	driveStatusActionBytes(a, fmt.Sprintf("\x1b[<65;%d;%dM", x, a.scr.height-1))
	if !strings.Contains(strings.Join(a.view(), "\n"), "状态详情 · model") {
		t.Fatal("滚轮事件被误当成再次点击")
	}
}

func TestStatusHoverRealBytesPeekPinAndLeave(t *testing.T) {
	a := statusActionApp(120, 36)
	a.input = []rune("draft")
	x, y := statusActionX(t, a, "model-A", false), a.scr.height-1
	motion := func(nx, ny int) {
		driveStatusActionBytes(a, fmt.Sprintf("\x1b[<35;%d;%dM", nx, ny))
	}
	motion(x, y)
	if !strings.Contains(strings.Join(a.view(), "\n"), "状态详情 · model") {
		t.Fatal("SGR motion hover 没打开真实详情")
	}
	motion(1, 1)
	if strings.Contains(strings.Join(a.view(), "\n"), "状态详情") {
		t.Fatal("指针离开后 hover 详情没关闭")
	}

	motion(x, y)
	statusActionClick(a, x, y) // hover 后第一次点击 = 固定，不是立刻关掉
	motion(1, 1)
	if !strings.Contains(strings.Join(a.view(), "\n"), "状态详情 · model") {
		t.Fatal("固定后的详情被 pointer leave 关掉")
	}
	statusActionClick(a, x, y)
	if strings.Contains(strings.Join(a.view(), "\n"), "状态详情") {
		t.Fatal("固定后再次点击没有关闭")
	}
	if got := string(a.input); got != "draft" {
		t.Fatalf("hover/motion 字节漏进草稿：%q", got)
	}
}

func TestStatusMouseAnyMotionModeIsEnabledAndRestored(t *testing.T) {
	var out strings.Builder
	s := newScreenTo(&out, 120, 36)
	s.enterAlt()
	if !strings.Contains(out.String(), "\x1b[?1003h") {
		t.Fatalf("hover 需要 any-motion 1003，进入字节=%q", out.String())
	}
	out.Reset()
	s.Leave()
	if !strings.Contains(out.String(), "\x1b[?1003l") {
		t.Fatalf("退出没恢复 any-motion 1003：%q", out.String())
	}
}

func TestStatusPopupSmallTerminalsNeverOverflow(t *testing.T) {
	for _, size := range [][2]int{{120, 36}, {80, 24}, {40, 10}, {20, 6}, {10, 3}, {8, 2}, {1, 1}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			a := statusActionApp(size[0], size[1])
			before := strings.Join(a.view(), "\n")
			if size[0] >= 2 && size[1] >= 2 {
				statusActionClick(a, 2, size[1]-1)
			}
			rows := a.view()
			if len(rows) != size[1] {
				t.Fatalf("弹层后行数=%d，应为 %d", len(rows), size[1])
			}
			for i, row := range rows {
				if got := displayWidth(row); got > size[0] {
					t.Fatalf("第 %d 行宽 %d > %d：%q", i, got, size[0], row)
				}
			}
			after := strings.Join(rows, "\n")
			if size[0] >= 2 && size[1] >= 3 && before == after {
				t.Fatal("有弹层空间时点击却没有任何可见反馈")
			}
			if size[1] < 3 && before != after {
				t.Fatal("不足一行弹层空间时不得盖住状态/输入行")
			}
		})
	}
}

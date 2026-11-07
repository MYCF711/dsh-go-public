package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func ackTestApp(handler http.HandlerFunc) (*app, *httptest.Server) {
	srv := httptest.NewServer(handler)
	a := newTestApp(80, 24)
	a.cl = &client{base: srv.URL, hc: srv.Client()}
	return a, srv
}

func receivePrompt(t *testing.T, a *app) promptOutcome {
	t.Helper()
	select {
	case result := <-a.promptResults:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("等待 session.prompt HTTP 结果超时")
		return promptOutcome{}
	}
}

func settlePrompt(t *testing.T, a *app) []ServerFrame {
	t.Helper()
	return a.handlePromptResult(receivePrompt(t, a))
}

func userLineCount(a *app, text string) int {
	n := 0
	for i, ln := range a.lines {
		if a.meta[i].role == roleUser && strings.Contains(ln, text) {
			n++
		}
	}
	return n
}

func durableUser(seq int, id, text string) ServerFrame {
	raw, _ := json.Marshal(map[string]any{"id": id, "text": text})
	return ServerFrame{Seq: seq, Type: "user/message", Data: raw}
}

// HTTP accepted 只确认宿主收下；正文必须等 durable user/message，不能由 submit 伪造。
func TestSubmitAcceptedWaitsForDurableUser(t *testing.T) {
	a, srv := ackTestApp(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"accepted":true,"turnId":"turn-1"}}`))
	})
	defer srv.Close()
	a.input = []rune("只显示一次")
	a.submit()
	if got := userLineCount(a, "只显示一次"); got != 0 {
		t.Fatalf("accepted 前就出现了 %d 条用户正文", got)
	}
	if got := string(a.input); got != "只显示一次" {
		t.Fatalf("accepted 前草稿变成 %q", got)
	}
	settlePrompt(t, a)
	if len(a.input) != 0 {
		t.Fatalf("accepted 后草稿没清：%q", string(a.input))
	}
	if got := userLineCount(a, "只显示一次"); got != 0 {
		t.Fatalf("没有 durable user/message 就出现了 %d 条用户正文", got)
	}
	frame := durableUser(1, "m1", "只显示一次")
	a.handleFrame(frame)
	a.handleFrame(frame)
	if got := userLineCount(a, "只显示一次"); got != 1 {
		t.Fatalf("durable 正文应为 1 条，实际 %d", got)
	}
}

// WS 可能先于延迟的 HTTP accepted 到达：先暂存，确认后落定且只落定一次。
func TestDelayedAcceptedBuffersEarlyDurableUser(t *testing.T) {
	release := make(chan struct{})
	a, srv := ackTestApp(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"accepted":true,"turnId":"turn-1"}}`))
	})
	defer srv.Close()
	a.input = []rune("等确认再上屏")
	a.submit()
	frame := durableUser(1, "m-early", "等确认再上屏")
	a.handleFrame(frame)
	a.handleFrame(frame) // 同一 durable 在确认前重放也不能重复暂存
	if got := userLineCount(a, "等确认再上屏"); got != 0 {
		t.Fatalf("延迟 accepted 前出现 %d 条用户正文", got)
	}
	if !strings.Contains(strings.Join(a.view(), "\n"), "草稿只读") {
		t.Fatal("pending 状态没有说明输入只读")
	}
	close(release)
	settlePrompt(t, a)
	if len(a.input) != 0 {
		t.Fatalf("accepted 后没有清草稿：%q", string(a.input))
	}
	a.handleFrame(frame) // accepted 后迟到重放
	if got := userLineCount(a, "等确认再上屏"); got != 1 {
		t.Fatalf("确认前后重放后正文应为 1 条，实际 %d：%v", got, a.lines)
	}
}

// 发送被拒绝时草稿必须原样保留，且不能留下一个看似已提交的用户气泡。
func TestSubmitFailureKeepsDraftWithoutPhantom(t *testing.T) {
	a, srv := ackTestApp(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"busy","message":"上一轮还没结束","hint":"稍后重试","retryable":true}}`))
	})
	defer srv.Close()
	a.push("既有历史")
	a.input = []rune("失败也要保留")
	a.submit()
	settlePrompt(t, a)
	if got := string(a.input); got != "失败也要保留" {
		t.Fatalf("失败后草稿变成 %q", got)
	}
	if got := userLineCount(a, "失败也要保留"); got != 0 {
		t.Fatalf("失败后留下 %d 条幻影用户正文", got)
	}
	if len(a.lines) != 1 || a.lines[0] != "既有历史" {
		t.Fatalf("失败回滚或改写了既有历史：%v", a.lines)
	}
	if !strings.Contains(a.err, "草稿已保留") || !strings.Contains(a.err, "上一轮还没结束") {
		t.Fatalf("发送失败错误不可见或缺草稿提示：%q", a.err)
	}
}

func TestSubmitAcceptedFalseIsFailure(t *testing.T) {
	a, srv := ackTestApp(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"accepted":false,"turnId":"turn-fake"}}`))
	})
	defer srv.Close()
	a.input = []rune("没有 accepted")
	a.submit()
	settlePrompt(t, a)
	if string(a.input) != "没有 accepted" || a.busy {
		t.Fatalf("accepted=false 被当成功：input=%q busy=%v", string(a.input), a.busy)
	}
	if userLineCount(a, "没有 accepted") != 0 || !strings.Contains(a.err, "accepted=false") {
		t.Fatalf("accepted=false 处理错误：lines=%v err=%q", a.lines, a.err)
	}
}

func TestSubmitFailureCanRetrySameDraft(t *testing.T) {
	var calls atomic.Int32
	a, srv := ackTestApp(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"busy","message":"稍后再试","hint":"重试","retryable":true}}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":{"accepted":true,"turnId":"turn-retry"}}`))
	})
	defer srv.Close()
	a.input = []rune("同一份草稿重试")
	a.submit()
	settlePrompt(t, a)
	if string(a.input) != "同一份草稿重试" {
		t.Fatal("第一次失败后草稿没保留")
	}
	a.submit() // 不重新打字，直接重试
	settlePrompt(t, a)
	if calls.Load() != 2 || len(a.input) != 0 || a.activeTurnID != "turn-retry" {
		t.Fatalf("重试状态错误：calls=%d input=%q turn=%q", calls.Load(), string(a.input), a.activeTurnID)
	}
	a.handleFrame(durableUser(1, "m-retry", "同一份草稿重试"))
	if got := userLineCount(a, "同一份草稿重试"); got != 1 {
		t.Fatalf("重试后 durable 正文应为 1 条，实际 %d", got)
	}
}

// pending 时所有编辑键都不能改变草稿，查看与退出键仍保留。
func TestPromptPendingInputIsReadOnly(t *testing.T) {
	a := newTestApp(80, 24)
	a.input = []rune("发送中的草稿")
	a.pendingEcho = "发送中的草稿"
	a.pendingSend = &pendingPrompt{id: 1, sid: a.sid, text: "发送中的草稿"}
	for _, r := range []rune{'x', 8, 13, 6} {
		a.handleRune(r)
	}
	a.handleSpecial("esc")
	if got := string(a.input); got != "发送中的草稿" {
		t.Fatalf("pending 时草稿被改成 %q", got)
	}
	if a.searching {
		t.Fatal("pending 时进入搜索会挪用草稿输入行")
	}
	a.handleRune(20)
	if !a.showToolResults {
		t.Fatal("pending 时 Ctrl+T 查看开关不应被锁")
	}
}

// 去重只认 durable 身份，不认正文：重放同一条不重复，同文两条新消息都要显示。
func TestDurableUserDedupesByIdentityNotText(t *testing.T) {
	a := newTestApp(80, 24)
	first := durableUser(1, "m1", "同文")
	a.handleFrame(first)
	a.handleFrame(first)
	a.handleFrame(durableUser(2, "m2", "同文"))
	if got := userLineCount(a, "同文"); got != 2 {
		t.Fatalf("durable 重放/同文新消息处理后应有 2 条，实际 %d：%v", got, a.lines)
	}
}

func TestSameTextTwoSubmissionsAndLateReplay(t *testing.T) {
	a := newTestApp(80, 24)
	var calls atomic.Int32
	a.sendPrompt = func(string, string) (promptAck, error) {
		return promptAck{Accepted: true, TurnID: "turn-current"}, nil
	}
	for round := 1; round <= 2; round++ {
		a.input = []rune("同文发送")
		a.submit()
		settlePrompt(t, a)
		calls.Add(1)
		frame := durableUser(round, "m"+string(rune('0'+round)), "同文发送")
		a.handleFrame(frame)
		a.handleFrame(frame)
		if got := userLineCount(a, "同文发送"); got != round {
			t.Fatalf("第 %d 次同文发送后正文数=%d", round, got)
		}
		a.handleFrame(ServerFrame{Type: "turn/done", Data: json.RawMessage(`{"turnId":"turn-current"}`)})
	}
	a.handleFrame(durableUser(1, "m1", "同文发送"))
	a.handleFrame(durableUser(2, "m2", "同文发送"))
	if got := userLineCount(a, "同文发送"); got != 2 {
		t.Fatalf("迟到重放把同文正文变成 %d 条", got)
	}
}

// 第一次 HTTP 迟到时已经本地判失败并重试；旧 accepted 不得清新草稿或串掉新 turn。
func TestLateAcceptedCannotCrossRetryTurn(t *testing.T) {
	a := newTestApp(80, 24)
	firstRelease := make(chan struct{})
	secondRelease := make(chan struct{})
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	var calls atomic.Int32
	a.sendPrompt = func(string, string) (promptAck, error) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-firstRelease
			return promptAck{Accepted: true, TurnID: "turn-old"}, nil
		}
		close(secondStarted)
		<-secondRelease
		return promptAck{Accepted: true, TurnID: "turn-new"}, nil
	}
	a.input = []rune("重试中的同文")
	a.submit()
	<-firstStarted
	a.handlePromptResult(promptOutcome{id: 1, sid: a.sid, text: "重试中的同文", err: errors.New("本地超时")})
	if string(a.input) != "重试中的同文" {
		t.Fatal("本地失败后草稿未保留")
	}
	a.submit()
	<-secondStarted
	close(firstRelease)
	a.handlePromptResult(receivePrompt(t, a)) // turn-old 的迟到 accepted
	if a.pendingSend == nil || a.pendingSend.id != 2 || string(a.input) != "重试中的同文" {
		t.Fatalf("旧 accepted 污染了重试：pending=%+v input=%q", a.pendingSend, string(a.input))
	}
	a.handleFrame(durableUser(2, "m-new", "重试中的同文"))
	close(secondRelease)
	settlePrompt(t, a)
	if a.activeTurnID != "turn-new" || userLineCount(a, "重试中的同文") != 1 {
		t.Fatalf("重试确认/正文错误：turn=%q lines=%v", a.activeTurnID, a.lines)
	}
	a.handleFrame(ServerFrame{Type: "turn/done", Data: json.RawMessage(`{"turnId":"turn-old"}`)})
	if !a.busy || a.activeTurnID != "turn-new" {
		t.Fatal("旧 turn/done 把新一轮提前结束")
	}
	a.handleFrame(ServerFrame{Type: "turn/done", Data: json.RawMessage(`{"turnId":"turn-new"}`)})
	if a.busy {
		t.Fatal("新 turn/done 没有结束新一轮")
	}
}

func TestResumeFallbackPromptAlsoKeepsDraftOnFailure(t *testing.T) {
	a := newTestApp(80, 24)
	a.pickFrom = []sessionRow{{ID: "session-other"}}
	a.sendPrompt = func(string, string) (promptAck, error) {
		return promptAck{Accepted: false}, nil
	}
	a.input = []rune("不是会话序号")
	a.submit()
	settlePrompt(t, a)
	if a.pickFrom != nil {
		t.Fatal("非序号没有取消会话选择")
	}
	if got := string(a.input); got != "不是会话序号" {
		t.Fatalf("/resume 回退 prompt 失败后草稿变成 %q", got)
	}
	if userLineCount(a, "不是会话序号") != 0 {
		t.Fatal("/resume 回退 prompt 失败留下幻影用户行")
	}
}

func TestSendAckLayoutsStayWithinTerminal(t *testing.T) {
	for _, size := range [][2]int{{120, 36}, {80, 24}} {
		a := newTestApp(size[0], size[1])
		a.input = []rune(strings.Repeat("发送中草稿很长", 30))
		a.pendingEcho = string(a.input)
		a.pendingSend = &pendingPrompt{id: 1, sid: a.sid, text: string(a.input)}
		a.err = strings.Repeat("发送失败但仍保留草稿；", 20)
		view := a.view()
		if len(view) != size[1] {
			t.Fatalf("%dx%d：得到 %d 行", size[0], size[1], len(view))
		}
		for i, ln := range view {
			if got := dispWidth(ln); got > size[0] {
				t.Fatalf("%dx%d 第 %d 行宽 %d：%q", size[0], size[1], i, got, ln)
			}
		}
	}
}

package main

// 官方 `commands.*` / `subagents.*` / `userQuestions.attachWait` 的**宿主侧**
// 真实语义测试。
//
// 与 internal/transport 的分工：那边只测形状与协议翻译，这里测"到底有没有
// 真的动到真实状态"——命令真的执行了、消息真的投进了子会话、等待真的有时钟。
// 任何一条拿不到真实支撑，就必须是响亮的未实现（ErrOfficialNotImplemented），
// 不许静默成功，也不许编一个回执。

import (
	"errors"
	"strings"
	"testing"
	"time"

	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// ---------------------------------------------------------------------------
// commands.*
// ---------------------------------------------------------------------------

// commands.list 必须列出**真实注册过**的命令（不是空数组、不是 TUI 命令表）。
func TestOfficialCommandListIsReal(t *testing.T) {
	f := newAgentAPIFixture(t)
	sessionID := f.newSession("", nil)

	list, err := f.api.CommandsList(sessionID)
	if err != nil {
		t.Fatalf("commands.list: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("宿主没有列出任何命令：命令注册表必须真实可列出")
	}
	names := make([]string, 0, len(list))
	for _, cmd := range list {
		if cmd.Name == "" || strings.HasPrefix(cmd.Name, "/") {
			t.Fatalf("命令名必须是不带斜杠的非空名字：%q", cmd.Name)
		}
		if strings.TrimSpace(cmd.Description) == "" {
			t.Fatalf("命令 %q 没有 description", cmd.Name)
		}
		names = append(names, cmd.Name)
	}
	if !containsString(names, "help") {
		t.Fatalf("命令清单里没有 help（真实注册表至少要有它）：%v", names)
	}
	// 排序口径：官方按 name 升序。
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("命令清单没有按 name 升序：%v", names)
		}
	}
}

// commands.execute 必须真的执行，并回真实的 commandId + 结果。
func TestOfficialCommandExecuteIsReal(t *testing.T) {
	f := newAgentAPIFixture(t)
	sessionID := f.newSession("", nil)

	exec, err := f.api.CommandsExecute(sessionID, "/help", nil)
	if err != nil {
		t.Fatalf("commands.execute /help: %v", err)
	}
	if exec == nil {
		t.Fatal("commands.execute 回了 undefined：/help 是注册过的命令，必须真执行")
	}
	if !strings.HasPrefix(exec.CommandID, "cmd-") {
		t.Fatalf("commandId 形状不对：%q", exec.CommandID)
	}
	if exec.Result.Kind != "success" {
		t.Fatalf("/help 应该是 success，实际 %+v", exec.Result)
	}
	if !strings.Contains(exec.Result.Text, "help") {
		t.Fatalf("/help 的正文没列出命令：%q", exec.Result.Text)
	}
}

// 语法/名字不解析 → 官方 undefined（本仓 nil,nil），**不记账**。
func TestOfficialCommandExecuteUnresolvedIsUndefined(t *testing.T) {
	f := newAgentAPIFixture(t)
	sessionID := f.newSession("", nil)

	for _, line := range []string{"/nope", "help", "/", "/HELP", "/help!", ""} {
		exec, err := f.api.CommandsExecute(sessionID, line, nil)
		if err != nil {
			t.Fatalf("%q 应该回 undefined，实际错误 %v", line, err)
		}
		if exec != nil {
			t.Fatalf("%q 应该回 undefined，实际 %+v", line, exec)
		}
	}
}

// 生命周期必须真的落到 durable 日志：command/run + command/done。
func TestOfficialCommandExecuteAppendsLifecycle(t *testing.T) {
	f := newAgentAPIFixture(t)
	sessionID := f.newSession("", nil)

	exec, err := f.api.CommandsExecute(sessionID, "/help", nil)
	if err != nil || exec == nil {
		t.Fatalf("commands.execute: %v %+v", err, exec)
	}
	events, err := f.core.agentReadEvents(sessionID)
	if err != nil {
		t.Fatalf("读会话事件: %v", err)
	}
	var sawRun, sawDone bool
	for _, ev := range events {
		switch ev.Type {
		case "command/run":
			if !strings.Contains(string(ev.Data), exec.CommandID) {
				t.Fatalf("command/run 没带本次 commandId %q：%s", exec.CommandID, ev.Data)
			}
			sawRun = true
		case "command/done":
			if !strings.Contains(string(ev.Data), exec.CommandID) {
				t.Fatalf("command/done 没带本次 commandId %q：%s", exec.CommandID, ev.Data)
			}
			sawDone = true
		}
	}
	if !sawRun || !sawDone {
		t.Fatalf("生命周期没落盘：run=%v done=%v", sawRun, sawDone)
	}
}

// 命令不接受附件时，带附件要结算成 error **结果**（不是 transport 错误）。
func TestOfficialCommandExecuteRejectsAttachments(t *testing.T) {
	f := newAgentAPIFixture(t)
	sessionID := f.newSession("", nil)

	exec, err := f.api.CommandsExecute(sessionID, "/help", []transport.OfficialSubmittedAttachment{
		{Type: "file", ReceiptID: "r-1"},
	})
	if err != nil {
		t.Fatalf("带附件应该是 error **结果**，不是 transport 错误：%v", err)
	}
	if exec == nil || exec.Result.Kind != "error" {
		t.Fatalf("应该结算成 error 结果，实际 %+v", exec)
	}
	if !strings.Contains(exec.Result.Text, "does not accept attachments") {
		t.Fatalf("错误正文没说明不接受附件：%q", exec.Result.Text)
	}
}

// ---------------------------------------------------------------------------
// subagents.*
// ---------------------------------------------------------------------------

// 没有子会话时 subagents.prompt 必须响亮失败，**绝不**回一个编的 messageId。
func TestOfficialSubagentPromptUnknownChildIsLoud(t *testing.T) {
	f := newAgentAPIFixture(t)
	parentID := f.newSession("", nil)

	receipt, err := f.api.SubagentsPrompt(transport.OfficialSubagentPromptRequest{
		RequestID: "req-1", ParentSessionID: parentID, ChildSessionID: "child-nope",
		Mode: "continuable", Delivery: "queue",
		Content: []transport.OfficialSubagentPromptContent{{Type: "text", Text: "继续"}},
	})
	if err == nil {
		t.Fatalf("没有子会话竟然回了回执：%+v", receipt)
	}
	if receipt.MessageID != "" {
		t.Fatalf("失败时不许带回执：%+v", receipt)
	}
}

// 真实子会话：投递要真的落进子会话并能被父中断。
func TestOfficialSubagentPromptAndInterrupt(t *testing.T) {
	f := newAgentAPIFixture(t)
	parentID := f.newSession("", nil)

	// 真实投递通道：宿主的 promptService.Submit 在测试里换成记录器。
	var delivered []string
	f.core.deps.submit = func(sessionID, text string) (transport.PromptResult, error) {
		delivered = append(delivered, sessionID+":"+text)
		return transport.PromptResult{Accepted: true, TurnID: "turn-1"}, nil
	}
	f.core.deps.interrupt = func(string) error { return nil }
	f.core.children = nil // 让注册表用新 deps（含 submit / interrupt）重建

	child, err := f.core.childRegistry().Spawn("child-1", parentID, f.home)
	if err != nil {
		t.Fatalf("spawn 子会话: %v", err)
	}
	if child.ID != "child-1" {
		t.Fatalf("子会话地址不对：%+v", child)
	}

	receipt, err := f.api.SubagentsPrompt(transport.OfficialSubagentPromptRequest{
		RequestID: "req-1", ParentSessionID: parentID, ChildSessionID: "child-1",
		Mode: "continuable", Delivery: "queue",
		Content: []transport.OfficialSubagentPromptContent{{Type: "text", Text: "继续"}},
	})
	if err != nil {
		t.Fatalf("subagents.prompt: %v", err)
	}
	if !strings.HasPrefix(receipt.MessageID, "msg-") {
		t.Fatalf("messageId 形状不对：%q", receipt.MessageID)
	}
	if got := len(child.Inbox()); got != 1 {
		t.Fatalf("inbox 应该有 1 条，实际 %d", got)
	}

	accepted, err := f.api.SubagentsInterruptByParent("child-1", parentID)
	if err != nil {
		t.Fatalf("interruptByParent: %v", err)
	}
	if !accepted.Accepted {
		t.Fatalf("interrupt 应该被接受：%+v", accepted)
	}
}

// 父 id 不匹配 = 跨会话，403，不是静默成功。
func TestOfficialSubagentCrossSessionIsRejected(t *testing.T) {
	f := newAgentAPIFixture(t)
	parentA := f.newSession("", nil)
	parentB := f.newSession("", nil)

	if _, err := f.core.childRegistry().Spawn("child-x", parentA, f.home); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	_, err := f.api.SubagentsPrompt(transport.OfficialSubagentPromptRequest{
		RequestID: "req-x", ParentSessionID: parentB, ChildSessionID: "child-x",
		Mode: "continuable", Delivery: "queue",
		Content: []transport.OfficialSubagentPromptContent{{Type: "text", Text: "偷看"}},
	})
	if err == nil {
		t.Fatal("跨会话投递竟然成功了")
	}
	if !strings.Contains(err.Error(), "cross_session") && !errors.Is(err, transport.ErrOfficialBadInput) {
		// cross_session 由 officialError 承载，错误文本里带码。
		if !strings.Contains(strings.ToLower(err.Error()), "属于父会话") {
			t.Fatalf("错误没说清是跨会话：%v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// userQuestions.attachWait
// ---------------------------------------------------------------------------

// 问题还开着时，attachWait 必须给出**真实**剩余毫秒（单调递减）。
func TestOfficialQuestionAttachWaitIsReal(t *testing.T) {
	f := newAgentAPIFixture(t)

	sessionID := f.newSession("", func(lg *session.Log) {
		if err := lg.Append("tool/call", session.ToolCallEvent(1, 1, "call-ask", "ask_user_question", `{"questions":[]}`)); err != nil {
			t.Fatalf("追加 tool/call: %v", err)
		}
		if err := lg.Append("tool/result", session.ToolResultEvent(1, 1, "call-ask", nil, false, "", nil)); err != nil {
			t.Fatalf("追加 tool/result: %v", err)
		}
	})

	// 真实时钟 + 可拨动的 now：deadline 从 durable 的 tool/result 帧时间起算，
	// 所以起点必须是**真实**时刻（编一个过去的时间只会让窗口早就过期）。
	now := time.Now()
	f.core.questionWaits = transport.NewOfficialQuestionWaits(transport.OfficialQuestionWaitOptions{
		Timeout: 10 * time.Second,
		Now:     func() time.Time { return now },
	})

	first, err := f.api.UserQuestionsAttachWait(sessionID, "call-ask")
	if err != nil {
		t.Fatalf("attachWait: %v", err)
	}
	if first == nil {
		t.Fatal("问题开着的时候 attachWait 不该是空帧")
	}
	if first.RemainingMs <= 0 || first.RemainingMs > 10_000 {
		t.Fatalf("remainingMs 不在 (0, 10000]：%d", first.RemainingMs)
	}

	now = now.Add(4 * time.Second)
	second, err := f.api.UserQuestionsAttachWait(sessionID, "call-ask")
	if err != nil || second == nil {
		t.Fatalf("第二次 attachWait: %v %+v", err, second)
	}
	if second.RemainingMs >= first.RemainingMs {
		t.Fatalf("剩余时间没有递减：%d → %d", first.RemainingMs, second.RemainingMs)
	}

	// 窗口过去之后：官方不再发帧（L1 = 空帧），不是回一个 0 骗 UI 继续转。
	now = now.Add(200 * time.Second)
	third, err := f.api.UserQuestionsAttachWait(sessionID, "call-ask")
	if err != nil {
		t.Fatalf("超时后 attachWait: %v", err)
	}
	if third != nil {
		t.Fatalf("窗口结束后不该再发帧，实际 %+v", third)
	}
}

// 没有这个 question 通道时必须响亮失败（不是编一个 remainingMs）。
func TestOfficialQuestionAttachWaitUnknownCallIsLoud(t *testing.T) {
	f := newAgentAPIFixture(t)
	sessionID := f.newSession("", nil)

	if _, err := f.api.UserQuestionsAttachWait(sessionID, "call-nope"); err == nil {
		t.Fatal("没有这个 question 调用竟然成功了")
	}
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

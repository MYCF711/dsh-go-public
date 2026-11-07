package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"dshgo/internal/acp"
	"dshgo/internal/agent"
	"dshgo/internal/session"
	"dshgo/internal/spi"
)

// 这一组钉住 ACP `session/load` 的**重放保真**：`session.Surface` 只装
// text + tool_calls（`llm.Message` 没有 reasoning 字段），所以 Load 必须自己回
// 原始事件取思考块（与 fork 同一份 `session.ReasoningBlocksBySeq`），否则断线重连/重开之后：
//
//   - 只有思考的回答会在重放里**整条消失**（正文为空、没有调用，什么也不 emit）；
//   - 有正文的回答丢掉了"它为什么这么干"。
//
// 块顺序与 serve_turn.assistantBlocks / fork.copyNode 一致：思考 → 正文 → 调用。

const acpLoadCwd = "/tmp/acp-load"

// buildACPLoadSession 造一份真实形状的会话：第一条 assistant 是
// 思考 → 正文 → 调用，第二条**只有思考**（正是会整条消失的那种）。
func buildACPLoadSession(t *testing.T, home, id string) {
	t.Helper()
	l, err := session.CreateSession(home, acpLoadCwd, id, "standard")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	must := func(ev string, d any) {
		t.Helper()
		if err := l.Append(ev, d); err != nil {
			t.Fatalf("写 %s 失败: %v", ev, err)
		}
	}
	must("system/message", session.SystemMessageData(1, 1, "你是 dsh-go"))
	must("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: "数文件"}}))
	must("assistant/message", session.AssistantMessageEvent(1, 1, []map[string]any{
		session.ReasoningBlock("先想一下：用户要的是文件数"),
		session.TextBlock("我先数一下"),
		session.ToolCallBlock("c1", "glob", `{"pattern":"*.go"}`),
	}, "tool_calls", nil))
	must("tool/call", session.ToolCallEvent(1, 1, "c1", "glob", `{"pattern":"*.go"}`))
	must("tool/result", session.ToolResultEvent(1, 1, "c1",
		[]spi.Block{{Type: "text", Text: "68 个"}}, false, acpLoadCwd, []int64{2}))
	must("assistant/message", session.AssistantMessageEvent(1, 2, []map[string]any{
		session.ReasoningBlock("用户其实想知道目录大小"),
	}, "stop", nil))
	if err := l.Close(); err != nil {
		t.Fatalf("关会话失败: %v", err)
	}
}

// loadEvents 跑一次 Load，把这个会话的重放事件流原样收下来。
func loadEvents(t *testing.T, home, id string) []agent.Event {
	t.Helper()
	r := newACPRunner(&fakeReg{tools: map[string]spi.Tool{}}, &recorder{},
		"http://127.0.0.1:1", "m", home, 2)
	r.ctxWindow = 0 // 这条测试不关心 usage
	t.Cleanup(func() { _ = r.Shutdown() })

	var got []agent.Event
	if err := r.Load(context.Background(), id, acpLoadCwd, func(e agent.Event) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatalf("Load 出错: %v", err)
	}
	return got
}

// eventFlow 把重放事件压成 "种类|正文" 的一串，方便逐步比对顺序。
func eventFlow(evs []agent.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, string(e.Kind)+"|"+e.Text)
	}
	return out
}

func TestACPLoadReplaysReasoningOnlyAssistant(t *testing.T) {
	home := t.TempDir()
	buildACPLoadSession(t, home, "s-acp-load")

	got := eventFlow(loadEvents(t, home, "s-acp-load"))

	// 期望的就是**日志里模型看到的那段历史**，按 surface 顺序：
	// system、user、assistant(思考→正文→调用)、tool/result、只有思考的 assistant。
	want := []string{
		"assistant/text|你是 dsh-go",
		"assistant/text|数文件",
		"assistant/reasoning|先想一下：用户要的是文件数",
		"assistant/text|我先数一下",
		"tool/call|",
		"assistant/text|68 个",
		"assistant/reasoning|用户其实想知道目录大小",
	}
	if len(got) != len(want) {
		t.Fatalf("重放了 %d 条事件，期望 %d 条：\n got: %s\nwant: %s",
			len(got), len(want), strings.Join(got, "; "), strings.Join(want, "; "))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条重放事件 = %q，期望 %q\n got: %s\nwant: %s",
				i, got[i], want[i], strings.Join(got, "; "), strings.Join(want, "; "))
		}
	}

	// 只有思考的回答必须带思考块回来——以前它整条消失（正文为空、没有调用）。
	last := got[len(got)-1]
	if last != "assistant/reasoning|用户其实想知道目录大小" {
		t.Fatalf("只有思考的回答在 session/load 里丢了：最后一条 = %q", last)
	}
}

// TestACPLoadWithoutReasoningReplaysNoThoughts 是"不许误伤"那条：
// 日志里没有思考块时，重放一条 reasoning 都不许凭空多出来，其余顺序照旧。
func TestACPLoadWithoutReasoningReplaysNoThoughts(t *testing.T) {
	home := t.TempDir()
	l, err := session.CreateSession(home, acpLoadCwd, "s-acp-plain", "standard")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	if err := l.Append("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: "数文件"}})); err != nil {
		t.Fatal(err)
	}
	if err := l.Append("assistant/message", session.AssistantMessageEvent(1, 1, []map[string]any{
		session.TextBlock("一共 68 个"),
	}, "stop", nil)); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	got := loadEvents(t, home, "s-acp-plain")
	if len(got) != 2 {
		t.Fatalf("重放了 %d 条事件，期望 2 条：%v", len(got), eventFlow(got))
	}
	for i, e := range got {
		if e.Kind == agent.EventReasoning {
			t.Fatalf("第 %d 条重放凭空多了思考事件：%q", i, e.Text)
		}
	}
}

// TestACPLoadWireReplaysThoughtChunk 是 A 的**线级**判据：不只问 `Load` 有没有
// emit，而是把真的 acp.Server 接在真的 acpRunner 前面，走一遍 JSON-RPC 的
// `session/load`，看客户端收到的 `session/update` 里有没有 `agent_thought_chunk`。
//
// 为什么还要这一层：`Load` emit 的 `agent.Event` 要经 `updateFor` 投影成 ACP 的
// update 才上线。两层各自有测试，但"只有思考的回答在线级整条消失"这个症状必须
// 在缝合处钉一次，否则谁改了 `updateFor` 也看不出回归。
func TestACPLoadWireReplaysThoughtChunk(t *testing.T) {
	home := t.TempDir()
	buildACPLoadSession(t, home, "s-acp-wire")

	r := newACPRunner(&fakeReg{tools: map[string]spi.Tool{}}, &recorder{},
		"http://127.0.0.1:1", "m", home, 2)
	r.ctxWindow = 0
	t.Cleanup(func() { _ = r.Shutdown() })

	// client → server / server → client 两条管道，与 internal/acp 的测试同一套接法。
	srvIn, cliOut := io.Pipe()
	cliIn, srvOut := io.Pipe()
	srv := &acp.Server{In: srvIn, Out: srvOut, Agent: r, Name: "dsh-go-acp", Version: "test"}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()

	msgs := make(chan map[string]any, 64)
	go func() {
		sc := bufio.NewScanner(cliIn)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err == nil {
				msgs <- m
			}
		}
		close(msgs)
	}()

	req, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1.0, "method": "session/load",
		"params": map[string]any{"sessionId": "s-acp-wire", "cwd": acpLoadCwd},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cliOut.Write(append(req, '\n')); err != nil {
		t.Fatalf("发 session/load 失败: %v", err)
	}

	var thoughts []string
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("等 session/load 应答超时（已收到思考 %d 条）", len(thoughts))
		case m, ok := <-msgs:
			if !ok {
				t.Fatal("连接已关闭")
			}
			if m["method"] == acp.MethodSessionUpdate {
				params, _ := m["params"].(map[string]any)
				update, _ := params["update"].(map[string]any)
				if update["sessionUpdate"] == "agent_thought_chunk" {
					content, _ := update["content"].(map[string]any)
					text, _ := content["text"].(string)
					thoughts = append(thoughts, text)
				}
				continue
			}
			if id, ok := m["id"].(float64); ok && id == 1 {
				if e, ok := m["error"]; ok {
					t.Fatalf("session/load 报错: %v", e)
				}
				want := []string{"先想一下：用户要的是文件数", "用户其实想知道目录大小"}
				if strings.Join(thoughts, "|") != strings.Join(want, "|") {
					t.Fatalf("线级重放的思考块 = %q，期望 %q（只有思考的回答在线级整条消失了）",
						thoughts, want)
				}
				return
			}
			t.Fatalf("收到意外消息: %+v", m)
		}
	}
}

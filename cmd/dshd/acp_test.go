package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"dshgo/internal/agent"
	"dshgo/internal/modes"
	"dshgo/internal/spi"
)

// sseToolCall 起一个假的 OpenAI 兼容端点：第一次让模型请求工具 callTool，
// 之后直接收尾。
//
// 为什么要这个：acpRunner.Prompt 里"把会话模式的审批倾向传给 guardedRunner"
// 只有一行，但传错了（把 deny 写成 allow 或空）在别的测试里看不出来——
// 整条 Loop 链路真跑一遍才看得见。这里不碰真网关，事件流走的是同一条代码。
func sseToolCall(t *testing.T, callTool string) string {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		if atomic.AddInt32(&n, 1) == 1 {
			fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function",`+
					`"function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
				callTool))
		} else {
			fmt.Fprintf(w, "data: %s\n\n",
				`{"choices":[{"delta":{"content":"好"},"finish_reason":"stop"}]}`)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// newPromptHarness 造一个只依赖假网关的 acpRunner：工具表里只有一个写类工具 w，
// 内层执行器和审批回调都只记账（不真跑、不真问）。
func newPromptHarness(t *testing.T, base, modeID string, modeReg *modes.Registry) (*acpRunner, *recorder, *int) {
	t.Helper()
	reg := &fakeReg{tools: map[string]spi.Tool{
		"w": &fakeTool{name: "w", caps: spi.Caps{Writes: true}},
	}}
	inner := &recorder{}
	asked := 0
	r := newACPRunner(reg, inner, base, "m", t.TempDir(), 2)
	// 会话日志是一个真打开着的文件：不关掉，Windows 上 t.TempDir 的清理删不掉它
	// （注册在 TempDir 之后 → 先执行）。
	t.Cleanup(func() { _ = r.Shutdown() })
	r.ctxWindow = 0 // 这条测试不关心 usage
	r.modeReg = modeReg
	r.modeID = modeID
	r.ask = func(context.Context, string, string, string) (bool, error) {
		asked++
		return true, nil
	}
	return r, inner, &asked
}

// promptOnce 跑一轮，把这个会话的事件流收下来。
func promptOnce(t *testing.T, r *acpRunner, sessionID string) []agent.Event {
	t.Helper()
	var evs []agent.Event
	if _, _, err := r.Prompt(context.Background(), sessionID, t.TempDir(), "做点事",
		func(e agent.Event) error {
			evs = append(evs, e)
			return nil
		}); err != nil {
		t.Fatalf("Prompt 出错: %v", err)
	}
	return evs
}

// toolResultOf 取出某个工具调用的结果事件（没有就 nil）。
func toolResultOf(evs []agent.Event, tool string) *agent.Event {
	for i := range evs {
		if evs[i].Kind == agent.EventToolResult && evs[i].ToolName == tool {
			return &evs[i]
		}
	}
	return nil
}

// TestACPPromptPassesModeApproval 钉住 Prompt 里那一行
// `modeApproval: r.modeOf(sessionID).Approval`。
//
// 它看起来只是"把策略传下去"，但传错就是权限事故：
//   - 把 deny 传成 ask/""：只读会话里的写类工具会变成"可以问用户"，
//     而 ACP 客户端那边人可能根本没在看，一次 allow 就落盘；
//   - 把 ask 传成 allow：写类工具连问都不问，直接执行。
//
// 所以三个方向都要钉：传下去的值 == 当前模式里的 Approval，且三种取值的
// 行为各自可区分（deny 不问不跑 / 空要问 / allow 不问就跑）。
func TestACPPromptPassesModeApproval(t *testing.T) {
	builtin, err := modes.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("deny：只读模式不问人、直接拒", func(t *testing.T) {
		r, inner, asked := newPromptHarness(t, sseToolCall(t, "w"), "read-only", builtin)
		evs := promptOnce(t, r, "s-deny")

		if inner.called != 0 {
			t.Fatalf("只读模式下的写类工具被放行了 %d 次——modeApproval 没传成 deny", inner.called)
		}
		if *asked != 0 {
			t.Fatalf("deny 模式不该问人，实际问了 %d 次", *asked)
		}
		ev := toolResultOf(evs, "w")
		if ev == nil || ev.Result == nil {
			t.Fatalf("应当有一条 w 的工具结果事件：%+v", evs)
		}
		// 必须说成"策略拒"，不是"用户拒"：两者对模型的下一步要求不同。
		if !strings.Contains(ev.Result.Summary, "策略拒绝") {
			t.Fatalf("拒绝理由应当是策略（不是用户）：%q", ev.Result.Summary)
		}
		if !strings.Contains(ev.Result.Hint, "deny") {
			t.Fatalf("Hint 应当点出当前模式是 deny：%q", ev.Result.Hint)
		}
	})

	t.Run("空：默认模式要问，同意后才跑", func(t *testing.T) {
		r, inner, asked := newPromptHarness(t, sseToolCall(t, "w"), modes.DefaultID, builtin)
		promptOnce(t, r, "s-ask")

		if *asked != 1 {
			t.Fatalf("默认模式的审批倾向是空（沿用宿主），必须问一次，实际 %d 次", *asked)
		}
		if inner.called != 1 {
			t.Fatalf("同意之后应当执行一次，实际 %d 次", inner.called)
		}
	})

	t.Run("allow：自建模式不问就放行", func(t *testing.T) {
		via, err := modes.NewRegistry([]modes.Mode{{ID: "auto-allow", Approval: "allow"}})
		if err != nil {
			t.Fatal(err)
		}
		r, inner, asked := newPromptHarness(t, sseToolCall(t, "w"), "auto-allow", via)
		promptOnce(t, r, "s-allow")

		if *asked != 0 {
			t.Fatalf("allow 模式不该问人，实际问了 %d 次", *asked)
		}
		if inner.called != 1 {
			t.Fatalf("allow 模式应当直接执行一次，实际 %d 次", inner.called)
		}
	})
}

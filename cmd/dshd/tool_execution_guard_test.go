package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"dshgo/internal/agent"
	"dshgo/internal/modes"
	"dshgo/internal/spi"
	"dshgo/modules/tool/workflow"
)

// guardSpyTool 记录真正落到 Tool.Exec 的次数。拒绝路径只检查 error 文本不够：
// 旧实现正是返回成功并执行了当前模式没有声明的工具。
type guardSpyTool struct {
	name     string
	caps     spi.Caps
	delegate spi.Tool
	execs    int
}

func (t *guardSpyTool) Name() string            { return t.name }
func (t *guardSpyTool) Description() string     { return "工具准入测试" }
func (t *guardSpyTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *guardSpyTool) Caps() spi.Caps          { return t.caps }
func (t *guardSpyTool) Exec(ctx context.Context, args json.RawMessage) (spi.Result, error) {
	t.execs++
	if t.delegate != nil {
		return t.delegate.Exec(ctx, args)
	}
	return spi.Result{Summary: t.name + "-ok", Data: map[string]any{"tool": t.name}}, nil
}

// guardExecRecorder 是最内层执行器：先记录 Runner 入口，再真实调用 Tool.Exec。
// 对拒绝用例，calls 和 Tool.Exec 都必须保持为零。
type guardExecRecorder struct {
	reg   spi.ToolRegistry
	calls []string
}

func (r *guardExecRecorder) Run(ctx context.Context, name string, args json.RawMessage) (spi.Result, error) {
	r.calls = append(r.calls, name)
	tool, ok := r.reg.Get(name)
	if !ok {
		return spi.Result{Hint: "使用已注册工具"}, fmt.Errorf("unknown tool %q", name)
	}
	return tool.Exec(ctx, args)
}

type guardPromptFixture struct {
	runner   *acpRunner
	inner    *guardExecRecorder
	visible  *guardSpyTool
	hidden   *guardSpyTool
	workflow *guardSpyTool
	asked    int

	mu       sync.Mutex
	declared []string
}

func (f *guardPromptFixture) declaredTools() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.declared...)
}

func newGuardPromptFixture(t *testing.T, callTool string, callArgs json.RawMessage, withAsk bool) *guardPromptFixture {
	t.Helper()
	f := &guardPromptFixture{
		visible:  &guardSpyTool{name: "visible", caps: spi.Caps{Writes: true}},
		hidden:   &guardSpyTool{name: "hidden", caps: spi.Caps{Writes: true}},
		workflow: &guardSpyTool{name: "workflow", delegate: &workflow.Tool{}},
	}
	reg := &fakeReg{tools: map[string]spi.Tool{
		"visible":  f.visible,
		"hidden":   f.hidden,
		"workflow": f.workflow,
	}}
	f.inner = &guardExecRecorder{reg: reg}

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if atomic.LoadInt32(&requests) == 0 {
			var body struct {
				Tools []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tools"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Errorf("解码模型请求失败：%v", err)
			}
			f.mu.Lock()
			for _, tool := range body.Tools {
				f.declared = append(f.declared, tool.Function.Name)
			}
			f.mu.Unlock()
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		var payload []byte
		if atomic.AddInt32(&requests, 1) == 1 {
			payload, _ = json.Marshal(map[string]any{
				"choices": []any{map[string]any{
					"delta": map[string]any{"tool_calls": []any{map[string]any{
						"index": 0, "id": "guard-call-1", "type": "function",
						"function": map[string]any{"name": callTool, "arguments": string(callArgs)},
					}}},
					"finish_reason": "tool_calls",
				}},
			})
		} else {
			payload = []byte(`{"choices":[{"delta":{"content":"完成"},"finish_reason":"stop"}]}`)
		}
		fmt.Fprintf(w, "data: %s\n\n", payload)
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)

	modeReg, err := modes.NewRegistry([]modes.Mode{{
		ID: "guard-restricted", Tools: []string{"visible", "workflow"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.runner = newACPRunner(reg, f.inner, srv.URL, "m", t.TempDir(), 2)
	t.Cleanup(func() { _ = f.runner.Shutdown() })
	f.runner.ctxWindow = 0
	f.runner.modeReg = modeReg
	f.runner.modeID = "guard-restricted"
	if withAsk {
		f.runner.ask = func(context.Context, string, string, string) (bool, error) {
			f.asked++
			return true, nil
		}
	}
	return f
}

func (f *guardPromptFixture) prompt(t *testing.T, sessionID string) []agent.Event {
	t.Helper()
	return promptOnce(t, f.runner, sessionID)
}

func guardWorkflowArgs(t *testing.T, child string) json.RawMessage {
	t.Helper()
	script, err := json.Marshal([]map[string]any{{"tool": child, "args": map[string]any{}}})
	if err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal(map[string]any{"script": string(script)})
	if err != nil {
		t.Fatal(err)
	}
	return args
}

func guardFailedSteps(t *testing.T, evs []agent.Event, tool string) int {
	t.Helper()
	ev := toolResultOf(evs, tool)
	if ev == nil || ev.Result == nil {
		t.Fatalf("缺少 %s 的工具结果：%+v", tool, evs)
	}
	data, ok := ev.Result.Data.(map[string]any)
	if !ok {
		t.Fatalf("workflow 结果缺少结构化 Data：%+v", ev.Result)
	}
	failed, ok := data["failed"].(int)
	if !ok {
		t.Fatalf("workflow 结果缺少 failed：%+v", data)
	}
	return failed
}

func TestACPToolExecutionGuard(t *testing.T) {
	t.Run("隐藏工具直接调用拒绝且 Exec 为零", func(t *testing.T) {
		f := newGuardPromptFixture(t, "hidden", json.RawMessage(`{}`), true)
		evs := f.prompt(t, "guard-hidden-direct")

		declared := f.declaredTools()
		if !reflect.DeepEqual(declared, []string{"visible", "workflow"}) && !reflect.DeepEqual(declared, []string{"workflow", "visible"}) {
			t.Fatalf("模型工具表应只含当前视图，实际 %v", declared)
		}
		if f.hidden.execs != 0 || len(f.inner.calls) != 0 {
			t.Fatalf("隐藏工具落到底层：Runner calls=%v, hidden Exec=%d", f.inner.calls, f.hidden.execs)
		}
		if f.asked != 0 {
			t.Fatalf("隐藏工具应在审批前拒绝，实际询问 %d 次", f.asked)
		}
		ev := toolResultOf(evs, "hidden")
		if ev == nil || !ev.IsError {
			t.Fatalf("隐藏工具必须产生拒绝结果：%+v", evs)
		}
	})

	t.Run("workflow 嵌套隐藏工具拒绝且 Exec 为零", func(t *testing.T) {
		f := newGuardPromptFixture(t, "workflow", guardWorkflowArgs(t, "hidden"), true)
		evs := f.prompt(t, "guard-hidden-nested")

		if f.workflow.execs != 1 {
			t.Fatalf("允许的 workflow 应执行一次，实际 %d", f.workflow.execs)
		}
		if f.hidden.execs != 0 || !reflect.DeepEqual(f.inner.calls, []string{"workflow"}) {
			t.Fatalf("嵌套隐藏工具落到底层：Runner calls=%v, hidden Exec=%d", f.inner.calls, f.hidden.execs)
		}
		if f.asked != 0 {
			t.Fatalf("嵌套隐藏工具不应进入审批，实际询问 %d 次", f.asked)
		}
		if got := guardFailedSteps(t, evs, "workflow"); got != 1 {
			t.Fatalf("workflow 应记录一个被拒绝的子步骤，实际 failed=%d", got)
		}
	})

	t.Run("允许工具正常执行且结果不变", func(t *testing.T) {
		for _, withAsk := range []bool{true, false} {
			t.Run(fmt.Sprintf("ask=%t", withAsk), func(t *testing.T) {
				f := newGuardPromptFixture(t, "visible", json.RawMessage(`{}`), withAsk)
				evs := f.prompt(t, fmt.Sprintf("guard-visible-%t", withAsk))

				if f.visible.execs != 1 || !reflect.DeepEqual(f.inner.calls, []string{"visible"}) {
					t.Fatalf("允许工具应正常执行一次：Runner calls=%v, visible Exec=%d", f.inner.calls, f.visible.execs)
				}
				wantAsked := 0
				if withAsk {
					wantAsked = 1
				}
				if f.asked != wantAsked {
					t.Fatalf("审批语义改变：询问=%d，期望 %d", f.asked, wantAsked)
				}
				ev := toolResultOf(evs, "visible")
				if ev == nil || ev.IsError || ev.Result == nil || ev.Result.Summary != "visible-ok" {
					t.Fatalf("允许工具的结果改变：%+v", ev)
				}
			})
		}
	})

	t.Run("ask nil 同样拒绝隐藏工具", func(t *testing.T) {
		f := newGuardPromptFixture(t, "hidden", json.RawMessage(`{}`), false)
		evs := f.prompt(t, "guard-hidden-no-ask")

		if f.runner.ask != nil {
			t.Fatal("这个用例必须覆盖 ask=nil 路径")
		}
		if f.hidden.execs != 0 || len(f.inner.calls) != 0 {
			t.Fatalf("ask=nil 绕过准入：Runner calls=%v, hidden Exec=%d", f.inner.calls, f.hidden.execs)
		}
		ev := toolResultOf(evs, "hidden")
		if ev == nil || !ev.IsError {
			t.Fatalf("ask=nil 时隐藏工具必须拒绝：%+v", evs)
		}
	})

	t.Run("未知工具不进入底层执行器", func(t *testing.T) {
		f := newGuardPromptFixture(t, "not_registered", json.RawMessage(`{}`), true)
		evs := f.prompt(t, "guard-unknown")

		if len(f.inner.calls) != 0 {
			t.Fatalf("未知工具进入底层 Runner：%v", f.inner.calls)
		}
		if f.visible.execs+f.hidden.execs+f.workflow.execs != 0 {
			t.Fatalf("未知工具触发了其他工具 Exec：visible=%d hidden=%d workflow=%d",
				f.visible.execs, f.hidden.execs, f.workflow.execs)
		}
		ev := toolResultOf(evs, "not_registered")
		if ev == nil || !ev.IsError {
			t.Fatalf("未知工具必须拒绝：%+v", evs)
		}
	})

	t.Run("workflow 嵌套允许工具保留子分派与审批", func(t *testing.T) {
		f := newGuardPromptFixture(t, "workflow", guardWorkflowArgs(t, "visible"), true)
		evs := f.prompt(t, "guard-visible-nested")

		if !reflect.DeepEqual(f.inner.calls, []string{"workflow", "visible"}) {
			t.Fatalf("允许的嵌套调用链改变：%v", f.inner.calls)
		}
		if f.workflow.execs != 1 || f.visible.execs != 1 {
			t.Fatalf("允许的嵌套工具应各执行一次：workflow=%d visible=%d", f.workflow.execs, f.visible.execs)
		}
		if f.asked != 1 {
			t.Fatalf("嵌套写工具仍应审批一次，实际 %d", f.asked)
		}
		if got := guardFailedSteps(t, evs, "workflow"); got != 0 {
			t.Fatalf("允许的嵌套步骤不应失败，failed=%d", got)
		}
	})
}

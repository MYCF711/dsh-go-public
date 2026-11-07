package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dshgo/internal/spi"
)

// 本文件钉住官方 #78 `session-checkpoint-policy` 的 **tools/execute 前钩子**（任务 B）。
//
// 官方原件（0.2.2-rc.1，asar 只读解出，dsh-session-checkpoint-policy/lib/index.js）：
//
//	:61-66  ctx.on("tools/execute", async (exec, next) => {
//	           if (exec.agent === void 0 || exec.parent !== void 0) return next();
//	           await ctx.sessions.flush(exec.agent.session);
//	           if (exec.signal.aborted) return abortedBeforeDispatchResult();
//	           return next();
//	        });
//	:75-84  function abortedBeforeDispatchResult() {
//	           return { content: [...], isError: true,
//	                    error: { message: "tool call aborted before dispatch",
//	                             info: { name: "AbortError",
//	                                     code: TOOL_ABORTED_BEFORE_DISPATCH } } };
//	        }
//
// 判据只有三条，缺一条这个钩子就等于没接：
//   - flush 失败 ⇒ 工具体**零执行**（fail-closed）；
//   - 已 abort ⇒ 返回官方 code，工具体**零执行**；
//   - 顶层才 flush：无会话（agent === void 0）与嵌套（parent !== void 0）都放行，不落盘。

// TestCheckpointBeforeDispatchFailsClosed 钉「flush 失败 ⇒ 工具体零执行」。
func TestCheckpointBeforeDispatchFailsClosed(t *testing.T) {
	ran := false
	tl := &stubTool{name: "x", ran: &ran, res: spi.Result{Summary: "不该跑到这里"}}
	sentinel := errors.New("durability checkpoint: 侧车根目录被占用")
	var calls int
	p, k := newPipe(t, Options{
		BeforeDispatch: func(context.Context, *spi.Call) error {
			calls++
			return sentinel
		},
	}, tl)
	defer k.Root().Dispose()

	res, err := p.Run(spi.WithSession(context.Background(), "s-1"), "x", nil)
	if err == nil {
		t.Fatal("派发前落盘失败必须报错（fail-closed）")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("错误没有保留落盘失败的原因：%v", err)
	}
	if ran {
		t.Fatal("落盘失败仍执行了工具体（零执行断言不成立）")
	}
	if calls != 1 {
		t.Fatalf("派发前落盘应只调用 1 次，实际 %d", calls)
	}
	if res.Hint == "" {
		t.Fatal("失败必须带 Hint（R3）")
	}
}

// TestCheckpointAbortedBeforeDispatch 钉官方 aborted 分支：
// flush 成功之后发现这一轮已被取消 ⇒ 返回 TOOL_ABORTED_BEFORE_DISPATCH，
// 工具体零执行，且 errors.Is(context.Canceled) 仍然成立（取消语义不许被吞掉）。
func TestCheckpointAbortedBeforeDispatch(t *testing.T) {
	ran := false
	tl := &stubTool{name: "x", ran: &ran, res: spi.Result{Summary: "不该跑到这里"}}
	ctx, cancel := context.WithCancel(spi.WithSession(context.Background(), "s-1"))
	cancel()
	var calls int
	p, k := newPipe(t, Options{
		BeforeDispatch: func(context.Context, *spi.Call) error {
			calls++
			return nil
		},
	}, tl)
	defer k.Root().Dispose()

	res, err := p.Run(ctx, "x", nil)
	if err == nil {
		t.Fatal("已 abort 的调用必须报错")
	}
	if !errors.Is(err, ErrToolAbortedBeforeDispatch) {
		t.Fatalf("错误不是 ErrToolAbortedBeforeDispatch：%v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消语义被吞掉了：%v", err)
	}
	if calls != 1 {
		t.Fatalf("aborted 判定之前必须先落盘（官方顺序：flush 再判 aborted），实际调用 %d 次", calls)
	}
	if ran {
		t.Fatal("aborted 之后仍执行了工具体（零执行断言不成立）")
	}
	code, _ := res.Data.(map[string]any)["code"].(string)
	if code != ToolAbortedBeforeDispatch {
		t.Fatalf("结果 code = %q，期望官方值 %q（data=%v）", code, ToolAbortedBeforeDispatch, res.Data)
	}
	if res.Hint == "" {
		t.Fatal("aborted 结果必须带 Hint（R3）")
	}
	if !strings.Contains(res.Text, "aborted before dispatch") {
		t.Fatalf("模型可见正文 = %q，期望包含官方的 aborted before dispatch", res.Text)
	}
}

// TestCheckpointSkipsSessionlessAndNested 钉官方两条放行条件：
// 没有会话（agent === void 0）与嵌套子调用（parent !== void 0）都不落盘。
func TestCheckpointSkipsSessionlessAndNested(t *testing.T) {
	t.Run("无会话", func(t *testing.T) {
		ran := false
		tl := &stubTool{name: "x", ran: &ran, res: spi.Result{Summary: "ok"}}
		calls := 0
		p, k := newPipe(t, Options{
			BeforeDispatch: func(context.Context, *spi.Call) error {
				calls++
				return errors.New("不该被调用")
			},
		}, tl)
		defer k.Root().Dispose()
		if _, err := p.Run(context.Background(), "x", nil); err != nil {
			t.Fatalf("无会话时不该因 checkpoint 失败：%v", err)
		}
		if !ran || calls != 0 {
			t.Fatalf("无会话时不应落盘（ran=%v calls=%d）", ran, calls)
		}
	})

	t.Run("嵌套子调用", func(t *testing.T) {
		ran := false
		tl := &stubTool{name: "x", ran: &ran, res: spi.Result{Summary: "ok"}}
		calls := 0
		p, k := newPipe(t, Options{
			BeforeDispatch: func(context.Context, *spi.Call) error {
				calls++
				return errors.New("不该被调用")
			},
		}, tl)
		defer k.Root().Dispose()
		ctx := spi.WithSession(context.Background(), "s-1")
		ctx = spi.WithSubDispatch(ctx, spi.SubDispatch{Runner: p, Depth: 1})
		if _, err := p.Run(ctx, "x", nil); err != nil {
			t.Fatalf("嵌套子调用不该因 checkpoint 失败：%v", err)
		}
		if !ran || calls != 0 {
			t.Fatalf("嵌套子调用不应落盘（ran=%v calls=%d）", ran, calls)
		}
	})
}

// TestCheckpointRunsForTopLevelSession 钉正向面：顶层 + 有会话时必须落盘，
// 且落盘发生在工具体**之前**（工具体里能看见快照）。
func TestCheckpointRunsForTopLevelSession(t *testing.T) {
	var order []string
	tl := &execHookTool{name: "x", onExec: func() { order = append(order, "exec") }}
	p, k := newPipe(t, Options{
		BeforeDispatch: func(context.Context, *spi.Call) error {
			order = append(order, "checkpoint")
			return nil
		},
	}, tl)
	defer k.Root().Dispose()
	ctx := spi.WithSession(context.Background(), "s-1")
	if _, err := p.Run(ctx, "x", nil); err != nil {
		t.Fatalf("顶层会话的调用应成功：%v", err)
	}
	if strings.Join(order, "→") != "checkpoint→exec" {
		t.Fatalf("调用顺序 = %v，期望 checkpoint→exec", order)
	}
}

// execHookTool 是只带 Exec 回调的最小工具（顺序断言用）。
type execHookTool struct {
	name   string
	onExec func()
}

func (t *execHookTool) Name() string            { return t.name }
func (t *execHookTool) Description() string     { return "hook" }
func (t *execHookTool) Caps() spi.Caps          { return spi.Caps{} }
func (t *execHookTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *execHookTool) Exec(context.Context, json.RawMessage) (spi.Result, error) {
	t.onExec()
	return spi.Result{Summary: "ok"}, nil
}

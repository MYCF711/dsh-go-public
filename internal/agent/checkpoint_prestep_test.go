package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"dshgo/internal/llm"
	"dshgo/internal/spi"
)

// 本文件钉住官方 #78 `session-checkpoint-policy` 的 **agent/pre-step 钩子**（任务 B）。
//
// 官方原件（0.2.2-rc.1，asar 只读解出，dsh-session-checkpoint-policy/lib/index.js）：
//
//	:67-70  ctx.on("agent/pre-step", async ({ agent }, next) => {
//	           await ctx.sessions.flush(agent.session);
//	           return next();
//	        });
//
// 判据：flush 拒绝 ⇒ 下游（模型请求）根本不进入（fail-closed）；成功 ⇒
// 每一步模型请求之前都跑一次，第一步也不例外。

// TestPreStepFailsClosedBeforeModelRequest 钉 fail-closed：
// pre-step 落盘失败时，**一次模型请求都不许发出去**。
func TestPreStepFailsClosedBeforeModelRequest(t *testing.T) {
	chat := &scriptedChat{steps: []*llm.Response{{Content: "不该被请求"}}}
	sentinel := errors.New("durability checkpoint: 侧车根目录被占用")
	var steps []int
	loop := &Loop{
		Chat: chat,
		PreStep: func(_ context.Context, step int) error {
			steps = append(steps, step)
			return sentinel
		},
	}
	_, err := loop.Run(context.Background(), 1, nil)
	if err == nil {
		t.Fatal("pre-step 失败必须中止本轮（fail-closed）")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("错误没有保留落盘失败的原因：%v", err)
	}
	if len(chat.reqs) != 0 {
		t.Fatalf("pre-step 失败仍派发了 %d 次模型请求（fail-closed 不成立）", len(chat.reqs))
	}
	if len(steps) != 1 || steps[0] != 1 {
		t.Fatalf("PreStep 调用序列 = %v，期望 [1]（第一步就要跑）", steps)
	}
}

// TestPreStepRunsBeforeEveryModelStep 钉「每一步」：
// 两模型步的轮次（先调工具、再收工）必须在 step=1 与 step=2 各跑一次。
func TestPreStepRunsBeforeEveryModelStep(t *testing.T) {
	tool := &fakeTool{name: "t", res: spi.Result{Summary: "ok"}}
	chat := &scriptedChat{steps: []*llm.Response{
		{
			ToolCalls: []llm.ToolCall{{
				Index: 0, ID: "c1", Type: "function",
				Function: llm.Func{Name: "t", Arguments: "{}"},
			}},
			FinishReason: "tool_calls",
		},
		{Content: "收工", FinishReason: "stop"},
	}}
	reg := &fakeRegistry{tools: map[string]spi.Tool{"t": tool}}
	runner := &fakeRunner{reg: reg}
	var steps []int
	loop := &Loop{
		Chat:   chat,
		Tools:  reg,
		Runner: runner,
		PreStep: func(_ context.Context, step int) error {
			steps = append(steps, step)
			return nil
		},
	}
	if _, err := loop.Run(context.Background(), 1, nil); err != nil {
		t.Fatalf("这一轮应当正常收工: %v", err)
	}
	if got := fmt.Sprint(steps); got != "[1 2]" {
		t.Fatalf("PreStep 调用序列 = %s，期望 [1 2]（每个模型步各一次）", got)
	}
	if len(chat.reqs) != 2 {
		t.Fatalf("模型请求数 = %d，期望 2", len(chat.reqs))
	}
	if tool.runs != 1 {
		t.Fatalf("工具执行次数 = %d，期望 1", tool.runs)
	}
}

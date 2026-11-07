// Package subagent 提供 `subagent` 工具：把一件独立的事交给子代理跑，只拿结果。
//
// 省的是上下文：子代理有自己的历史，中间步骤不回到这里。
//
// 与上游的差别（写在 docs/tool-parity.md）：上游**异步**，立刻返回 id、完成后再回传；
// 我们**同步**，一直等到它收尾。异步要有"宿主主动通知客户端"的通道，现阶段的 ACP
// 实现里没有这条通道——与其做半套异步让人查不到状态，不如老实同步。
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "continuable": {"type": "boolean", "description": "true = 留下一条可续问的子会话（父轮结束后可以用 subagents.prompt 续问）；省略 = 不可续问（默认，不写任何额外的东西）"},
    "cwd": {"type": "string", "description": "子代理的初始工作目录；相对路径按当前目录算，省略表示继承"},
    "description": {"type": "string", "description": "3-5 个词的短描述，给人看"},
    "prompt": {"type": "string", "description": "完整、自包含的任务；子代理看不到本会话的历史"}
  },
  "required": ["description", "prompt"],
  "additionalProperties": false
}`)

type Tool struct{ svc spi.SubagentRunner }

func (t *Tool) Name() string            { return "subagent" }
func (t *Tool) Caps() spi.Caps          { return spi.Caps{} }
func (t *Tool) Schema() json.RawMessage { return schema }
func (t *Tool) Description() string {
	return "把一件独立的事交给子代理跑（它有自己的上下文），只拿结果回来。"
}

type args struct {
	Cwd         string `json:"cwd"`
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
	// Continuable 省略 = false：默认不留下可续问的子会话，既有调用方
	// 发给宿主的请求形状一个字节都不变（见 subagent_test.go 的逐字节断言）。
	//
	// 为什么必须从这里透传：`subagents.prompt` 的地址是
	// (parentSessionId, childSessionId) 控制对，而 child 只能由一次**真实
	// spawn** 产生。工具层不透传，模型就永远发起不了可续问委派——那个方法
	// 在真机上只能是 no_such_session。
	Continuable bool `json:"continuable"`
}

type data struct {
	Description string `json:"description"`
	Steps       int    `json:"steps"`
	Bytes       int    `json:"bytes"`
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"description": "…", "prompt": "…"}`}, err
	}
	desc := strings.TrimSpace(a.Description)
	if desc == "" {
		return spi.Result{Hint: "description 不能为空：给 3-5 个词，让人一眼知道派出去干什么"},
			fmt.Errorf("description 为空")
	}
	prompt := strings.TrimSpace(a.Prompt)
	if prompt == "" {
		return spi.Result{Hint: "prompt 不能为空：子代理看不到本会话的历史，任务必须自包含"},
			fmt.Errorf("prompt 为空")
	}
	if t.svc == nil {
		return spi.Result{Hint: "宿主没有装子代理运行器；这件事只能自己做，或用 bash 起一个独立的 dshd"},
			fmt.Errorf("没有子代理服务")
	}

	res, err := t.svc.RunSub(ctx, spi.SubagentRequest{
		Prompt: prompt, Description: desc, Cwd: strings.TrimSpace(a.Cwd),
		// 省略时是 false：老调用方的线上形状不变。
		Continuable: a.Continuable,
	})
	if err != nil {
		return spi.Result{Hint: "子代理没跑成：先看它卡在哪——任务是否自包含、有没有它够不着的文件；也可以自己直接做"},
			err
	}

	return spi.Result{
		Summary: fmt.Sprintf("子代理完成「%s」（%d 步，%d 字%s）",
			desc, res.Steps, len([]rune(res.Text)),
			// 借用授权必须写进人看的这一行：它是一次权限面扩大。
			map[bool]string{true: "；沿用主会话审批记忆", false: ""}[res.BorrowedApprovals]),
		Text: res.Text,
		Data: data{Description: desc, Steps: res.Steps, Bytes: len(res.Text)},
	}, nil
}

// Module 返回装配用的模块描述。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-subagent",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools", "subagent"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			svc, err := kernel.Inject(s, spi.SubagentKey)
			if err != nil {
				return err
			}
			return reg.Register(&Tool{svc: svc})
		},
	}
}

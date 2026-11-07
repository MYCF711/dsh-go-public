// Package subagentfork 提供 `subagent_fork` 工具：派一个**带着本会话上下文**的子代理。
//
// 与 `subagent` 的唯一差别：子代理能看到父会话最近说过的话。
// 语料里这个工具被调了 112 次，参数形状与 `subagent` 完全一致
// （{"description":…, "prompt":…}），所以两者的区别不在异步，而在继承。
//
// 继承的文本由宿主注入：模块自己**不读会话**——spi 不认识 session，
// 这条结构约束不能为了一个工具就破掉。
package subagentfork

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
    "cwd": {"type": "string", "description": "子代理的初始工作目录；相对路径按当前目录算，省略表示继承"},
    "description": {"type": "string", "description": "3-5 个词的短描述，给人看"},
    "prompt": {"type": "string", "description": "任务本身；子代理能看到本会话最近的对话"}
  },
  "required": ["description", "prompt"],
  "additionalProperties": false
}`)

// Tool 实现 spi.Tool。svc 负责真正去跑子轮，inherit 负责把父会话片段取出来。
type Tool struct {
	svc     spi.SubagentRunner
	inherit func(ctx context.Context) string
}

func (t *Tool) Name() string            { return "subagent_fork" }
func (t *Tool) Caps() spi.Caps          { return spi.Caps{} }
func (t *Tool) Schema() json.RawMessage { return schema }
func (t *Tool) Description() string {
	return "派一个能看到本会话上下文的子代理去办一件事；它只回结果，不带回中间步骤。"
}

type args struct {
	Cwd         string `json:"cwd"`
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
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
		return spi.Result{Hint: "prompt 不能为空：说清要子代理做什么、产出放在哪"},
			fmt.Errorf("prompt 为空")
	}

	req := spi.SubagentRequest{Description: desc, Prompt: prompt, Cwd: a.Cwd}
	if t.inherit != nil {
		req.Inherit = t.inherit(ctx)
	}

	res, err := t.svc.RunSub(ctx, req)
	if err != nil {
		return spi.Result{Hint: "子代理失败；看错误原文决定是改任务描述还是先修环境"},
			fmt.Errorf("%w", err)
	}
	return spi.Result{
		Data: data{
			Description: desc, Steps: res.Steps, Bytes: len([]rune(res.Text)),
			BorrowedApprovals: res.BorrowedApprovals,
		},
		Summary: fmt.Sprintf("%s「%s」（%d 步，%d 字%s）",
			inheritNote(req.Inherit), desc, res.Steps, len([]rune(res.Text)),
			borrowNote(res.BorrowedApprovals)),
		Text: res.Text,
	}, nil
}

// inheritNote 说清这一次到底带了多少历史。
//
// 为什么必须说：keep 是宿主定的上限（现在 20 条 ≈ 最近 10 轮）。
// 不说的话，模型会以为"派个 fork 它就知道全部"，然后在长会话里问一个
// 子代理根本看不到的东西，拿到"不知道"还以为是子代理能力不行。
func inheritNote(inherit string) string {
	if strings.TrimSpace(inherit) == "" {
		return "子代理完成（这次没带上历史：会话太短或读不到）"
	}
	n := 0
	for _, line := range strings.Split(inherit, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return fmt.Sprintf("带上下文子代理完成（继承 %d 条历史，更早的看不到）", n)
}

func borrowNote(b bool) string {
	if b {
		return "；沿用主会话审批记忆"
	}
	return ""
}

type data struct {
	Description       string `json:"description"`
	Steps             int    `json:"steps"`
	Bytes             int    `json:"bytes"`
	BorrowedApprovals bool   `json:"borrowed_approvals"`
}

// Module 返回装配用的模块描述。inherit 由接线处提供（读父会话最近若干条并拼成文本）。
func Module(inherit func(ctx context.Context) string) kernel.Module {
	return kernel.Module{
		ID:       "tool-subagentfork",
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
			return reg.Register(&Tool{svc: svc, inherit: inherit})
		},
	}
}

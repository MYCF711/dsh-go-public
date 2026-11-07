// Package workflow 提供 `workflow` 工具：把"一串工具调用"写成一份声明式步骤表，
// 一次往返全部跑完。
//
// 为什么需要它：agent 常见的浪费是"调一次、想一次"。查五个文件的模式、
// 对十个结果逐个 grep——这类活儿的编排是固定的，让模型一步步走，
// 每一步都要一次完整往返（token + 时间都贵）。
//
// 为什么是**声明式 JSON** 而不是脚本语言：
//   - 本仓 R6 明确不嵌 JS 运行时；
//   - 子进程/表达式求值会把"能干什么"从工具契约扩散成任意代码，闸门与审批都难守；
//   - 先证明通道对（子调用照样过审批），语言能力是后面的事。
//
// 子调用必须走 ctx 里的 spi.SubDispatch —— 那是宿主的受管通道。
// 拿不到就直接拒绝，不许自己找后门（拿着裸 Pipeline 编排十次写盘而不问人，
// 正是这个功能最容易出的事故形状）。
package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

// maxTextRunes 是回给模型的正文上限。定 3000 是为了整条链可验证：
// spill 阈值 4000 字符不会被触发 → prune 走快路径 → 模型看到的就是这段。
const maxTextRunes = 3000

// schema 的参数名跟 DSH 对齐：`script` + `meta`。
//
// 但 **script 的语法是本仓自己的**：一段 JSON 步骤表，不是 JS。
// 为什么不实现 JS：本仓 R6 不嵌 JS 运行时，而且表达式求值会把"能干什么"
// 从工具契约扩散成任意代码，闸门与审批都难守。
// 参数名对齐是为了"接进来能调通"，语义差异写在这里和 Description 里，
// 不假装我们跑的是 JS。
var schema = json.RawMessage(`{"type":"object","properties":{"script":{"type":"string","description":"JSON 步骤表：[{\"tool\":\"grep\",\"args\":{…},\"save\":\"hit\"}]；后一步可用 ${hit} 引用前一步结果"},"meta":{"type":"object","description":"可选的元信息，原样回显在结果里，便于调用方对账"}},"required":["script"],"additionalProperties":false}`)

// Tool 是 workflow 工具本身。
type Tool struct{}

func (t *Tool) Name() string { return "workflow" }

func (t *Tool) Caps() spi.Caps { return spi.Caps{} }

func (t *Tool) Schema() json.RawMessage { return schema }

func (t *Tool) Description() string {
	return "把一串工具调用写成步骤表一次跑完：前一步结果可用 ${var} 喂给后一步，省掉来回往返。"
}

type step struct {
	Tool    string         `json:"tool"`
	Args    map[string]any `json:"args"`
	Foreach string         `json:"foreach"`
	Save    string         `json:"save"`
}

type args struct {
	Script string         `json:"script"`
	Meta   map[string]any `json:"meta"`
}

// trace 是一步的结果，**成功失败都记**：模型要的是"哪一步出了什么事"。
type trace struct {
	Step int    `json:"step"`
	Tool string `json:"tool"`
	OK   bool   `json:"ok"`
	// Brief 是一行摘要；MS 是这一步花了多久。
	// 记时长不是为了好看：编排慢的时候，人第一个问题就是"哪一步慢"。
	// stop 记这次子调用要不要停（Result.Stop），不对外暴露。
	stop      bool
	Brief     string         `json:"brief,omitempty"`
	Detail    string         `json:"detail,omitempty"`
	MS        int64          `json:"ms,omitempty"`
	IsError   bool           `json:"isError,omitempty"`
	Text      string         `json:"text,omitempty"`
	Data      any            `json:"data,omitempty"`
	Artifacts []spi.Artifact `json:"artifacts,omitempty"`
	Hint      string         `json:"hint,omitempty"`
	View      *spi.ViewSpec  `json:"view,omitempty"`
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数形状：{"script":"[{\"tool\":\"grep\",\"args\":{…}}]"}`}, err
	}
	var steps []step
	if err := json.Unmarshal([]byte(a.Script), &steps); err != nil {
		return spi.Result{
			Hint: `script 是一段 JSON 步骤表（不是 JS），例如 [{"tool":"glob","args":{"pattern":"*.go"},"save":"f"}]`,
		}, fmt.Errorf("script 不是合法的 JSON 步骤表: %w", err)
	}
	if len(steps) == 0 {
		return spi.Result{Hint: "script 里至少要有一步"}, fmt.Errorf("步骤为空")
	}

	sub := spi.SubDispatchFrom(ctx)
	if sub.Runner == nil {
		return spi.Result{
			Hint: "这个环境没有编排通道（宿主没注入 SubDispatch）；改用普通工具一次一次调",
		}, fmt.Errorf("没有可用的子调用通道")
	}
	if sub.Depth >= spi.MaxSubDepth {
		return spi.Result{
			Hint: fmt.Sprintf("嵌套太深（%d 层）：workflow 里不要再套 workflow，把它拆成两段", sub.Depth),
		}, fmt.Errorf("嵌套超过 %d 层", spi.MaxSubDepth)
	}
	ctx = spi.WithSubDispatch(ctx, spi.SubDispatch{Runner: sub.Runner, Depth: sub.Depth + 1})

	vars := map[string]any{}
	var traces []trace
	failed := 0
	// stop 收集子调用里出现的 Result.Stop。
	//
	// 为什么必须传出去：exit_plan_mode / ask_user_question 的语义是
	// "这一步之后停下来，把控制权交回人"。它们的 Stop 若被编排吞掉，
	// 就会出现"方案交上去了、模型却自己接着动手"——计划模式当场失效。
	stop := false

	for i, st := range steps {
		if strings.TrimSpace(st.Tool) == "" {
			return spi.Result{Hint: fmt.Sprintf("第 %d 步缺 tool", i+1)}, fmt.Errorf("第 %d 步缺 tool", i+1)
		}
		// foreach：对数组逐项执行；不是数组就当普通一步
		items, ok := lookupArray(vars, st.Foreach)
		if st.Foreach == "" || !ok {
			tr, val, hardErr := runOne(ctx, sub.Runner, i, st, st.Args, vars)
			traces = append(traces, tr)
			if hardErr != nil {
				// 只有通道级 hardErr 才向上报错；工具自身的失败记进 trace 继续。
				res := buildResult(traces, vars, failed, a.Meta, stop)
				res.Hint = hardHint(hardErr)
				return res, hardErr
			}
			if !tr.OK {
				failed++
			}
			if tr.stop {
				stop = true
			}
			if st.Save != "" {
				vars[st.Save] = val
			}
			continue
		}
		var got []any
		for j, it := range items {
			callArgs := map[string]any{}
			for k, v := range st.Args {
				callArgs[k] = v
			}
			callArgs["item"] = it
			tr, val, hardErr := runOne(ctx, sub.Runner, i, st, callArgs, vars)
			tr.Step = i
			tr.Tool = st.Tool
			_ = j
			traces = append(traces, tr)
			if hardErr != nil {
				res := buildResult(traces, vars, failed, a.Meta, stop)
				res.Hint = hardHint(hardErr)
				return res, hardErr
			}
			if !tr.OK {
				failed++
				continue
			}
			if tr.stop {
				stop = true
			}
			got = append(got, val)
		}
		if st.Save != "" {
			vars[st.Save] = got
		}
	}

	return buildResult(traces, vars, failed, a.Meta, stop), nil
}

// runOne 执行一步，并把结果折成 trace。
//
// 关键：**工具失败不返回 error**。真实 Text/Data/Artifacts/View/Hint
// 都留在当前 trace；W22 已修原生 agent 错误投影，不需要把整次 workflow
// 变成 error。单步失败记进 trace，脚本整体仍算跑完。
// 取消/超时同样维持原有软失败语义：记 trace，由编排继续处理。
func runOne(ctx context.Context, r spi.Runner, idx int, st step, callArgs map[string]any, vars map[string]any) (trace, any, error) {
	rawArgs, err := json.Marshal(expand(callArgs, vars))
	if err != nil {
		return trace{Step: idx, Tool: st.Tool, OK: false, Brief: "参数序列化失败"}, nil, nil
	}
	start := time.Now()
	res, err := r.Run(ctx, st.Tool, rawArgs)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		tr := trace{
			Step: idx, Tool: st.Tool, OK: false, Brief: trunc(err.Error(), 200),
			Detail: err.Error(), MS: ms, IsError: true,
			// 子调用的真实正文**有界**保留：只留 err.Error() 的话，
			// "第 N 步失败了"之外的信息（工具到底吐了什么）全丢，而模型
			// 判断下一步恰恰只能靠它。上界与回给模型的正文同一个预算，
			// 取回指针仍在 Artifacts 里，不会因为截断就找不到全文。
			Text: trunc(res.Text, maxTextRunes), Data: res.Data,
			Artifacts: res.Artifacts, Hint: res.Hint, View: res.View,
		}
		if res.Text != "" {
			tr.Brief = trunc(firstLine(res.Text), 200)
		} else if res.Summary != "" {
			tr.Brief = trunc(res.Summary, 200)
		}
		return tr, dataOf(res), nil
	}
	brief := res.Summary
	if brief == "" {
		brief = trunc(firstLine(res.Text), 120)
	}
	return trace{Step: idx, Tool: st.Tool, OK: true, Brief: trunc(brief, 200), MS: ms,
		stop: res.Stop}, dataOf(res), nil
}

func buildResult(traces []trace, vars map[string]any, failed int, meta map[string]any, stop bool) spi.Result {
	var b strings.Builder
	for _, tr := range traces {
		mark := "ok"
		if !tr.OK {
			mark = "ERR"
		}
		fmt.Fprintf(&b, "%d. %s [%s] %s\n", tr.Step+1, tr.Tool, mark, tr.Brief)
	}
	text := b.String()
	if len([]rune(text)) > maxTextRunes {
		text = string([]rune(text)[:maxTextRunes]) + "\n…（步骤太多，后面截断）"
	}
	summary := fmt.Sprintf("跑了 %d 步", len(traces))
	if failed > 0 {
		summary += fmt.Sprintf("，%d 步失败", failed)
	}
	return spi.Result{
		Summary: summary,
		Text:    text,
		// 子调用里只要有一个要求停，整段就停：宁可多停一次，也不要越过人要批的那一步。
		Stop: stop,
		Data: map[string]any{
			"ok":     failed == 0,
			"failed": failed,
			"steps":  traces,
			"vars":   vars,
			// meta 原样回显：调用方拿它和自己的那批任务对账，
			// 不用再去猜"这条结果对应我发的哪一次编排"。
			"meta": meta,
		},
	}
}

// hardHint 是"通道级失败"时给模型的那句话。
//
// 关键信息是"**前面的步骤已经执行过**"：编排跑到一半断了，
// 模型若当作什么都没发生重跑一遍，写盘的步骤就会做两次。
func hardHint(cause error) string {
	return "编排被中断（" + trunc(cause.Error(), 120) +
		"）：上面的步骤已经执行过，别当没发生；要重跑就只把没跑完的那几步写进 script"
}

// expand 把参数里的 "${var}" 替换成前面 save 的结果（字符串形式）。
func expand(in map[string]any, vars map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = expandValue(v, vars)
	}
	return out
}

func expandValue(v any, vars map[string]any) any {
	switch t := v.(type) {
	case string:
		if name, ok := strings.CutPrefix(t, "${"); ok {
			if name, ok := strings.CutSuffix(name, "}"); ok {
				if got, hit := vars[name]; hit {
					return got
				}
			}
		}
		return t
	case map[string]any:
		return expand(t, vars)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = expandValue(e, vars)
		}
		return out
	default:
		return v
	}
}

// lookupArray 取 ${var} 指的是不是数组。
func lookupArray(vars map[string]any, ref string) ([]any, bool) {
	if ref == "" {
		return nil, false
	}
	name, ok := strings.CutPrefix(ref, "${")
	if !ok {
		return nil, false
	}
	name, ok = strings.CutSuffix(name, "}")
	if !ok {
		return nil, false
	}
	arr, ok := vars[name].([]any)
	return arr, ok
}

// dataOf 取结果里给下一步用的东西：优先 Data，其次 Text。
func dataOf(res spi.Result) any {
	if res.Data != nil {
		return res.Data
	}
	return res.Text
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Module 返回装配用的模块描述。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-workflow",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			return reg.Register(&Tool{})
		},
	}
}

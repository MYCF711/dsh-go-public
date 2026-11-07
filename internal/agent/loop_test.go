package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"dshgo/internal/llm"
	"dshgo/internal/session"
	"dshgo/internal/spi"
)

// ---------- 假模型 ----------

type scriptedChat struct {
	steps []*llm.Response
	errs  []error
	i     int
	reqs  []llm.Request
}

func (c *scriptedChat) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event) error) (*llm.Response, error) {
	c.reqs = append(c.reqs, req)
	if c.i >= len(c.steps) {
		return nil, fmt.Errorf("脚本用完了")
	}
	i := c.i
	c.i++
	if i < len(c.errs) && c.errs[i] != nil {
		return nil, c.errs[i]
	}
	r := c.steps[i]
	if onEvent != nil {
		if r.Reasoning != "" {
			_ = onEvent(llm.Event{Kind: llm.EventReasoning, Text: r.Reasoning})
		}
		if r.Content != "" {
			_ = onEvent(llm.Event{Kind: llm.EventContent, Text: r.Content})
		}
	}
	return r, nil
}

// ---------- 假工具 ----------

type fakeTool struct {
	name string
	res  spi.Result
	err  error
	runs int
}

func (t *fakeTool) Name() string            { return t.name }
func (t *fakeTool) Caps() spi.Caps          { return spi.Caps{} }
func (t *fakeTool) Description() string     { return "假工具" }
func (t *fakeTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *fakeTool) Exec(context.Context, json.RawMessage) (spi.Result, error) {
	t.runs++
	return t.res, t.err
}

type fakeRegistry struct{ tools map[string]spi.Tool }

func (r *fakeRegistry) Register(t spi.Tool) error {
	if r.tools == nil {
		r.tools = map[string]spi.Tool{}
	}
	r.tools[t.Name()] = t
	return nil
}
func (r *fakeRegistry) Tools() []spi.Tool {
	out := make([]spi.Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	return out
}
func (r *fakeRegistry) Get(name string) (spi.Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

type fakeRunner struct {
	reg  *fakeRegistry
	runs int
}

func (r *fakeRunner) Run(ctx context.Context, name string, args json.RawMessage) (spi.Result, error) {
	r.runs++
	t, ok := r.reg.Get(name)
	if !ok {
		return spi.Result{Hint: "没有这个工具"}, fmt.Errorf("工具 %q 不存在", name)
	}
	return t.Exec(ctx, args)
}

// writeTool 是一个**真的会落盘**的写工具。
//
// 为什么不用 fakeTool 的计数器：这条用例要断言的是"malformed 之后磁盘上
// 什么都没有"，计数器只能证明"没调 Exec"，证明不了"没落盘"。
type writeTool struct {
	dir  string
	runs int
}

func (t *writeTool) Name() string            { return "write" }
func (t *writeTool) Caps() spi.Caps          { return spi.Caps{Writes: true} }
func (t *writeTool) Description() string     { return "写文件" }
func (t *writeTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (t *writeTool) Exec(_ context.Context, raw json.RawMessage) (spi.Result, error) {
	t.runs++
	var in struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return spi.Result{Hint: "参数坏了"}, err
	}
	if err := os.WriteFile(in.FilePath, []byte(in.Content), 0o644); err != nil {
		return spi.Result{Hint: "写失败"}, err
	}
	return spi.Result{Summary: "written " + in.FilePath}, nil
}

func newLoop(t *testing.T, chat Chat, tools ...spi.Tool) (*Loop, *[]Event) {
	t.Helper()
	reg := &fakeRegistry{tools: map[string]spi.Tool{}}
	for _, tl := range tools {
		if err := reg.Register(tl); err != nil {
			t.Fatal(err)
		}
	}
	var events []Event
	l := &Loop{
		Chat:   chat,
		Tools:  reg,
		Runner: &fakeRunner{reg: reg},
		Model:  "test-model",
		Emit: func(e Event) error {
			events = append(events, e)
			return nil
		},
	}
	return l, &events
}

func kinds(events []Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = string(e.Kind)
	}
	return out
}

// ---------- 测试 ----------

func TestSingleStepWithoutTools(t *testing.T) {
	chat := &scriptedChat{steps: []*llm.Response{{Content: "你好", FinishReason: "stop"}}}
	l, events := newLoop(t, chat)

	hist, err := l.Run(context.Background(), 1, []llm.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || hist[1].Role != "assistant" || hist[1].Content != "你好" {
		t.Fatalf("历史 = %+v", hist)
	}
	got := strings.Join(kinds(*events), ",")
	want := "step/start,assistant/text,assistant/message,step/end,turn/end"
	if got != want {
		t.Fatalf("事件序列 = %s，期望 %s", got, want)
	}
	if (*events)[len(*events)-1].Finish != "stop" {
		t.Fatalf("turn/end 没带 finish: %+v", (*events)[len(*events)-1])
	}
}

func TestRegularRequestUsesModelCapacity(t *testing.T) {
	for _, tc := range []struct {
		name          string
		model         string
		declared      llm.ModelCapacity
		wantMaxTokens int
	}{
		{name: "未声明自定义模型使用十进制128K输出", model: "custom/undeclared", wantMaxTokens: 128000},
		{
			name: "显式32K上下文和8K输出覆盖不变", model: "custom/small",
			declared: llm.ModelCapacity{ContextWindow: 32000, MaxOutput: 8000}, wantMaxTokens: 8000,
		},
		{name: "内置已知模型保持原行为", model: "group/deepseek-v4.1-flash-1017", wantMaxTokens: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chat := &scriptedChat{steps: []*llm.Response{{Content: "好", FinishReason: "stop"}}}
			l, _ := newLoop(t, chat)
			l.Model = tc.model
			l.Capacity = tc.declared
			if _, err := l.Run(context.Background(), 1, []llm.Message{{Role: "user", Content: "hi"}}); err != nil {
				t.Fatal(err)
			}
			if len(chat.reqs) != 1 {
				t.Fatalf("请求数 = %d，期望 1", len(chat.reqs))
			}
			if got := chat.reqs[0].MaxTokens; got != tc.wantMaxTokens {
				t.Fatalf("常规请求 max_tokens = %d，期望 %d", got, tc.wantMaxTokens)
			}
		})
	}
}

func TestToolCallRunsAndFeedsBack(t *testing.T) {
	tool := &fakeTool{name: "read", res: spi.Result{Summary: "读到 3 行"}}
	chat := &scriptedChat{steps: []*llm.Response{
		{
			FinishReason: "tool_calls",
			ToolCalls: []llm.ToolCall{{
				ID: "call_1", Type: "function",
				Function: llm.Func{Name: "read", Arguments: `{"file_path":"a.txt"}`},
			}},
		},
		{Content: "读完了", FinishReason: "stop"},
	}}
	l, events := newLoop(t, chat, tool)

	hist, err := l.Run(context.Background(), 2, []llm.Message{{Role: "user", Content: "读 a.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 4 {
		t.Fatalf("历史长度 = %d：%+v", len(hist), hist)
	}
	if hist[1].Role != "assistant" || len(hist[1].ToolCalls) != 1 {
		t.Fatalf("assistant 消息没带工具调用: %+v", hist[1])
	}
	if hist[2].Role != "tool" || hist[2].ToolCallID != "call_1" {
		t.Fatalf("tool 消息不对: %+v", hist[2])
	}
	if !strings.Contains(hist[2].Content, "读到 3 行") {
		t.Fatalf("tool 消息内容 = %q", hist[2].Content)
	}
	if hist[3].Content != "读完了" {
		t.Fatalf("最终回答 = %q", hist[3].Content)
	}

	got := strings.Join(kinds(*events), ",")
	want := "step/start,assistant/message,tool/call,tool/result,step/end,step/start,assistant/text,assistant/message,step/end,turn/end"
	if got != want {
		t.Fatalf("事件序列 =\n %s\n期望\n %s", got, want)
	}
}

func TestToolErrorBecomesToolMessage(t *testing.T) {
	tool := &fakeTool{name: "read", res: spi.Result{Hint: "先 glob 找文件"}, err: errors.New("文件不存在")}
	chat := &scriptedChat{steps: []*llm.Response{
		{
			FinishReason: "tool_calls",
			ToolCalls: []llm.ToolCall{{
				ID: "c1", Function: llm.Func{Name: "read", Arguments: `{}`},
			}},
		},
		{Content: "抱歉", FinishReason: "stop"},
	}}
	l, events := newLoop(t, chat, tool)

	hist, err := l.Run(context.Background(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 没有初始历史时：0=assistant(tool_calls)，1=tool 结果，2=最终回答。
	if hist[1].Role != "tool" {
		t.Fatalf("第 1 条应是 tool 消息，得到 %+v", hist[1])
	}
	if !strings.Contains(hist[1].Content, "文件不存在") || !strings.Contains(hist[1].Content, "先 glob 找文件") {
		t.Fatalf("失败信息与 Hint 都要进 tool 消息，得到 %q", hist[1].Content)
	}
	for _, e := range *events {
		if e.Kind == EventToolResult && !e.IsError {
			t.Fatal("tool/result 应标记 IsError")
		}
	}
}

func TestToolErrorPreservesActualResult(t *testing.T) {
	toolErr := errors.New("synthetic failure must not replace actual output")
	data := map[string]any{"exit_code": 7, "detail": "kept"}
	artifacts := []spi.Artifact{{Name: "stderr", Path: "tmp/actual-body.log", Bytes: 11}}
	view := &spi.ViewSpec{Kind: spi.ViewPlain, Title: "actual view", Body: "actual-body-view"}
	truncated := &spi.Truncation{Kept: 11, Total: 9000, Pointer: "tmp/full-output.txt"}
	tool := &fakeTool{name: "read", res: spi.Result{
		Text: "actual-body", Data: data, Artifacts: artifacts, Truncated: truncated,
		View: view, Hint: "保留原始提示",
	}, err: toolErr}
	chat := &scriptedChat{steps: []*llm.Response{
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{
			ID: "c1", Function: llm.Func{Name: "read", Arguments: `{}`},
		}}},
		{Content: "done", FinishReason: "stop"},
	}}
	l, events := newLoop(t, chat, tool)
	l.Project = session.ProjectResult

	hist, err := l.Run(context.Background(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 3 || hist[1].Role != "tool" {
		t.Fatalf("历史缺少 tool 消息：%+v", hist)
	}
	if !strings.Contains(hist[1].Content, "actual-body") {
		t.Fatalf("模型 tool 消息丢了工具真实 Text：%q", hist[1].Content)
	}
	// 失败摘要不能被 ProjectResult 的「Text 优先」规则藏掉：
	// 模型必须同时看到"错在哪"和"工具到底返回了什么"。
	if !strings.Contains(hist[1].Content, toolErr.Error()) {
		t.Fatalf("模型 tool 消息丢了错误摘要：%q", hist[1].Content)
	}

	var got *Event
	for i := range *events {
		if (*events)[i].Kind == EventToolResult {
			got = &(*events)[i]
		}
	}
	if got == nil {
		t.Fatal("没有发出 tool/result 事件")
	}
	if !got.IsError || !errors.Is(got.Err, toolErr) {
		t.Fatalf("tool/result 必须保留原始 err 且 IsError=true：%+v", got)
	}
	if got.Result == nil || got.Result.Text != "actual-body" {
		t.Fatalf("tool/result 丢了工具真实 Text：%+v", got.Result)
	}
	if !reflect.DeepEqual(got.Result.Data, data) {
		t.Fatalf("Data 被改写：got=%#v want=%#v", got.Result.Data, data)
	}
	if !reflect.DeepEqual(got.Result.Artifacts, artifacts) {
		t.Fatalf("Artifacts 被改写：got=%+v want=%+v", got.Result.Artifacts, artifacts)
	}
	if !reflect.DeepEqual(got.Result.Truncated, truncated) {
		t.Fatalf("Truncated 被改写：got=%+v want=%+v", got.Result.Truncated, truncated)
	}
	if !reflect.DeepEqual(got.Result.View, view) {
		t.Fatalf("View 被改写：got=%+v want=%+v", got.Result.View, view)
	}
	if got.Result.Hint != "保留原始提示" {
		t.Fatalf("Hint 被改写：%q", got.Result.Hint)
	}
	// 失败结果的 Text 必须在 tool/result 的渲染里也有出口：journal 与客户端
	// 看到的是这份 Result，不是模型消息。
	if !strings.Contains(renderBlocks(session.ProjectResult(*got.Result)), "actual-body") {
		t.Fatalf("tool/result 的 Result 投影丢了 actual-body：%+v", got.Result)
	}
}

func TestToolErrorWithoutTextUsesSyntheticError(t *testing.T) {
	tool := &fakeTool{name: "read", res: spi.Result{Hint: "先 glob 找文件"}, err: errors.New("文件不存在")}
	chat := &scriptedChat{steps: []*llm.Response{
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{
			ID: "c1", Function: llm.Func{Name: "read", Arguments: `{}`},
		}}},
		{Content: "done", FinishReason: "stop"},
	}}
	l, events := newLoop(t, chat, tool)
	l.Project = session.ProjectResult

	hist, err := l.Run(context.Background(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hist[1].Content, "Error: 文件不存在") || !strings.Contains(hist[1].Content, "先 glob 找文件") {
		t.Fatalf("无 Text 时必须合成可读错误并保留 Hint：%q", hist[1].Content)
	}
	for _, e := range *events {
		if e.Kind == EventToolResult {
			if !e.IsError || e.Result == nil || !strings.Contains(e.Result.Summary, "Error: 文件不存在") {
				t.Fatalf("无 Text 时 tool/result 必须带合成错误且 IsError=true：%+v", e)
			}
		}
	}
}

func TestMaxStepsGuard(t *testing.T) {
	call := llm.ToolCall{ID: "c", Function: llm.Func{Name: "read", Arguments: `{}`}}
	chat := &scriptedChat{steps: []*llm.Response{
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{call}},
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{call}},
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{call}},
	}}
	l, _ := newLoop(t, chat, &fakeTool{name: "read", res: spi.Result{Summary: "x"}})
	l.MaxSteps = 3

	_, err := l.Run(context.Background(), 1, nil)
	if err == nil || !strings.Contains(err.Error(), "36") && !strings.Contains(err.Error(), "3") {
		t.Fatalf("步数用尽必须报错，得到 %v", err)
	}
}

func TestEmitErrorStops(t *testing.T) {
	chat := &scriptedChat{steps: []*llm.Response{{Content: "x", FinishReason: "stop"}}}
	l, _ := newLoop(t, chat)
	l.Emit = func(Event) error { return errors.New("下游不要了") }
	if _, err := l.Run(context.Background(), 1, nil); err == nil {
		t.Fatal("Emit 报错必须中断")
	}
}

func TestSystemPromptInsertedOnce(t *testing.T) {
	chat := &scriptedChat{steps: []*llm.Response{{Content: "ok", FinishReason: "stop"}}}
	l, _ := newLoop(t, chat)
	l.System = "你是 dsh-go"
	hist, err := l.Run(context.Background(), 1, []llm.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if hist[0].Role != "system" || hist[0].Content != "你是 dsh-go" {
		t.Fatalf("系统提示没插到最前面: %+v", hist)
	}
	if len(chat.reqs) != 1 || chat.reqs[0].Messages[0].Role != "system" {
		t.Fatal("发给模型的请求里没有系统提示")
	}
}

func TestToolDefsReachTheModel(t *testing.T) {
	chat := &scriptedChat{steps: []*llm.Response{{Content: "ok", FinishReason: "stop"}}}
	l, _ := newLoop(t, chat, &fakeTool{name: "read"}, &fakeTool{name: "write"})
	if _, err := l.Run(context.Background(), 1, nil); err != nil {
		t.Fatal(err)
	}
	defs := chat.reqs[0].Tools
	if len(defs) != 2 {
		t.Fatalf("工具定义数 = %d", len(defs))
	}
	for _, d := range defs {
		if d.Type != "function" || d.Function.Name == "" || len(d.Function.Parameters) == 0 {
			t.Fatalf("工具定义不完整: %+v", d)
		}
	}
}

func TestModelErrorSurfaces(t *testing.T) {
	chat := &scriptedChat{steps: []*llm.Response{{}}, errs: []error{errors.New("网关 502")}}
	l, _ := newLoop(t, chat)
	_, err := l.Run(context.Background(), 1, nil)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("模型错误要冒出来，得到 %v", err)
	}
}

// 工具失败时，客户端看到的那条记录必须带原因。
// 早先发出的是工具原始的 Result（Summary 为空），界面上就是"一行红色、什么都不说"。
func TestToolErrorCarriesReasonToClient(t *testing.T) {
	losingErr := errors.New("文件不存在：a.txt")
	chat := &scriptedChat{steps: []*llm.Response{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.Func{
			Name: "read", Arguments: `{"file_path":"a.txt"}`,
		}}}, FinishReason: "tool_calls"},
		{Content: "好", FinishReason: "stop"},
	}}
	reg := &fakeRegistry{tools: map[string]spi.Tool{"read": &fakeTool{name: "read", err: losingErr}}}
	var got *spi.Result
	proj := func(r spi.Result) []spi.Block { return []spi.Block{{Type: "text", Text: r.Summary}} }
	l := &Loop{
		Chat:     chat,
		Tools:    reg,
		Runner:   &errRunner{err: losingErr, hint: "先 glob 找文件"},
		Model:    "m",
		MaxSteps: 2,
		Project:  proj,
		Emit: func(e Event) error {
			if e.Kind == EventToolResult && e.IsError {
				got = e.Result
			}
			return nil
		},
	}
	if _, err := l.Run(context.Background(), 1, nil); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("没有发出 tool/result 事件")
	}
	if got.Summary == "" {
		t.Fatal("失败事件的文本是空的——客户端只会看到一行没有原因的红色记录")
	}
	if !strings.Contains(got.Summary, "a.txt") {
		t.Fatalf("文本里没有原因: %q", got.Summary)
	}
	if got.Hint == "" {
		t.Fatal("Hint 丢了")
	}
}

type errRunner struct {
	err  error
	hint string
}

func (r *errRunner) Run(ctx context.Context, name string, args json.RawMessage) (spi.Result, error) {
	return spi.Result{Hint: r.hint}, r.err
}

// 取消 ≠ 失败。命令可能已经跑完，只是结果没收到。
// 说成 failed 会让模型以为"什么都没发生"，于是做出错误判断。
func TestCancelledToolIsUnknownNotFailed(t *testing.T) {
	chat := &scriptedChat{steps: []*llm.Response{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.Func{
			Name: "bash", Arguments: `{"command":"sleep 10"}`,
		}}}, FinishReason: "tool_calls"},
	}}
	var got *spi.Result
	l := &Loop{
		Chat:     chat,
		Tools:    &fakeRegistry{tools: map[string]spi.Tool{"bash": &fakeTool{name: "bash"}}},
		Runner:   &errRunner{err: context.Canceled},
		Model:    "m",
		MaxSteps: 1,
		Project:  func(r spi.Result) []spi.Block { return []spi.Block{{Type: "text", Text: r.Summary}} },
		Emit: func(e Event) error {
			if e.Kind == EventToolResult && e.IsError {
				got = e.Result
			}
			return nil
		},
	}
	_, _ = l.Run(context.Background(), 1, nil)
	if got == nil {
		t.Fatal("没有发出 tool/result 事件——日志里会留下一个有调用没结果的洞")
	}
	if !strings.Contains(got.Summary, "outcome is unknown") {
		t.Fatalf("取消应该报「结果未知」，实际: %q", got.Summary)
	}
	if strings.Contains(got.Summary, "canceled") {
		t.Fatalf("不该说成失败/取消: %q", got.Summary)
	}
	if got.Hint == "" {
		t.Fatal("结果未知时必须给下一步提示")
	}
}

// 合法调用与坏调用同处一个 completed response 时，不能先执行合法调用。
// 这个用例在旧实现上必须先红：Runner.Run=2、合法工具 Exec=1。
func TestValidCallBeforeMalformedCallIsNotExecuted(t *testing.T) {
	read := &fakeTool{name: "read", res: spi.Result{Summary: "ok"}}
	chat := &scriptedChat{steps: []*llm.Response{{
		FinishReason: "tool_calls",
		ToolCalls: []llm.ToolCall{
			{ID: "good_call", Function: llm.Func{Name: "read", Arguments: `{"file_path":"a.txt"}`}},
			{ID: "bad_call", Function: llm.Func{Name: "broken", Arguments: `{"x":`}},
		},
	}}}
	l, events := newLoop(t, chat, read)

	hist, err := l.Run(context.Background(), 1, []llm.Message{{Role: "user", Content: "hi"}})
	runner := l.Runner.(*fakeRunner)
	if runner.runs != 0 || read.runs != 0 {
		t.Fatalf("坏响应执行了工具：Runner.Run=%d，合法工具 Exec=%d；两者都必须为 0", runner.runs, read.runs)
	}
	if err == nil || !strings.Contains(err.Error(), "bad_call") ||
		!strings.Contains(err.Error(), "JSON 对象") || !strings.Contains(err.Error(), "unexpected end of JSON input") {
		t.Fatalf("错误必须带 callId 与具体 JSON 原因，得到 %v", err)
	}
	if len(chat.reqs) != 1 {
		t.Fatalf("坏响应必须立即终止，实际又请求了模型 %d 次", len(chat.reqs)-1)
	}
	// 契约迁移（HOST-TOOL-SAFETY/A1）：W22 那版允许"assistant 是模型确实产生的
	// 原始事实，可以先落盘"。新契约更严——作废的响应**整体**不算产出：
	// 0 工具事件、0 assistant 消息、0 下一次模型请求，只留模型调用前的 step/start。
	if got, want := strings.Join(kinds(*events), ","), string(EventStepStart); got != want {
		t.Fatalf("坏响应不得发 tool/call、tool/result、assistant/message 或正常收尾，事件=%s，期望=%s", got, want)
	}
	if len(hist) != 1 || hist[0].Role != "user" {
		t.Fatalf("作废的响应不该写进历史，历史=%+v", hist)
	}
	for _, msg := range hist {
		if msg.Role == "assistant" || msg.Role == "tool" {
			t.Fatalf("未执行的响应却留下了 %s 消息：%+v", msg.Role, msg)
		}
	}
}

func TestMalformedToolCallPositionFailsWholeResponse(t *testing.T) {
	for _, badPos := range []int{0, 1, 2} {
		t.Run([]string{"first", "middle", "last"}[badPos], func(t *testing.T) {
			calls := make([]llm.ToolCall, 3)
			for i := range calls {
				calls[i] = llm.ToolCall{
					ID:       fmt.Sprintf("good_%d", i),
					Function: llm.Func{Name: "read", Arguments: `{}`},
				}
			}
			calls[badPos] = llm.ToolCall{
				ID: "bad_call", Function: llm.Func{Name: "broken", Arguments: `{"x":`},
			}
			read := &fakeTool{name: "read", res: spi.Result{Summary: "ok"}}
			chat := &scriptedChat{steps: []*llm.Response{{FinishReason: "tool_calls", ToolCalls: calls}}}
			l, events := newLoop(t, chat, read)

			hist, err := l.Run(context.Background(), 1, nil)
			runner := l.Runner.(*fakeRunner)
			if runner.runs != 0 || read.runs != 0 {
				t.Fatalf("坏 call 在位置 %d 时仍有执行：Runner.Run=%d，工具 Exec=%d", badPos, runner.runs, read.runs)
			}
			if err == nil || !strings.Contains(err.Error(), "bad_call") || !strings.Contains(err.Error(), "JSON 对象") {
				t.Fatalf("错误必须指出坏 call 与参数格式，得到 %v", err)
			}
			// 契约迁移（HOST-TOOL-SAFETY/A1）：作废的响应不发 assistant/message，
			// 也不写历史——它整体不是模型产出的一部分。
			if got, want := strings.Join(kinds(*events), ","), string(EventStepStart); got != want {
				t.Fatalf("坏 call 在位置 %d 时不得开始任何工具/assistant 事件，事件=%s，期望=%s", badPos, got, want)
			}
			if len(hist) != 0 {
				t.Fatalf("坏 call 在位置 %d 时不该写进历史，历史=%+v", badPos, hist)
			}
		})
	}
}

func TestToolArgumentsMustBeJSONObject(t *testing.T) {
	cases := map[string]string{
		"invalid syntax": `{"x":`,
		"string":         `"not an object"`,
		"array":          `[]`,
		"null":           `null`,
		// 契约迁移（HOST-TOOL-SAFETY/A1）：空串从这张表移到
		// TestEmptyArgumentsAreNotMalformed——判据只有一份且与
		// tools.Pipeline 共用，而流水线必须允许宿主用 nil 调零参数工具
		// （now_line 等）。详见该用例。
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			read := &fakeTool{name: "read", res: spi.Result{Summary: "ok"}}
			chat := &scriptedChat{steps: []*llm.Response{{ToolCalls: []llm.ToolCall{
				{ID: "bad_call", Function: llm.Func{Name: "read", Arguments: args}},
			}}}}
			l, _ := newLoop(t, chat, read)
			_, err := l.Run(context.Background(), 1, nil)
			runner := l.Runner.(*fakeRunner)
			if runner.runs != 0 || read.runs != 0 {
				t.Fatalf("非 object 参数 %q 被执行：Runner.Run=%d，工具 Exec=%d", args, runner.runs, read.runs)
			}
			if err == nil || !strings.Contains(err.Error(), "bad_call") || !strings.Contains(err.Error(), "JSON 对象") {
				t.Fatalf("非 object 参数 %q 的错误不完整：%v", args, err)
			}
		})
	}
}

// 同一完整响应里只要有一条 tool call 的参数不是 JSON 对象，**整条响应作废**：
// 排在它前面的合法写工具一次都不能执行——"先做一半再报错"比干脆失败更糟，
// 因为模型会以为整批都没发生，重跑一遍就写两次。
//
// 零副作用的判据必须同时是四样：0 执行、0 落盘、0 事件、0 assistant 消息。
// 只断言 err != nil 不够——W22 那版就是把校验放在 assistant/message 之后，
// 错误照样返回，界面上却已经多了一条"模型说了话"的记录。
func TestMalformedToolCallAbortsWholeResponseBeforeAnyEffect(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "written.txt")
	write := &writeTool{dir: dir}
	chat := &scriptedChat{steps: []*llm.Response{
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{
			// 第一个是**合法**的写工具：它一旦执行就会落盘。
			{ID: "c1", Function: llm.Func{
				Name:      "write",
				Arguments: fmt.Sprintf(`{"file_path":%q,"content":"should-not-exist"}`, filepath.ToSlash(target)),
			}},
			// 第二个参数 malformed：整条响应必须在这里作废。
			{ID: "c22", Function: llm.Func{Name: "write", Arguments: `{"file_path": "{not json"`}},
		}},
		// 第二步故意留着：如果循环没终止，它会用掉这一步。
		{Content: "不该走到这里", FinishReason: "stop"},
	}}
	l, events := newLoop(t, chat, write)

	hist, err := l.Run(context.Background(), 1, nil)
	if err == nil {
		t.Fatal("任一 call 参数 malformed 必须终止整条响应")
	}
	if !strings.Contains(err.Error(), "c22") {
		t.Fatalf("错误必须点出是哪一条 call 坏了：%v", err)
	}

	// 0 执行：Runner 与工具 Exec 都不能被调用一次（审批在 Runner 上层，
	// 没走到 Runner 就等于没走审批）。
	if runner := l.Runner.(*fakeRunner); runner.runs != 0 {
		t.Fatalf("malformed 之后不该再调 Runner：%d 次", runner.runs)
	}
	if write.runs != 0 {
		t.Fatalf("合法的前置写工具也不得执行：%d 次", write.runs)
	}
	// 0 落盘。
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("malformed 之后磁盘上不该有任何文件：%v", entries)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("%s 不该存在（stat err=%v）", target, statErr)
	}
	// 0 assistant 消息：作废的响应不是模型产出的一部分。
	if len(hist) != 0 {
		t.Fatalf("作废的响应不该写进历史：%+v", hist)
	}
	// 0 工具/assistant 事件：只剩模型调用前的那条 step/start。
	if got := strings.Join(kinds(*events), ","); got != string(EventStepStart) {
		t.Fatalf("事件序列 = %q，期望只有 step/start", got)
	}
	// 0 下一次模型请求。
	if len(chat.reqs) != 1 {
		t.Fatalf("作废之后不该再问模型：发了 %d 次请求", len(chat.reqs))
	}
}

// 空参数不是 malformed：零参数工具的 tool call 本来就可能是空串。
// 把它当格式错误会让整条响应作废，而这是正常调用。
func TestEmptyArgumentsAreNotMalformed(t *testing.T) {
	ping := &fakeTool{name: "ping", res: spi.Result{Summary: "pong"}}
	chat := &scriptedChat{steps: []*llm.Response{
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{
			{ID: "c1", Function: llm.Func{Name: "ping", Arguments: ""}},
			{ID: "c2", Function: llm.Func{Name: "ping", Arguments: "  "}},
		}},
		{Content: "好", FinishReason: "stop"},
	}}
	l, _ := newLoop(t, chat, ping)

	if _, err := l.Run(context.Background(), 1, nil); err != nil {
		t.Fatalf("空参数不该终止响应：%v", err)
	}
	if ping.runs != 2 {
		t.Fatalf("两个零参数 call 都应执行：%d", ping.runs)
	}
}

func TestAllValidToolCallsRun(t *testing.T) {
	read := &fakeTool{name: "read", res: spi.Result{Summary: "ok"}}
	chat := &scriptedChat{steps: []*llm.Response{
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{
			{ID: "c1", Function: llm.Func{Name: "read", Arguments: `{"file_path":"a.txt"}`}},
			{ID: "c2", Function: llm.Func{Name: "read", Arguments: `{}`}},
		}},
		{Content: "好了", FinishReason: "stop"},
	}}
	l, events := newLoop(t, chat, read)

	hist, err := l.Run(context.Background(), 1, nil)
	if err != nil {
		t.Fatalf("全合法响应必须正常执行：%v", err)
	}
	runner := l.Runner.(*fakeRunner)
	if runner.runs != 2 || read.runs != 2 {
		t.Fatalf("两个合法 call 都应执行一次：Runner.Run=%d，工具 Exec=%d", runner.runs, read.runs)
	}
	if len(hist) != 4 || hist[1].ToolCallID != "c1" || hist[2].ToolCallID != "c2" || hist[3].Content != "好了" {
		t.Fatalf("全合法响应的历史不完整：%+v", hist)
	}
	want := "step/start,assistant/message,tool/call,tool/result,tool/call,tool/result,step/end," +
		"step/start,assistant/text,assistant/message,step/end,turn/end"
	if got := strings.Join(kinds(*events), ","); got != want {
		t.Fatalf("全合法响应事件=\n%s\n期望=\n%s", got, want)
	}
}

// 未知工具与缺 required 字段不是 malformed JSON：仍按原语义生成可读的
// tool/result，让下一步模型自我纠正，不能被全响应格式校验顺手改成 turn 错误。
func TestNonMalformedToolCallErrorsKeepTheTurnAlive(t *testing.T) {
	read := &fakeTool{name: "read", err: errors.New("缺少 required 字段 file_path"), res: spi.Result{Hint: "补上 file_path"}}
	chat := &scriptedChat{steps: []*llm.Response{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Function: llm.Func{Name: "no_such_tool", Arguments: "{}"}}}},
		{ToolCalls: []llm.ToolCall{{ID: "c2", Function: llm.Func{Name: "read", Arguments: "{}"}}}},
		{Content: "好了", FinishReason: "stop"},
	}}
	l, _ := newLoop(t, chat, read)

	hist, err := l.Run(context.Background(), 1, []llm.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("非 malformed JSON 的工具错误必须保留原继续语义：%v", err)
	}
	var toolMsgs []string
	for _, msg := range hist {
		if msg.Role == "tool" {
			toolMsgs = append(toolMsgs, msg.Content)
		}
	}
	if len(toolMsgs) != 2 {
		t.Fatalf("未知工具与缺字段都应各有一条 tool 消息，实际 %d 条", len(toolMsgs))
	}
	for i, content := range toolMsgs {
		if strings.TrimSpace(content) == "" {
			t.Fatalf("第 %d 条 tool 消息为空", i+1)
		}
	}
	if !strings.Contains(toolMsgs[0], "不存在") && !strings.Contains(toolMsgs[0], "Error") {
		t.Fatalf("未知工具必须说清：%q", toolMsgs[0])
	}
	if !strings.Contains(toolMsgs[1], "required") || !strings.Contains(toolMsgs[1], "file_path") {
		t.Fatalf("缺 required 字段必须保留原错误：%q", toolMsgs[1])
	}
	if hist[len(hist)-1].Content != "好了" {
		t.Fatalf("模型应能在工具错误后继续，最终历史=%+v", hist)
	}
}

// 工具有话说"必须等人"（Result.Stop）：本轮就在这一步收尾，
// 模型**不会再被叫一次**去自己编答案。
func TestStopEndsTurnImmediately(t *testing.T) {
	chat := &scriptedChat{steps: []*llm.Response{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Function: llm.Func{
			Name: "ask", Arguments: `{}`}}}},
		// 第二步故意留着：如果循环没停，它会用掉这一步。
		{Content: "我猜答案是 A"},
	}}
	loop, events := newLoop(t, chat,
		&fakeTool{name: "ask", res: spi.Result{Summary: "等你回答", Stop: true}})
	hist := []llm.Message{{Role: "user", Content: "x"}}
	if _, err := loop.Run(context.Background(), 1, hist); err != nil {
		t.Fatal(err)
	}

	if len(chat.reqs) != 1 {
		t.Fatalf("设了 Stop 就不该再问模型，实际发了 %d 次请求", len(chat.reqs))
	}
	var end *Event
	for i := range *events {
		if (*events)[i].Kind == EventTurnEnd {
			end = &(*events)[i]
		}
	}
	if end == nil {
		t.Fatal("应该发出 TurnEnd")
	}
	if end.Finish != "awaiting_user" {
		t.Fatalf("收尾原因要说清在等人，实际 %q", end.Finish)
	}
}

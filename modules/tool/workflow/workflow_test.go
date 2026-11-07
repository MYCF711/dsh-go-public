package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"dshgo/internal/spi"
)

// fakeRunner 记录每次子调用的工具名与参数，并按预设返回。
type fakeRunner struct {
	calls []string
	args  []string
	reply func(tool string, args json.RawMessage) (spi.Result, error)
}

func (f *fakeRunner) Run(ctx context.Context, name string, args json.RawMessage) (spi.Result, error) {
	f.calls = append(f.calls, name)
	f.args = append(f.args, string(args))
	if f.reply != nil {
		return f.reply(name, args)
	}
	return spi.Result{Summary: name + " 成功", Data: map[string]any{"from": name}}, nil
}

func ctxWith(r spi.Runner) context.Context {
	return spi.WithSubDispatch(context.Background(), spi.SubDispatch{Runner: r})
}

// scriptArg 按宿主契约包一层：参数名是 script（内容是 JSON 步骤表）。
func scriptArg(t *testing.T, steps string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"script": steps})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func run(t *testing.T, ctx context.Context, raw string) (spi.Result, error) {
	t.Helper()
	return (&Tool{}).Exec(ctx, json.RawMessage(raw))
}

// 顺序执行 + ${var} 把上一步结果喂给下一步。
func TestSequentialStepsPassResults(t *testing.T) {
	f := &fakeRunner{}
	steps := `[{"tool":"glob","args":{"pattern":"*.go"},"save":"files"},
	           {"tool":"grep","args":{"in":"${files}"}}]`
	res, err := run(t, ctxWith(f), scriptArg(t, steps))
	if err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	if len(f.calls) != 2 || f.calls[0] != "glob" || f.calls[1] != "grep" {
		t.Fatalf("调用顺序不对：%v", f.calls)
	}
	// 第二步的参数里必须是第一步的结果，而不是字面量 "${files}"
	if !strings.Contains(f.args[1], "glob") {
		t.Fatalf("${files} 没被替换成上一步结果：%s", f.args[1])
	}
	if !strings.Contains(res.Text, "glob [ok]") || !strings.Contains(res.Text, "grep [ok]") {
		t.Fatalf("trace 不完整：%s", res.Text)
	}
}

// 单步失败**不是**脚本失败：错误记进 trace，继续往下跑，整体 err=nil。
//
// 这条是硬要求：宿主 agent 循环在 error 路径上会丢掉 Text/Data，
// 那样"第几步失败了"这个最有用的信息就没了。
func TestStepFailureDoesNotAbortScript(t *testing.T) {
	f := &fakeRunner{reply: func(tool string, _ json.RawMessage) (spi.Result, error) {
		if tool == "boom" {
			return spi.Result{Hint: "炸了"}, context.DeadlineExceeded
		}
		return spi.Result{Summary: "还行"}, nil
	}}
	res, err := run(t, ctxWith(f), scriptArg(t, `[{"tool":"boom"},{"tool":"ok"}]`))
	if err != nil {
		t.Fatalf("单步失败不该让整个脚本返回 error：%v", err)
	}
	if !strings.Contains(res.Text, "boom [ERR]") {
		t.Fatalf("失败步骤没标出来：%s", res.Text)
	}
	if !strings.Contains(res.Text, "ok [ok]") {
		t.Fatalf("失败之后应当继续执行后面的步骤：%s", res.Text)
	}
	m, _ := res.Data.(map[string]any)
	if m["ok"] != false {
		t.Fatalf("Data.ok 应当是 false：%v", m)
	}
	if m["failed"] != 1 {
		t.Fatalf("失败计数应当是 1：%v", m)
	}
}

// context.Canceled 与 DeadlineExceeded 一样，只结算当前步骤并继续。
func TestCancellationDoesNotAbortScript(t *testing.T) {
	f := &fakeRunner{reply: func(tool string, _ json.RawMessage) (spi.Result, error) {
		if tool == "boom" {
			return spi.Result{Text: "cancel-body", Hint: "已取消"}, context.Canceled
		}
		return spi.Result{Summary: "after cancel"}, nil
	}}
	res, err := run(t, ctxWith(f), scriptArg(t, `[{"tool":"boom"},{"tool":"after"}]`))
	if err != nil {
		t.Fatalf("取消不该让整个脚本返回 error：%v", err)
	}
	if !reflect.DeepEqual(f.calls, []string{"boom", "after"}) {
		t.Fatalf("取消后必须继续后续步骤：%v", f.calls)
	}
	if !strings.Contains(res.Text, "cancel-body") || !strings.Contains(res.Text, "after [ok]") {
		t.Fatalf("取消 trace 与后续步骤都要保留：%s", res.Text)
	}
	m, _ := res.Data.(map[string]any)
	steps, _ := m["steps"].([]trace)
	if m["ok"] != false || m["failed"] != 1 || len(steps) != 2 || steps[0].OK || !steps[0].IsError {
		t.Fatalf("取消必须只记一个失败步骤：%#v", m)
	}
}

// 普通嵌套失败必须保留子调用的真实结果，但仍只是一步失败：写 trace、
// 继续后续步骤、整体 err=nil。W22 已修原生 agent 错误投影，不需要靠
// 整次 workflow error 来保留 actual-body。
func TestNestedFailurePreservesTraceAndResult(t *testing.T) {
	nestedErr := errors.New("nested failed")
	want := spi.Result{
		Summary: "child summary",
		Text:    "actual-body",
		Data:    map[string]any{"answer": 42, "nested": map[string]any{"kept": true}},
		Artifacts: []spi.Artifact{
			{Name: "full-output", Path: `artifacts\full-output.txt`, Bytes: 123},
		},
		Hint: "child hint",
		View: &spi.ViewSpec{Kind: spi.ViewCard, Title: "child view", Body: "view-body"},
	}
	assertTrace := func(t *testing.T, tr trace, want spi.Result) {
		t.Helper()
		if tr.OK {
			t.Error("失败步骤的 OK 必须为 false")
		}
		if !tr.IsError {
			t.Error("失败步骤的 isError 必须为 true")
		}
		if tr.Brief != "actual-body" || tr.Text != want.Text {
			t.Errorf("Brief/Text 必须保留真实正文：Brief=%q Text=%q", tr.Brief, tr.Text)
		}
		if tr.Detail != nestedErr.Error() {
			t.Errorf("Detail 必须保留错误原文：%q", tr.Detail)
		}
		if !reflect.DeepEqual(tr.Data, want.Data) {
			t.Errorf("trace.Data 丢失或被改写：\n got: %#v\nwant: %#v", tr.Data, want.Data)
		}
		if !reflect.DeepEqual(tr.Artifacts, want.Artifacts) {
			t.Errorf("trace.Artifacts 丢失或被改写：\n got: %#v\nwant: %#v", tr.Artifacts, want.Artifacts)
		}
		if !reflect.DeepEqual(tr.View, want.View) {
			t.Errorf("trace.View 丢失或被改写：\n got: %#v\nwant: %#v", tr.View, want.View)
		}
		if tr.Hint != want.Hint {
			t.Errorf("trace.Hint 丢失或被改写：got %q want %q", tr.Hint, want.Hint)
		}
	}
	newRunner := func() *fakeRunner {
		return &fakeRunner{reply: func(tool string, _ json.RawMessage) (spi.Result, error) {
			if tool == "boom" {
				return want, nestedErr
			}
			return spi.Result{Summary: "after ok", Data: map[string]any{"after": true}}, nil
		}}
	}

	t.Run("trace", func(t *testing.T) {
		tr, val, err := runOne(context.Background(), newRunner(), 0,
			step{Tool: "boom"}, nil, nil)
		if err != nil {
			t.Fatalf("普通单步失败不得成为 runOne 的 hardErr：%v", err)
		}
		assertTrace(t, tr, want)
		if !reflect.DeepEqual(val, want.Data) {
			t.Errorf("失败结果仍应可供后续步骤引用：\n got: %#v\nwant: %#v", val, want.Data)
		}
		raw, marshalErr := json.Marshal(tr)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if !strings.Contains(string(raw), "actual-body") || !strings.Contains(string(raw), `"isError":true`) {
			t.Errorf("序列化 trace 必须含真实正文和 isError：%s", raw)
		}
	})

	t.Run("result", func(t *testing.T) {
		f := newRunner()
		res, err := run(t, ctxWith(f), scriptArg(t, `[{"tool":"boom"},{"tool":"after"}]`))
		if err != nil {
			t.Fatalf("普通单步失败不得成为整次 workflow error：%v", err)
		}
		if !reflect.DeepEqual(f.calls, []string{"boom", "after"}) {
			t.Fatalf("普通失败后必须继续后续步骤：%v", f.calls)
		}
		if !strings.Contains(res.Text, "actual-body") || !strings.Contains(res.Text, "after [ok]") {
			t.Fatalf("结果 Text 必须含真实正文和后续成功步骤：%s", res.Text)
		}
		m, ok := res.Data.(map[string]any)
		if !ok {
			t.Fatalf("结果 Data 形状不对：%T", res.Data)
		}
		if m["ok"] != false || m["failed"] != 1 {
			t.Fatalf("普通失败必须计入 Data.ok=false / failed=1：%v", m)
		}
		steps, ok := m["steps"].([]trace)
		if !ok || len(steps) != 2 {
			t.Fatalf("必须保留失败与后续两步 trace：%#v", m["steps"])
		}
		assertTrace(t, steps[0], want)
		if !steps[1].OK || steps[1].IsError {
			t.Fatalf("后续步骤必须成功且不误标 isError：%+v", steps[1])
		}
	})

	t.Run("empty hint", func(t *testing.T) {
		withoutHint := want
		withoutHint.Hint = ""
		f := &fakeRunner{reply: func(string, json.RawMessage) (spi.Result, error) {
			return withoutHint, nestedErr
		}}
		tr, _, err := runOne(context.Background(), f, 0, step{Tool: "boom"}, nil, nil)
		if err != nil {
			t.Fatalf("空 Hint 的普通失败同样不得成为 hardErr：%v", err)
		}
		assertTrace(t, tr, withoutHint)
		if tr.Hint != "" {
			t.Fatalf("trace 不得伪造子调用 Hint：%q", tr.Hint)
		}
	})
}

// 嵌套失败 trace 里的原始正文必须**有界**：一次 workflow 可能跑几十步，
// 每步都把整段正文抄进 Data.steps 会把结果撑爆（spill 阈值 4000）。
// 但"有界"不等于"换成 err.Error()"——开头那段真实正文必须还在。
func TestNestedFailureBoundsTraceText(t *testing.T) {
	nestedErr := errors.New("nested failed")
	huge := "actual-body\n" + strings.Repeat("x", maxTextRunes*2)
	f := &fakeRunner{reply: func(string, json.RawMessage) (spi.Result, error) {
		return spi.Result{
			Text:    huge,
			Data:    map[string]any{"kept": true},
			Hint:    "child hint",
			View:    &spi.ViewSpec{Kind: spi.ViewPlain, Body: "view-body"},
			Summary: "child summary",
		}, nestedErr
	}}

	tr, _, err := runOne(context.Background(), f, 0, step{Tool: "boom"}, nil, nil)
	if err != nil {
		t.Fatalf("普通单步失败不得成为 runOne 的 hardErr：%v", err)
	}
	if !tr.IsError {
		t.Fatal("失败步骤的 isError 必须为 true")
	}
	if !strings.HasPrefix(tr.Text, "actual-body") {
		t.Fatalf("trace 必须以真实正文开头，不能只剩错误串：%.40q", tr.Text)
	}
	if len([]rune(tr.Text)) > maxTextRunes+1 {
		t.Fatalf("trace.Text 必须有界：%d rune > %d", len([]rune(tr.Text)), maxTextRunes+1)
	}
	if tr.Detail != nestedErr.Error() {
		t.Fatalf("Detail 必须保留错误原文：%q", tr.Detail)
	}
	if !reflect.DeepEqual(tr.Data, map[string]any{"kept": true}) {
		t.Fatalf("Data 丢失：%#v", tr.Data)
	}
	if tr.Hint != "child hint" || tr.View == nil || tr.View.Body != "view-body" {
		t.Fatalf("Hint/View 丢失：%+v", tr)
	}
	// Brief 仍是一行摘要，且不是错误串——模型先看到"哪一步、吐了什么"。
	if strings.Contains(tr.Brief, nestedErr.Error()) || tr.Brief != "actual-body" {
		t.Fatalf("Brief 该是真实正文首行：%q", tr.Brief)
	}
}

// 没有编排通道时必须明确拒绝，并告诉模型改用普通工具。
func TestNoSubDispatchIsRefused(t *testing.T) {
	res, err := run(t, context.Background(), scriptArg(t, `[{"tool":"bash"}]`))
	if err == nil {
		t.Fatal("没有通道时必须报错")
	}
	if !strings.Contains(res.Hint, "SubDispatch") && !strings.Contains(res.Hint, "没有编排通道") {
		t.Fatalf("Hint 要说清为什么不行：%q", res.Hint)
	}
}

// script 不是合法 JSON 步骤表时，Hint 要指出**参数名对、语法不对**，
// 否则模型会以为是自己拼错了参数名。
func TestBadScriptExplainsSyntax(t *testing.T) {
	_, err := run(t, ctxWith(&fakeRunner{}), scriptArg(t, `not json`))
	if err == nil {
		t.Fatal("坏 script 必须报错")
	}
}

// 嵌套上限：workflow 里再套 workflow 要被拦住，别把栈打穿。
func TestDepthLimit(t *testing.T) {
	ctx := spi.WithSubDispatch(context.Background(),
		spi.SubDispatch{Runner: &fakeRunner{}, Depth: spi.MaxSubDepth})
	if _, err := run(t, ctx, scriptArg(t, `[{"tool":"bash"}]`)); err == nil {
		t.Fatal("超过嵌套上限必须报错")
	}
}

// 输出必须有界：步数一多，正文不能无限长。
func TestTextIsBounded(t *testing.T) {
	var steps []map[string]any
	for i := 0; i < 300; i++ {
		steps = append(steps, map[string]any{"tool": "t" + strings.Repeat("x", 40)})
	}
	stepsJSON, _ := json.Marshal(steps)
	res, err := run(t, ctxWith(&fakeRunner{}), scriptArg(t, string(stepsJSON)))
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(res.Text)); n > maxTextRunes+40 {
		t.Fatalf("正文 %d 字，超过上限 %d", n, maxTextRunes)
	}
}

// foreach：对数组逐项执行。
func TestForeachRunsPerItem(t *testing.T) {
	f := &fakeRunner{reply: func(string, json.RawMessage) (spi.Result, error) {
		return spi.Result{Summary: "ok", Data: []any{"a", "b", "c"}}, nil
	}}
	res, err := run(t, ctxWith(f), scriptArg(t, `[{"tool":"list","save":"items"},
		{"tool":"one","foreach":"${items}"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 4 { // list + 3 次 one
		t.Fatalf("foreach 应当执行 3 次，总调用 %d：%v", len(f.calls), f.calls)
	}
	_ = res
}

// Caps：编排工具自己不写盘、不起进程——真正干活的是子调用。
func TestCapsDeclareNothing(t *testing.T) {
	c := (&Tool{}).Caps()
	if c.Writes || c.Executes || c.Destructive {
		t.Fatalf("workflow 自己不碰盘不起进程，Caps 应为空：%+v", c)
	}
}

// meta 必须原样回显：调用方拿它跟自己的任务批次对账，
// 不然一次编排的结果回来，谁也不知道它对应哪一次请求。
func TestMetaEchoed(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"script": `[{"tool":"repo_here"}]`,
		"meta":   map[string]any{"batch": "B-7", "by": "lead"},
	})
	res, err := (&Tool{}).Exec(ctxWith(&fakeRunner{}), body)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := res.Data.(map[string]any)
	got, ok := m["meta"].(map[string]any)
	if !ok || got["batch"] != "B-7" {
		t.Fatalf("meta 没回显：%v", m["meta"])
	}
}

// 每步要带时长：编排慢的时候，人第一个问题就是"哪一步慢"。
func TestStepCarriesDuration(t *testing.T) {
	res, err := (&Tool{}).Exec(ctxWith(&fakeRunner{}),
		json.RawMessage(`{"script":"[{\"tool\":\"slow\"}]"}`))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := res.Data.(map[string]any)
	steps, _ := m["steps"].([]trace)
	if len(steps) != 1 {
		t.Fatalf("步数不对：%v", m["steps"])
	}
	if steps[0].MS < 0 {
		t.Fatalf("时长不能为负：%d", steps[0].MS)
	}
}

// 子调用要求停（exit_plan_mode / ask_user_question），整段编排就必须停。
//
// 吞掉它的后果不是"少停一次"，而是**计划模式当场失效**：
// 方案交上去了，模型却接着说"好的我这就去改"。
func TestStopFromSubCallPropagates(t *testing.T) {
	f := &fakeRunner{reply: func(tool string, _ json.RawMessage) (spi.Result, error) {
		if tool == "exit_plan_mode" {
			return spi.Result{Summary: "方案已提交", Text: "## 方案", Stop: true}, nil
		}
		return spi.Result{Summary: "ok"}, nil
	}}
	res, err := run(t, ctxWith(f), scriptArg(t,
		`[{"tool":"exit_plan_mode"},{"tool":"write"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stop {
		t.Fatal("子调用要求停，编排结果必须也是 Stop——否则模型会越过人批准那一步")
	}
}

// 没有子调用要求停时，不许乱停。
func TestNoStopWhenNoSubCallStops(t *testing.T) {
	res, err := run(t, ctxWith(&fakeRunner{}), scriptArg(t, `[{"tool":"glob"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop {
		t.Fatal("没人要求停，不许自己停")
	}
}

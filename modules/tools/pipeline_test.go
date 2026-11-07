package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"dshgo/internal/agent"
	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

var errTestBoom = errors.New("炸了")

type stubTool struct {
	name string
	res  spi.Result
	err  error
	ran  *bool
}

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return "stub" }

func (s *stubTool) Caps() spi.Caps          { return spi.Caps{} }
func (s *stubTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (s *stubTool) Exec(context.Context, json.RawMessage) (spi.Result, error) {
	if s.ran != nil {
		*s.ran = true
	}
	return s.res, s.err
}

func newPipe(t *testing.T, opts Options, list ...spi.Tool) (*Pipeline, *kernel.Kernel) {
	t.Helper()
	k := kernel.New("t")
	reg := New()
	for _, tl := range list {
		if err := reg.Register(tl); err != nil {
			t.Fatal(err)
		}
	}
	return NewPipeline(reg, k.Bus(), opts), k
}

func TestUnknownToolHasHint(t *testing.T) {
	p, k := newPipe(t, Options{})
	defer k.Root().Dispose()
	res, err := p.Run(context.Background(), "nope", nil)
	if err == nil {
		t.Fatal("未知工具必须报错")
	}
	if res.Hint == "" {
		t.Fatal("失败必须带 Hint（R3）")
	}
}

func TestPreExecuteCanVeto(t *testing.T) {
	ran := false
	tl := &stubTool{name: "x", ran: &ran, res: spi.Result{Summary: "不该跑到这里"}}
	p, k := newPipe(t, Options{}, tl)
	defer k.Root().Dispose()

	kernel.OnTyped(k.Bus(), k.Root(), spi.EventToolPreExecute, func(*spi.Call) error {
		return errors.New("沙箱拒绝")
	})
	res, err := p.Run(context.Background(), "x", nil)
	if err == nil || !strings.Contains(err.Error(), "沙箱拒绝") {
		t.Fatalf("veto 应中断并返回原因，得到 %v", err)
	}
	if ran {
		t.Fatal("被 veto 的工具不应执行")
	}
	if !strings.Contains(res.Hint, "策略拒绝") {
		t.Fatalf("veto 后的 Hint 不可行动: %q", res.Hint)
	}
}

func TestMissingHintIsPatchedAndRecorded(t *testing.T) {
	tl := &stubTool{name: "bad", err: errors.New("炸了")}
	p, k := newPipe(t, Options{}, tl)
	defer k.Root().Dispose()

	res, err := p.Run(context.Background(), "bad", nil)
	if err == nil {
		t.Fatal("应返回错误")
	}
	if res.Hint == "" {
		t.Fatal("流水线必须兜底 Hint")
	}
	v := p.Violations()
	if len(v) != 1 || v[0] != "bad" {
		t.Fatalf("R3 违规未记账: %v", v)
	}
}

func TestLargeDataIsSpilled(t *testing.T) {
	big := strings.Repeat("x", 500)
	tl := &stubTool{name: "big", res: spi.Result{Summary: "大结果", Data: map[string]any{"body": big}}}
	// 阈值要够装下"指针体"（含 artifact 路径）。64 字节连路径都放不下，
	// 那条"替换体不许超阈值"的规则在物理上就不可能满足。
	p, k := newPipe(t, Options{MaxChars: 400}, tl)
	defer k.Root().Dispose()

	res, err := p.Run(context.Background(), "big", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated == nil {
		t.Fatal("超阈值必须标记 Truncated")
	}
	if len(res.Artifacts) != 1 {
		t.Fatalf("应有 1 个 artifact，得到 %d", len(res.Artifacts))
	}
	m, ok := res.Data.(map[string]any)
	if !ok || m["spilled"] != true {
		t.Fatalf("Data 应替换成指针体，得到 %#v", res.Data)
	}
	if res.Hint == "" || !strings.Contains(res.Hint, "read") {
		t.Fatalf("外置后必须告诉模型怎么取回，得到 %q", res.Hint)
	}

	// 原始全文必须完整落盘。
	raw, err := os.ReadFile(res.Truncated.Pointer)
	if err != nil {
		t.Fatalf("外部文件不存在: %v", err)
	}
	if !strings.Contains(string(raw), big) {
		t.Fatal("外置文件里不是完整原文")
	}
	// 替换体本身必须小于阈值，否则取回又会被外置。
	repl, _ := json.Marshal(res.Data)
	if len(repl) > p.opts.MaxChars {
		t.Fatalf("替换体 %d 字节超过阈值 %d", len(repl), p.opts.MaxChars)
	}
}

func TestSmallDataStaysInline(t *testing.T) {
	tl := &stubTool{name: "small", res: spi.Result{Summary: "小", Data: map[string]any{"n": 1}}}
	p, k := newPipe(t, Options{MaxChars: 4000}, tl)
	defer k.Root().Dispose()

	res, err := p.Run(context.Background(), "small", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated != nil || len(res.Artifacts) != 0 {
		t.Fatal("小结果不该外置")
	}
}

func TestPostExecuteCanRewrite(t *testing.T) {
	tl := &stubTool{name: "x", res: spi.Result{Summary: "原始"}}
	p, k := newPipe(t, Options{}, tl)
	defer k.Root().Dispose()

	kernel.OnTyped(k.Bus(), k.Root(), spi.EventToolPostExecute, func(c *spi.Call) error {
		c.Result.Summary = "已脱敏"
		return nil
	})
	res, err := p.Run(context.Background(), "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary != "已脱敏" {
		t.Fatalf("post-execute 的改写没生效: %q", res.Summary)
	}
}

// 工具成功但什么也没返回：模型不能只看到一行「（无输出）」然后自己猜。
//
// 这是「有产出但产出里没有载荷」的工具版：err == nil、日志里是 ok、退出码 0，
// 但模型拿到的是一句没有任何信息的话——它不知道该不该相信这次调用真的做了什么，
// 下一步只能瞎试。流水线要补一句可执行的话（R3 的精神：失败要指路，
// "成功但空"同样要指路）。
func TestEmptySuccessGetsNoticeAndHint(t *testing.T) {
	p, k := newPipe(t, Options{},
		&stubTool{name: "silent"},
		// 空数组是**有效载荷**（"集合是空的"本身就是答案），不算空结果。
		&stubTool{name: "empty_list", res: spi.Result{Data: []string{}}},
		// View 只给客户端画卡片，模型看不到：只返回 View 等于对模型没有产出。
		&stubTool{name: "view_only", res: spi.Result{View: &spi.ViewSpec{Kind: spi.ViewCard, Title: "卡片"}}},
	)
	defer k.Root().Dispose()

	res, err := p.Run(context.Background(), "silent", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary == "" || !strings.Contains(res.Summary, "没有返回任何模型可见的输出") {
		t.Fatalf("空结果必须有一句人话，得到 Summary=%q", res.Summary)
	}
	if res.Hint == "" || !strings.Contains(res.Hint, "核实") {
		t.Fatalf("要告诉模型下一步（别假设它做了事），得到 Hint=%q", res.Hint)
	}

	res, err = p.Run(context.Background(), "empty_list", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Summary, "模型可见的输出") {
		t.Fatalf("Data=[] 是有效载荷，不许当成空结果：%q", res.Summary)
	}

	res, err = p.Run(context.Background(), "view_only", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Summary, "模型可见的输出") {
		t.Fatalf("只返回 View 的结果对模型是空的，必须补一句：%q", res.Summary)
	}
}

// 调用工具表里没有的名字：措辞必须和 DSH 一致（真实语料里这是最常见的失败类，
// 367 个失败样本里 84 个是 `Error: unknown tool "bash"`）。
func TestUnknownToolWording(t *testing.T) {
	p, _ := newPipe(t, Options{}, &stubTool{name: "read"})
	_, err := p.Run(context.Background(), "no_such_tool", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("不存在的工具必须报错")
	}
	if err.Error() != `unknown tool "no_such_tool"` {
		t.Fatalf("措辞不一致: %q", err.Error())
	}
}

// 参数不是 JSON 对象时，**在执行前**就拦下。
//
// 让每个工具自己 Unmarshal 也行，但不校验参数的工具会拿着垃圾照跑。
func TestPipelineRejectsNonJSONArgs(t *testing.T) {
	ran := false
	p, k := newPipe(t, Options{}, &stubTool{name: "loose", ran: &ran})
	defer k.Root().Dispose()

	res, err := p.Run(context.Background(), "loose", json.RawMessage(`"{not json"`))
	if err == nil {
		t.Fatal("非 JSON 参数必须报错")
	}
	if ran {
		t.Fatal("参数不合法时不该执行工具")
	}
	if !strings.Contains(res.Hint, "JSON") {
		t.Fatalf("Hint 要说清是 JSON 的问题: %q", res.Hint)
	}
}

// JSON null 能 unmarshal 进 map 却留下 nil map：判据必须单独拒绝它，
// 否则同一个 `null` 参数在 agent 循环（拒绝）和流水线（放行）得到两种结论。
func TestPipelineRejectsNullArgs(t *testing.T) {
	ran := false
	p, k := newPipe(t, Options{}, &stubTool{name: "loose", ran: &ran})
	defer k.Root().Dispose()

	res, err := p.Run(context.Background(), "loose", json.RawMessage(`null`))
	if err == nil {
		t.Fatal("null 参数必须报错")
	}
	if ran {
		t.Fatal("参数不合法时不该执行工具")
	}
	if !strings.Contains(res.Hint, "JSON") {
		t.Fatalf("Hint 要说清是 JSON 的问题: %q", res.Hint)
	}
	if !strings.Contains(err.Error(), "null") {
		t.Fatalf("错误要说清是 null: %v", err)
	}
}

// 判据只有一份：流水线直接调 agent.ValidateToolCallArguments。
// 这条表把"什么算 malformed"钉死，防两层再次各写一套。
func TestValidateToolCallArgumentsSingleCriterion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    string
		wantErr bool
	}{
		{name: "object", args: `{}`},
		{name: "object with fields", args: `{"file_path":"a.txt","n":1}`},
		{name: "nested", args: `{"a":{"b":[1,2]}}`},
		// 没有参数不是"参数坏了"：零参数工具的 tool call 就是空串。
		{name: "empty", args: ``},
		{name: "whitespace", args: "  \n\t "},
		{name: "null", args: `null`, wantErr: true},
		{name: "array", args: `[1,2]`, wantErr: true},
		{name: "string", args: `"{\"a\":1}"`, wantErr: true},
		{name: "number", args: `1`, wantErr: true},
		{name: "truncated", args: `{"file_path": "{not json"`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := agent.ValidateToolCallArguments(json.RawMessage(tc.args))
			if tc.wantErr && err == nil {
				t.Fatalf("%q 必须判成 malformed", tc.args)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("%q 是合法参数，却被判成 malformed: %v", tc.args, err)
			}
		})
	}
}

// 每次工具调用都要在日志里留一条：名字、耗时、成败、错误原文。
// 出事之后人要能回答"它到底跑了什么、跑了多久、成没成"。
func TestPipelineLogsEveryToolCall(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	ran := false
	p, k := newPipe(t, Options{},
		&stubTool{name: "ok_tool", ran: &ran, res: spi.Result{Summary: "fine"}},
		&stubTool{name: "bad_tool", err: errTestBoom, res: spi.Result{Hint: "换个办法"}},
	)
	defer k.Root().Dispose()

	if _, err := p.Run(context.Background(), "ok_tool", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(context.Background(), "bad_tool", json.RawMessage(`{}`)); err == nil {
		t.Fatal("bad_tool 应该失败")
	}

	var okRec, errRec map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		switch rec["tool"] {
		case "ok_tool":
			okRec = rec
		case "bad_tool":
			errRec = rec
		}
	}
	if okRec == nil || errRec == nil {
		t.Fatalf("两次调用都该有日志，实际:\n%s", buf.String())
	}
	if okRec["ok"] != true || errRec["ok"] != false {
		t.Fatalf("成败要写清: ok=%v err=%v", okRec["ok"], errRec["ok"])
	}
	if _, has := okRec["ms"]; !has {
		t.Fatal("要记耗时")
	}
	if msg, _ := errRec["msg"].(string); !strings.Contains(msg, "失败") {
		t.Fatalf("失败那条的 msg 要说失败: %q", msg)
	}
	if e, _ := errRec["error"].(string); e == "" {
		t.Fatal("失败那条要带错误原文")
	}
}

// ---------- 统一 per-call 截止时间（timeout-policy） ----------

// hangTool 是一个**挂死**的假工具：它不看 ctx，只在 release 关闭后才返回。
//
// 用它证明两件事：①流水线到点会自己返回，而不是陪着工具一直等；②超时之后工具
// 晚到的结果绝不会被当成这次调用的成功结果（真工具里对应卡在无 deadline 的读上）。
type hangTool struct {
	name     string
	release  chan struct{}
	started  chan struct{} // Exec 进入时关闭
	finished chan struct{} // Exec 真正返回时关闭
	ctxDone  chan struct{} // Exec 收到的 ctx 结束时关闭
}

func (h *hangTool) Name() string            { return h.name }
func (h *hangTool) Description() string     { return "挂死的假工具" }
func (h *hangTool) Caps() spi.Caps          { return spi.Caps{} }
func (h *hangTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (h *hangTool) Exec(ctx context.Context, _ json.RawMessage) (spi.Result, error) {
	if h.started != nil {
		close(h.started)
	}
	if h.ctxDone != nil {
		go func() {
			<-ctx.Done()
			close(h.ctxDone)
		}()
	}
	<-h.release
	if h.finished != nil {
		close(h.finished)
	}
	return spi.Result{Summary: "假成功：挂死的工具自己跑完了"}, nil
}

// slowTool 是"慢但正常"的工具：不看 ctx，睡够了就返回成功。
//
// 它用来证明统一 deadline **没有**落到不该落的地方（预算内完成 = 原样成功）。
type slowTool struct {
	name string
	d    time.Duration
	res  spi.Result
}

func (s *slowTool) Name() string            { return s.name }
func (s *slowTool) Description() string     { return "慢但正常" }
func (s *slowTool) Caps() spi.Caps          { return spi.Caps{} }
func (s *slowTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (s *slowTool) Exec(context.Context, json.RawMessage) (spi.Result, error) {
	time.Sleep(s.d)
	return s.res, nil
}

// ctxTool 是"听 ctx 的"工具：等到 ctx 结束，把它原样返回。
type ctxTool struct {
	started chan struct{}
}

func (c *ctxTool) Name() string            { return "waits" }
func (c *ctxTool) Description() string     { return "等 ctx" }
func (c *ctxTool) Caps() spi.Caps          { return spi.Caps{} }
func (c *ctxTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (c *ctxTool) Exec(ctx context.Context, _ json.RawMessage) (spi.Result, error) {
	close(c.started)
	<-ctx.Done()
	return spi.Result{}, ctx.Err()
}

// panicTool 是"会炸"的工具：Exec 里直接 panic。
type panicTool struct{ name string }

func (p *panicTool) Name() string            { return p.name }
func (p *panicTool) Description() string     { return "会炸" }
func (p *panicTool) Caps() spi.Caps          { return spi.Caps{} }
func (p *panicTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p *panicTool) Exec(context.Context, json.RawMessage) (spi.Result, error) {
	panic("boom")
}

// 挂死的工具必须被统一截止时间掐断，而不是把整轮拖死。
//
// 红：旧实现（execute 阶段直接调 tool.Exec，没有任何 deadline）会让 Run 一直等
// 下去——这条测试在受控的短等待（2 秒）后判定"它还在等"并失败，而不是真挂 10 分钟。
func TestHungToolIsCutByDeadline(t *testing.T) {
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	tl := &hangTool{
		name:     "hang",
		release:  release,
		started:  make(chan struct{}),
		finished: make(chan struct{}),
	}
	budget := 100 * time.Millisecond
	p, k := newPipe(t, Options{ToolTimeout: budget}, tl)
	defer k.Root().Dispose()

	done := make(chan struct{})
	var res spi.Result
	var err error
	go func() {
		res, err = p.Run(context.Background(), "hang", json.RawMessage(`{}`))
		close(done)
	}()

	<-tl.started
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("挂死的工具没有返回：流水线没有统一截止时间，这一轮会被它拖死")
	}

	if err == nil {
		t.Fatal("超时必须报错，不能把超时写成成功")
	}
	var te *ToolTimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("超时必须是可辨认的 *ToolTimeoutError，得到 %T: %v", err, err)
	}
	if te.Tool != "hang" || te.Budget != budget {
		t.Fatalf("错误里要写清哪个工具、多大预算，得到 %+v", te)
	}
	// 超时不是取消，也不是"上游把这一轮掐了"：agent 循环对这两类有不同文案，
	// 混进来模型会以为有人停了它。
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时不该被认成取消/打断：%v", err)
	}

	// 模型可见的结构化错误：一句人话 + 事实 + 下一步。
	if res.Hint == "" || !strings.Contains(res.Hint, budget.String()) {
		t.Fatalf("Hint 要说清等了多久并给下一步，得到 %q", res.Hint)
	}
	if res.Summary == "" {
		t.Fatal("Summary 要给人一句能读的话")
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("超时要给结构化 Data，得到 %#v", res.Data)
	}
	if data["code"] != "tool_timeout" || data["tool"] != "hang" {
		t.Fatalf("结构化错误字段不对：%#v", data)
	}
	if data["timeout_ms"] != budget.Milliseconds() {
		t.Fatalf("timeout_ms 应为 %d，得到 %#v", budget.Milliseconds(), data["timeout_ms"])
	}
	// Hint 非空就不是 R3 违规：别把"超时"记成"工具作者没写 Hint"。
	if v := p.Violations(); len(v) != 0 {
		t.Fatalf("超时有 Hint，不该记 R3 违规：%v", v)
	}

	// 超时的时候工具**还在跑**：这就是"到点不再等"。
	select {
	case <-tl.finished:
		t.Fatal("工具已经返回，这不是超时场景")
	default:
	}
	// 工具晚到的结果不许变成这次调用的结果。
	close(release)
	released = true
	<-tl.finished
	if strings.Contains(res.Summary+res.Hint, "假成功") {
		t.Fatalf("超时之后工具吐的结果被写成了成功：%+v", res)
	}
	if err == nil || !errors.As(err, &te) {
		t.Fatal("工具晚到不影响已经返回的超时结论")
	}
}

// Exec 拿到的必须是**可取消**的 ctx：超时之后工具自己能看到取消信号。
//
// 只有这样才能让"听 ctx 的"工具真的停下来，而不是光靠流水线丢下它不管。
func TestTimeoutCancelsExecContext(t *testing.T) {
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	tl := &hangTool{
		name:    "hang",
		release: release,
		started: make(chan struct{}),
		ctxDone: make(chan struct{}),
	}
	p, k := newPipe(t, Options{ToolTimeout: 80 * time.Millisecond}, tl)
	defer k.Root().Dispose()

	if _, err := p.Run(context.Background(), "hang", json.RawMessage(`{}`)); err == nil {
		t.Fatal("应超时")
	}
	select {
	case <-tl.ctxDone:
	case <-time.After(2 * time.Second):
		t.Fatal("超时之后 Exec 的 ctx 没有结束：工具收不到取消信号")
	}
	close(release)
	released = true
}

// 用户取消 ≠ 超时：两条路都要能认出来，而且取消也不能陪着工具一直等。
func TestCancelIsNotReportedAsTimeout(t *testing.T) {
	tl := &ctxTool{started: make(chan struct{})}
	p, k := newPipe(t, Options{ToolTimeout: 10 * time.Second}, tl)
	defer k.Root().Dispose()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	var res spi.Result
	var err error
	go func() {
		res, err = p.Run(ctx, "waits", json.RawMessage(`{}`))
		close(done)
	}()

	<-tl.started
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消之后流水线还在等工具：取消要立刻归还")
	}

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消必须原样报成 context.Canceled，得到 %T: %v", err, err)
	}
	var te *ToolTimeoutError
	if errors.As(err, &te) {
		t.Fatalf("取消被报成了超时：%+v", te)
	}
	if res.Hint == "" {
		t.Fatal("取消也要给 Hint：结果未知时模型要知道先核实现场")
	}
}

// 预算内完成的调用不受影响：结果、错误、Hint 原样透传。
func TestCompletedCallIsUntouched(t *testing.T) {
	want := spi.Result{Summary: "正常完成", Data: map[string]any{"n": 1}, Hint: "原样提示"}
	tl := &slowTool{name: "ok", d: 5 * time.Millisecond, res: want}
	p, k := newPipe(t, Options{ToolTimeout: 5 * time.Second}, tl)
	defer k.Root().Dispose()

	res, err := p.Run(context.Background(), "ok", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("预算内完成不该报错：%v", err)
	}
	if res.Summary != want.Summary || res.Hint != want.Hint {
		t.Fatalf("结果被改动了：%+v", res)
	}
	if len(p.Violations()) != 0 {
		t.Fatalf("成功不该记违规：%v", p.Violations())
	}
}

// 一个调用超时，不许把下一个调用也带下水（deadline 是每次调用各一份）。
func TestDeadlineDoesNotLeakToNextCall(t *testing.T) {
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	hang := &hangTool{name: "hang", release: release, started: make(chan struct{})}
	ok := &slowTool{name: "ok", d: 60 * time.Millisecond, res: spi.Result{Summary: "后面这次成功"}}
	p, k := newPipe(t, Options{ToolTimeout: 100 * time.Millisecond}, hang, ok)
	defer k.Root().Dispose()

	if _, err := p.Run(context.Background(), "hang", json.RawMessage(`{}`)); err == nil {
		t.Fatal("第一次调用应超时")
	}
	res, err := p.Run(context.Background(), "ok", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("上一次调用的超时串到了下一次：%v", err)
	}
	if res.Summary != "后面这次成功" {
		t.Fatalf("第二次调用结果不对：%+v", res)
	}
	close(release)
	released = true
}

// 并发调用各自的窗口互不串：一个挂着超时，另一个照常在预算内成功。
func TestConcurrentCallsHaveIndependentDeadlines(t *testing.T) {
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	hang := &hangTool{name: "hang", release: release, started: make(chan struct{})}
	ok := &slowTool{name: "ok", d: 20 * time.Millisecond, res: spi.Result{Summary: "并发这次成功"}}
	p, k := newPipe(t, Options{ToolTimeout: 300 * time.Millisecond}, hang, ok)
	defer k.Root().Dispose()

	type outcome struct {
		res spi.Result
		err error
	}
	hangOut := make(chan outcome, 1)
	okOut := make(chan outcome, 1)
	go func() {
		r, e := p.Run(context.Background(), "hang", json.RawMessage(`{}`))
		hangOut <- outcome{r, e}
	}()
	go func() {
		r, e := p.Run(context.Background(), "ok", json.RawMessage(`{}`))
		okOut <- outcome{r, e}
	}()

	select {
	case o := <-okOut:
		if o.err != nil || o.res.Summary != "并发这次成功" {
			t.Fatalf("另一个调用的超时串了过来：%+v err=%v", o.res, o.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("正常的那个调用没有返回")
	}

	select {
	case o := <-hangOut:
		var te *ToolTimeoutError
		if !errors.As(o.err, &te) {
			t.Fatalf("挂死的那个应超时，得到 %T: %v", o.err, o.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("挂死的调用没有在预算附近返回")
	}
	close(release)
	released = true
}

// 显式关闭（负值）：行为回到"只看各工具自己"，慢活照常跑完。
func TestToolTimeoutCanBeDisabled(t *testing.T) {
	tl := &slowTool{name: "slow", d: 80 * time.Millisecond, res: spi.Result{Summary: "慢慢跑完"}}
	p, k := newPipe(t, Options{ToolTimeout: -1}, tl)
	defer k.Root().Dispose()

	res, err := p.Run(context.Background(), "slow", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("关闭超时后不该被掐：%v", err)
	}
	if res.Summary != "慢慢跑完" {
		t.Fatalf("结果不对：%+v", res)
	}
}

// 默认预算必须存在，而且**比各工具自带的最长超时更宽**——否则这一层就是
// 在偷偷改各工具的超时语义（bash 的 idle 上限 15 分钟会永远跑不到）。
func TestDefaultToolTimeoutDoesNotShadowToolBudgets(t *testing.T) {
	t.Setenv(toolTimeoutEnv, "")
	tl := &slowTool{name: "ok", res: spi.Result{Summary: "ok"}}
	p, k := newPipe(t, Options{}, tl)
	defer k.Root().Dispose()

	if p.opts.ToolTimeout != defaultToolTimeout {
		t.Fatalf("默认值没接上：%s", p.opts.ToolTimeout)
	}
	if p.opts.ToolTimeout < 15*time.Minute {
		t.Fatalf("统一预算 %s 比 bash 自带的 15 分钟上限还短，会改掉工具自己的超时语义", p.opts.ToolTimeout)
	}
}

// 环境变量是运维侧的旋钮：正数=预算、0=默认、负数=关闭、写错=默认。
func TestToolTimeoutEnvKnob(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		want time.Duration
	}{
		{name: "empty", env: "", want: defaultToolTimeout},
		{name: "zero", env: "0", want: defaultToolTimeout},
		{name: "seconds", env: "7", want: 7 * time.Second},
		{name: "off", env: "-1", want: -1},
		{name: "garbage", env: "abc", want: defaultToolTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(toolTimeoutEnv, tc.env)
			if got := (Options{}).withDefaults().ToolTimeout; got != tc.want {
				t.Fatalf("env=%q 期望 %s，得到 %s", tc.env, tc.want, got)
			}
		})
	}
}

// 工具 panic 不许把宿主带走：现在执行体跑在流水线的 goroutine 里，没有人替它
// recover——漏了这一步，一个 panic 就是整个进程退出，而不是一次失败的工具调用。
func TestToolPanicBecomesError(t *testing.T) {
	tl := &panicTool{name: "boom"}
	p, k := newPipe(t, Options{ToolTimeout: time.Second}, tl)
	defer k.Root().Dispose()

	res, err := p.Run(context.Background(), "boom", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("panic 必须转成错误")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Fatalf("要说明是 panic: %v", err)
	}
	if res.Hint == "" {
		t.Fatal("失败必须带 Hint（R3）")
	}
}

// 听 ctx 的工具在 deadline 上会**立刻**把 ctx.Err() 交回来——那不是"完成"，
// 而是被我们叫醒。同一次超时不许有时报"超时"、有时报"被打断/结果未知"
// （agent 循环对这两类的文案完全不同）。多跑几轮：两个 channel 同时就绪时
// select 是随机挑的，跑一轮有一半概率看不出问题。
func TestCtxAwareToolStillReportedAsTimeout(t *testing.T) {
	budget := 5 * time.Millisecond
	for i := 0; i < 30; i++ {
		tl := &ctxTool{started: make(chan struct{})}
		p, k := newPipe(t, Options{ToolTimeout: budget}, tl)
		res, err := p.Run(context.Background(), "waits", json.RawMessage(`{}`))
		var te *ToolTimeoutError
		ok := errors.As(err, &te)
		hint, data := res.Hint, res.Data
		k.Root().Dispose()
		if !ok {
			t.Fatalf("第 %d 轮：工具交回的 ctx.Err() 被当成结果了，超时报成 %T: %v", i, err, err)
		}
		if hint == "" || data == nil {
			t.Fatalf("第 %d 轮：超时结果必须是结构化的（Hint+Data）", i)
		}
	}
}

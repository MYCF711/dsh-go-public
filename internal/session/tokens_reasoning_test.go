package session

import (
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/spi"
)

// 这一组钉住 token 计量的口径：**思考正文也是模型真的占掉上下文的文本**。
//
// 为什么它比"显示不准"严重：Meter.Estimate 是自动压缩的唯一触发器
// （cmd/dshd 的 autoCompactNow、run 的 budget 检查）。Surface 只投影正文
// 与 tool_calls，思考块在投影那步被丢掉，估算因此**系统性偏低**——
// 于是压缩在真正该压之前就触发：多花一次摘要的钱，还把上下文提前丢掉。
//
// 口径（与 estimateString 同一份经验值，见 tokens.go）：
//   - 中文/其它非 ASCII：1 字 ≈ 1 token；
//   - ASCII：4 字节 ≈ 1 token；
//   - 每条 surface 节点 +4 结构开销（与内容块数无关）。
//
// 所以 10,000 字思考 ≈ 10,000 token，这是"改前/改后"该差出的量。

// buildMeterLog 造一份会话并读回来。body/reasoning 为空就不写对应的块，
// 于是"带思考"与"不带思考"两份日志**只差思考块**——差值就是思考正文的 token。
func buildMeterLog(t *testing.T, home, cwd, id, body, reasoning string, withTool bool) *v4.Log {
	t.Helper()
	l, err := CreateSession(home, cwd, id, "standard")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	must := func(ev string, d any) {
		t.Helper()
		if err := l.Append(ev, d); err != nil {
			t.Fatalf("写 %s 失败: %v", ev, err)
		}
	}
	must("user/message", UserMessage([]spi.Block{{Type: "text", Text: "hi"}}))

	blocks := make([]map[string]any, 0, 3)
	if reasoning != "" {
		blocks = append(blocks, ReasoningBlock(reasoning))
	}
	if body != "" {
		blocks = append(blocks, TextBlock(body))
	}
	finish := "stop"
	if withTool {
		blocks = append(blocks, ToolCallBlock("c1", "glob", `{"pattern":"*.go"}`))
		finish = "tool_calls"
	}
	must("assistant/message", AssistantMessageEvent(1, 1, blocks, finish, nil))
	if withTool {
		must("tool/call", ToolCallEvent(1, 1, "c1", "glob", `{"pattern":"*.go"}`))
		must("tool/result", ToolResultEvent(1, 1, "c1",
			[]spi.Block{{Type: "text", Text: strings.Repeat("r", 1000)}}, false, cwd, []int64{2}))
	}
	if err := l.Close(); err != nil {
		t.Fatalf("关会话失败: %v", err)
	}
	back, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatalf("读回来失败: %v", err)
	}
	return back
}

// TestMeterEstimateCountsReasoningText 是 B 的主判据：
// 同一份会话（正文 1,000 字）加一段 10,000 字思考，估算必须**多出思考正文那部分**。
func TestMeterEstimateCountsReasoningText(t *testing.T) {
	home := t.TempDir()
	body := strings.Repeat("正", 1000)   // 1,000 token
	think := strings.Repeat("想", 10000) // 10,000 token

	withR := buildMeterLog(t, home, "/tmp/tok-r", "tok-with", body, think, false)
	withoutR := buildMeterLog(t, home, "/tmp/tok-r", "tok-without", body, "", false)

	var m Meter
	base := m.Estimate(Surface(withoutR))
	got := m.Estimate(Surface(withR))

	// 改前 = base：旧代码只看正文，思考块在 Surface 投影里就不存在，
	// 所以"不带思考的那份日志"算出来的数正好是旧口径。
	wantBase := estimateString("hi") + 4 + estimateString(body) + 4
	if base != wantBase {
		t.Fatalf("不含思考时的估算 = %d，期望 %d（旧口径应当不变）", base, wantBase)
	}

	wantDelta := estimateString(think)
	if wantDelta != 10000 {
		t.Fatalf("测试假设不成立：10,000 个汉字应当估成 10,000 token，实际 %d", wantDelta)
	}
	if delta := got - base; delta != wantDelta {
		t.Fatalf("思考正文没有被算进估算：改后 - 改前 = %d，期望 %d（思考 %d 字）\n"+
			"改前 = %d、改后 = %d", delta, wantDelta, len([]rune(think)), base, got)
	}
	// 量级：正文 1,008 + 思考 10,000 ≈ 11,000，应当是改前（≈1,008）的十倍左右。
	if got < base*9 {
		t.Fatalf("估算量级不对：改后 = %d，改前 = %d（10k 字思考应当把总量抬高到 ~10 倍）", got, base)
	}
}

// TestMeterReasoningDoesNotDoubleCountToolResults 钉"每一段文本只算一次"：
// 期望值是按块逐项手算的——tool/result 的正文只算一次，assistant 的
// 工具名/参数只算一次，思考正文只算一次，没有哪一项被两遍加起来。
func TestMeterReasoningDoesNotDoubleCountToolResults(t *testing.T) {
	home := t.TempDir()
	body := strings.Repeat("正", 1000)
	think := strings.Repeat("想", 10000)
	const args = `{"pattern":"*.go"}`

	back := buildMeterLog(t, home, "/tmp/tok-t", "tok-tool", body, think, true)
	var m Meter
	got := m.Estimate(Surface(back))

	// surface 三个节点：user、assistant、tool/result（tool/call 是记账事件，不进 surface）。
	// 逐项手算 = AI 的块（思考→正文→调用）+ user 正文 + 工具结果正文 + 3 份结构开销。
	toolResultText := strings.Repeat("r", 1000)
	want := estimateString("hi") + 4 +
		estimateString(think) + estimateString(body) + estimateString("glob") + estimateString(args) + 4 +
		estimateString(toolResultText) + 4

	if got != want {
		t.Fatalf("估算 = %d，逐项手算 = %d：有项被算重/漏算（思考 %d、正文 %d、结果 %d）",
			got, want, estimateString(think), estimateString(body), estimateString(toolResultText))
	}

	// 反方向：把思考去掉，差值必须**只有**思考那部分（工具结果没有被重算一遍）。
	noR := buildMeterLog(t, home, "/tmp/tok-t2", "tok-tool-noreason", body, "", true)
	if delta := got - m.Estimate(Surface(noR)); delta != estimateString(think) {
		t.Fatalf("去掉思考后差值 = %d，期望 %d（多出来的部分不该跟着变化：工具结果被重复算了）",
			delta, estimateString(think))
	}
}

// TestSurfaceCarriesReasoningText 钉住估算的**来源**：思考正文挂在 surface 节点上，
// 所以"拿 []SurfaceMsg 算"的任何消费者（不只 Meter）都能看到它，不用改 llm.Message。
//
// 只加不改的边界也在这里：Message 的形状没动（仍是 role/content/tool_calls），
// 思考**没有**被拼进 Content——那会把模型的草稿当成"已经给用户的回答"。
func TestSurfaceCarriesReasoningText(t *testing.T) {
	home := t.TempDir()
	back := buildMeterLog(t, home, "/tmp/tok-s", "tok-surface", "", "只有想，没有正文的回答", false)

	nodes := Surface(back)
	var assistant *SurfaceMsg
	for i := range nodes {
		if nodes[i].Message.Role == "assistant" {
			assistant = &nodes[i]
		}
	}
	if assistant == nil {
		t.Fatal("surface 里没有 assistant 节点")
	}
	if got, want := strings.Join(assistant.Reasoning, "|"), "只有想，没有正文的回答"; got != want {
		t.Fatalf("SurfaceMsg.Reasoning = %q，期望 %q", got, want)
	}
	if assistant.Message.Content != "" {
		t.Fatalf("思考被拼进了正文：%q（那会把草稿当成回答）", assistant.Message.Content)
	}
	if assistant.Type != "assistant/message" {
		t.Fatalf("assistant 节点的 Type = %q", assistant.Type)
	}
	// 反方向：别的节点不该凭空带思考。
	for _, n := range nodes {
		if n.Message.Role != "assistant" && len(n.Reasoning) > 0 {
			t.Fatalf("%s 节点也带了思考：%v", n.Message.Role, n.Reasoning)
		}
	}
}

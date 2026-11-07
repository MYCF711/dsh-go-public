package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
	"dshgo/internal/spi"
	"dshgo/internal/transport"
)

// TestSessionSourceGolden 拿一个**真实会话**跑 sessionSource.Messages，钉住三件事：
//   - 至少产出 1 条消息，且 Seq 严格递增（transport 的分页依赖这个）
//   - Role 只可能是 user / assistant / tool
//   - 至少有一条 user 且正文非空（否则界面打开会话会是空白）
//
// 语料不在仓库里（只读引用 DSH_TEST_SESSION），CI 上没有就 Skip——
// 这不是"测不到"，是"语料不该进仓库"。
func TestSessionSourceGolden(t *testing.T) {
	path := os.Getenv("DSH_TEST_SESSION")
	if path == "" {
		t.Skip("没设 DSH_TEST_SESSION，跳过真实语料 golden（CI 上本来就没有这份会话）")
	}

	// 会话 id 与 cwd 都从文件自己的 header 里取：目录名是转义过的 segment，
	// 反解不是我们要验的东西，而 header 里两个值都是现成的。
	back, err := v4.ReadFile(path)
	if err != nil {
		t.Fatalf("读语料失败 %s: %v", path, err)
	}
	if back.Header.ID == "" {
		t.Fatalf("语料 header 没有 id：%s", path)
	}

	// <home>/sessions/<projectKey>/<segment>/session.v4.jsonl.zstd，往上四层就是 home。
	home := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(path))))
	if base := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path)))); base != "sessions" {
		t.Fatalf("语料路径不像 <home>/sessions/<project>/<segment>/session.v4.jsonl.zstd（倒数第三层是 %q）：%s",
			base, path)
	}

	src := newSessionSource(home, back.Header.Cwd)
	msgs, err := src.Messages(back.Header.ID)
	if err != nil {
		t.Fatalf("Messages(%q) 出错: %v", back.Header.ID, err)
	}
	if len(msgs) == 0 {
		// 有的会话只有 header 和控制类事件（没有 user/assistant/tool），映射出 0 条是对的。
		// 那种会话做 golden 什么也证明不了——换一个，不是让它红。
		t.Skipf("这个会话没有消息事件（%d 条事件），换一个有消息的会话", len(back.Events))
	}
	t.Logf("会话 %s：%d 条事件 → %d 条消息", back.Header.ID, len(back.Events), len(msgs))

	prev := -1
	sawUser := false
	for i, m := range msgs {
		if m.Seq <= prev {
			t.Fatalf("第 %d 条 Seq=%d 没有严格递增（上一条 %d）", i, m.Seq, prev)
		}
		prev = m.Seq

		switch m.Role {
		case "user":
			if m.Text != "" {
				sawUser = true
			}
		case "assistant", "tool":
			// 只要求 role 合法；正文可能为空（纯 tool-call 的 assistant 消息就是没有文字）。
		default:
			t.Fatalf("第 %d 条 Seq=%d 的 Role=%q 不是 user/assistant/tool", i, m.Seq, m.Role)
		}
	}
	if !sawUser {
		t.Fatal("没有任何一条 user 消息带正文——界面打开会话会是空白")
	}

	// 找不到的会话必须报哨兵值，界面靠它区分「会话没了」与「读失败」。
	if _, err := src.Messages("definitely-not-a-session-id"); !errors.Is(err, transport.ErrNoSuchSession) {
		t.Fatalf("不存在的会话应返回 ErrNoSuchSession，实际 %v", err)
	}
}

// B：session.load 的消息投影不能把"只有思考"的回答变成一条空消息。
//
// serve 推的 assistant/message 帧本来就带 reasoning（serve_turn.go 只在正文为空时
// 带它），可读回来时 transport.Message 没有 reasoning 字段——界面上就是一条空气泡，
// 而这次回答的全部内容都在思考块里。断了线重新 load 就等于把这条回答丢了。
//
// 断言按**线上 JSON** 判，不按结构体字段：旧代码没有 Reasoning 字段，
// 这条测试必须能在旧代码上红（而不是编译不过）。
func TestLoadKeepsReasoningOnlyAssistant(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	const sid = "session-load-reasoning"
	seedTurnSession(t, home, cwd, sid, func(lg *session.Log) error {
		if err := lg.Append("user/message", session.UserMessage([]spi.Block{
			{Type: "text", Text: "先想想"},
		})); err != nil {
			return err
		}
		return lg.Append("assistant/message", session.AssistantMessageEvent(1, 1,
			[]map[string]any{session.ReasoningBlock("先想一下：只有思考，没有正文")},
			"stop", nil))
	})

	msgs, err := newSessionSource(home, cwd).Messages(sid)
	if err != nil {
		t.Fatal(err)
	}
	var got *transport.Message
	for i := range msgs {
		if msgs[i].Role == "assistant" {
			got = &msgs[i]
		}
	}
	if got == nil {
		t.Fatalf("只有 reasoning 的 assistant 被整条丢掉了（%d 条消息）：%+v", len(msgs), msgs)
	}

	raw, merr := json.Marshal(*got)
	if merr != nil {
		t.Fatal(merr)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("解线上消息: %v（%s）", err, raw)
	}
	text, _ := wire["text"].(string)
	reason, _ := wire["reasoning"].(string)
	if strings.TrimSpace(text) == "" && strings.TrimSpace(reason) == "" {
		t.Fatalf("session.load 里这条是空消息（text 与 reasoning 都空）："+
			"serve 推的帧带 reasoning，读回来不能没有：%s", raw)
	}
	if !strings.Contains(reason, "只有思考") {
		t.Fatalf("reasoning 正文没带回来：%s", raw)
	}
}

// 不许误伤：只有 tool_calls 的 assistant 照样算内容。
//
// 它在 load 里的 text 是空的（调用本身不是正文），但**不能因此整条过滤**——
// 工具名/参数挂在下一条 tool/result 的 Tool/Args 上，少了这一条，
// 界面上就只剩一条"没有来源的工具结果"。判据是 assistantHasPayload 的三样占一样。
func TestLoadKeepsToolCallOnlyAssistant(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	const sid = "session-load-toolcall"
	seedTurnSession(t, home, cwd, sid, func(lg *session.Log) error {
		if err := lg.Append("user/message", session.UserMessage([]spi.Block{
			{Type: "text", Text: "看看目录"},
		})); err != nil {
			return err
		}
		if err := lg.Append("assistant/message", session.AssistantMessageEvent(1, 1,
			[]map[string]any{session.ToolCallBlock("call_1", "bash", `{"command":"ls"}`)},
			"tool_calls", nil)); err != nil {
			return err
		}
		// 真实日志里调用另有一条 tool/call 事件（agent 循环写的），tool/result 的
		// Tool/Args 就是从它补的。
		if err := lg.Append("tool/call", session.ToolCallEvent(1, 1, "call_1", "bash", `{"command":"ls"}`)); err != nil {
			return err
		}
		return lg.Append("tool/result", session.ToolResultEvent(1, 1, "call_1",
			[]spi.Block{{Type: "text", Text: "68 个文件"}}, false, cwd, nil))
	})

	msgs, err := newSessionSource(home, cwd).Messages(sid)
	if err != nil {
		t.Fatal(err)
	}
	sawAssistant, sawTool := false, false
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			sawAssistant = true
		case "tool":
			sawTool = true
			if m.Tool != "bash" || !strings.Contains(m.Args, "ls") {
				t.Fatalf("工具结果没补上调用信息（Tool=%q Args=%q）：%+v", m.Tool, m.Args, msgs)
			}
		}
	}
	if !sawAssistant {
		t.Fatalf("只有 tool_calls 的 assistant 被过滤了——它的工具结果会变成孤立的：%+v", msgs)
	}
	if !sawTool {
		t.Fatalf("工具结果丢了：%+v", msgs)
	}
}

// 不许误伤：真正空的消息（无 text、无 reasoning、无 tool_calls）仍然不算内容。
//
// 旧投影只按"JSON 解得开"就收下，这类事件会变成一条空消息——界面画出空气泡，
// 读盘的人却以为"那次模型说了什么，只是没显示"。
func TestLoadDropsTrulyEmptyAssistant(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	const sid = "session-load-empty"
	seedTurnSession(t, home, cwd, sid, func(lg *session.Log) error {
		if err := lg.Append("user/message", session.UserMessage([]spi.Block{
			{Type: "text", Text: "空一轮"},
		})); err != nil {
			return err
		}
		return lg.Append("assistant/message", session.AssistantMessageEvent(1, 1,
			[]map[string]any{{"type": "usage-note", "text": ""}}, "stop", nil))
	})

	msgs, err := newSessionSource(home, cwd).Messages(sid)
	if err != nil {
		t.Fatal(err)
	}
	sawUser := false
	for _, m := range msgs {
		if m.Role == "user" && m.Text == "空一轮" {
			sawUser = true
		}
		if m.Role == "assistant" {
			t.Fatalf("三样都没有的 assistant 仍然算内容（空消息 %+v）；整组消息：%+v", m, msgs)
		}
	}
	if !sawUser {
		t.Fatalf("用户消息不该被这次过滤误伤：%+v", msgs)
	}
}

// wireOf 把一条 Message 变成**线上 JSON** 的键值，断言只按它判。
//
// 为什么不用结构体字段：旧代码没有这些字段时测试连编译都过不了，
// 「先红」就退化成编译错误——那证明不了界面上真的丢东西。
func wireOf(t *testing.T, m transport.Message) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("序列化消息失败: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("解线上消息失败: %v（%s）", err, raw)
	}
	return wire
}

// W187 / TOOL-IDENTITY-LOAD：session.load 的 tool 结果必须带真实 callId/turn/step。
//
// 为什么是这三个：界面把 load 里 role=="tool" 的消息恢复成 tool.result 事件，
// 判据在 TUI `view-spec.ts` 的 `dshGoLoadToolResult`——`callId` 非空、`turn`/`step`
// 都是有限数字，缺一项就返回 undefined（注释原话是 intentional no-event）。
// W182 真机在 resume 后看到的 toolResult 消失，根因就在这里：session.load 的投影
// 从来没带过这三个字段，于是断线重连/重开时工具结果整条被丢掉。
//
// 这里用 turn=2/step=3（不是默认的 1/1）：只有真值才可能被投影成这两个数，
// 编 0 或编 1 都会让这条断言红。
func TestLoadKeepsToolResultIdentity(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	const (
		sid    = "session-load-tool-identity"
		callID = "call_w187"
		view   = `{"kind":"card","title":"目录","body":"68 个文件"}`
	)
	seedTurnSession(t, home, cwd, sid, func(lg *session.Log) error {
		if err := lg.Append("user/message", session.UserMessage([]spi.Block{
			{Type: "text", Text: "看看目录"},
		})); err != nil {
			return err
		}
		if err := lg.Append("assistant/message", session.AssistantMessageEvent(2, 3,
			[]map[string]any{session.ToolCallBlock(callID, "bash", `{"command":"ls"}`)},
			"tool_calls", nil)); err != nil {
			return err
		}
		// 真实日志里调用另有一条 tool/call 事件（agent 循环写的），
		// tool/result 的 Tool/Args 就是从它补的。
		if err := lg.Append("tool/call", session.ToolCallEvent(2, 3, callID, "bash", `{"command":"ls"}`)); err != nil {
			return err
		}
		return lg.Append("tool/result", session.ToolResultEventWithView(2, 3, callID,
			[]spi.Block{{Type: "text", Text: "68 个文件"}}, false, cwd, nil,
			json.RawMessage(view)))
	})

	msgs, _, err := newSessionSource(home, cwd).MessagesTail(sid, 50)
	if err != nil {
		t.Fatal(err)
	}

	var tool map[string]json.RawMessage
	for _, m := range msgs {
		if m.Role == "tool" {
			wire := wireOf(t, m)
			tool = wire
			break
		}
	}
	if tool == nil {
		t.Fatalf("tool/result 没投影出来（%d 条消息）：%+v", len(msgs), msgs)
	}
	raw, _ := json.Marshal(tool)
	t.Logf("session.load 的 tool 消息线上形状：%s", raw)

	// ① 身份三件套：TUI 的恢复门禁。
	var gotCallID string
	if err := json.Unmarshal(tool["callId"], &gotCallID); err != nil || gotCallID != callID {
		t.Fatalf("callId 必须是真实调用 id %q，实际 %s（TUI 缺它就整条丢弃）", callID, tool["callId"])
	}
	for _, key := range []string{"turn", "step"} {
		var n float64
		if err := json.Unmarshal(tool[key], &n); err != nil {
			t.Fatalf("%s 必须是有限数字，实际 %s（TUI 的 position() 只认数字）", key, tool[key])
		}
		want := map[string]float64{"turn": 2, "step": 3}[key]
		if n != want {
			t.Fatalf("%s 必须是真实值 %v，实际 %v（不许编 0）", key, want, n)
		}
	}

	// ② 结果本身：工具名/参数/正文/View 一个都不能少。
	var gotTool, gotArgs, gotText string
	_ = json.Unmarshal(tool["tool"], &gotTool)
	_ = json.Unmarshal(tool["args"], &gotArgs)
	_ = json.Unmarshal(tool["text"], &gotText)
	if gotTool != "bash" || !strings.Contains(gotArgs, "ls") {
		t.Fatalf("工具名/参数丢了：tool=%q args=%q", gotTool, gotArgs)
	}
	if gotText != "68 个文件" {
		t.Fatalf("工具结果正文丢了：%q", gotText)
	}
	if !strings.Contains(string(tool["view"]), `"card"`) {
		t.Fatalf("View 没透传：%s", tool["view"])
	}
}

// 无真值不伪造 0：老日志里的 tool/result 没有 turn/step/callId 时，投影必须
// **整个省略**这三个键，不能写 0——TUI 的 position(0) 是合法数字，会拿它当
// "第 0 轮第 0 步"去恢复一条身份是编出来的工具结果。同一条规矩也适用于
// user/assistant 消息：它们本来就没有工具身份，不许为了对齐形状补一份。
func TestLoadToolResultWithoutIdentityOmitsFields(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	const sid = "session-load-tool-no-identity"
	seedTurnSession(t, home, cwd, sid, func(lg *session.Log) error {
		if err := lg.Append("user/message", session.UserMessage([]spi.Block{
			{Type: "text", Text: "老日志"},
		})); err != nil {
			return err
		}
		// 只有 message.content 的 tool/result：turn/step/callId 都不在。
		return lg.Append("tool/result", map[string]any{
			"message": map[string]any{
				"role":    "tool",
				"content": []map[string]any{session.TextBlock("没有身份的老结果")},
			},
		})
	})

	msgs, _, err := newSessionSource(home, cwd).MessagesTail(sid, 50)
	if err != nil {
		t.Fatal(err)
	}
	identity := []string{"callId", "turn", "step"}
	sawTool := false
	for _, m := range msgs {
		wire := wireOf(t, m)
		if m.Role == "tool" {
			sawTool = true
			if text, _ := json.Marshal(wire["text"]); !strings.Contains(string(text), "没有身份") {
				t.Fatalf("老日志的工具结果正文丢了：%s", text)
			}
		}
		for _, key := range identity {
			if _, exists := wire[key]; exists {
				t.Fatalf("%s 消息上出现了 %q（%s）：没有真值就不许写，0 也是编的",
					m.Role, key, wire[key])
			}
		}
	}
	if !sawTool {
		t.Fatalf("没有身份的老工具结果仍应作为结果出现（只是不能恢复成事件）：%+v", msgs)
	}
}

// toolWires 只取 role=="tool" 的线上形状，保持日志顺序。
func toolWires(t *testing.T, msgs []transport.Message) []map[string]json.RawMessage {
	t.Helper()
	out := make([]map[string]json.RawMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "tool" {
			out = append(out, wireOf(t, m))
		}
	}
	return out
}

// W190 / TOOL-RESULT-ENVELOPE：**失败**的工具结果从 load 恢复后必须还是"失败"。
//
// 为什么必须有 isError：界面把 load 里 role=="tool" 的消息恢复成 tool.result 事件，
// 成败就是事件上的 isError（TUI `view-spec.ts`：`message.isError === true ||
// message.error !== undefined`）。W187 只补了 callId/turn/step，于是失败结果
// 能恢复成事件、却被画成成功——那比丢消息更糟：用户会以为命令真的跑通了。
//
// meta 是 durable 顶层键（与 WS 帧的 data.meta 同一份），有真值就原样透传。
func TestLoadKeepsToolResultErrorEnvelope(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	const (
		sid    = "session-load-tool-error"
		callID = "call_w190_fail"
	)
	seedTurnSession(t, home, cwd, sid, func(lg *session.Log) error {
		if err := lg.Append("user/message", session.UserMessage([]spi.Block{
			{Type: "text", Text: "跑一下"},
		})); err != nil {
			return err
		}
		if err := lg.Append("tool/call", session.ToolCallEvent(2, 3, callID, "bash", `{"command":"false"}`)); err != nil {
			return err
		}
		// isError=true 是真实 v4 的失败形状；meta 里带真实 cwd。
		return lg.Append("tool/result", session.ToolResultEvent(2, 3, callID,
			[]spi.Block{{Type: "text", Text: "exit status 1"}}, true, cwd, nil))
	})

	msgs, _, err := newSessionSource(home, cwd).MessagesTail(sid, 50)
	if err != nil {
		t.Fatal(err)
	}
	tools := toolWires(t, msgs)
	if len(tools) != 1 {
		t.Fatalf("应有 1 条 tool 消息，实际 %d：%+v", len(tools), msgs)
	}
	tool := tools[0]
	raw, _ := json.Marshal(tool)
	t.Logf("session.load 的失败 tool 消息线上形状：%s", raw)

	// ① 成败：TUI 只认字面量 true。
	if got, exists := tool["isError"]; !exists || string(got) != "true" {
		t.Fatalf("isError 必须是 true（这次工具真的失败了），实际 %s；"+
			"没有它，一次真实的失败从 load 恢复后会被画成成功", tool["isError"])
	}

	// ② meta：durable 的 {cwd} 原样透传。
	var meta struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(tool["meta"], &meta); err != nil {
		t.Fatalf("meta 必须是 JSON 对象（durable 原样），实际 %s：%v", tool["meta"], err)
	}
	if meta.Cwd != cwd {
		t.Fatalf("meta.cwd 必须是真实值 %q，实际 %q", cwd, meta.Cwd)
	}

	// ③ 这条事件没有 structured：不许凭空造一个空对象/空数组。
	if got, exists := tool["structured"]; exists {
		t.Fatalf("事件里没有 structured 就不许出现这个键（空对象也是编）：%s", got)
	}

	// ④ W187 的身份不能被这次加法弄丢。
	var gotCallID string
	if err := json.Unmarshal(tool["callId"], &gotCallID); err != nil || gotCallID != callID {
		t.Fatalf("callId 回退了：%s", tool["callId"])
	}
	for key, want := range map[string]float64{"turn": 2, "step": 3} {
		var n float64
		if err := json.Unmarshal(tool[key], &n); err != nil || n != want {
			t.Fatalf("%s 回退成 %s（want %v，err=%v）", key, tool[key], want, err)
		}
	}
	var gotText string
	if err := json.Unmarshal(tool["text"], &gotText); err != nil || gotText != "exit status 1" {
		t.Fatalf("失败正文丢了：%s", tool["text"])
	}
}

// 无真值不伪造：durable 里没有 isError/meta/structured（或显式 false/null）时，
// 三个键必须**整个省略**——写 `"isError":false` / `"meta":null` 都是编。
//
// 为什么 false 也要省略：界面用 `undefined` 区分"这次没有元数据"，
// 而 `false`/`null` 会被原样 spread 进事件，变成"有元数据、值是 null"。
func TestLoadToolResultOmitsFalseEnvelopeFields(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	const sid = "session-load-tool-envelope-honesty"
	seedTurnSession(t, home, cwd, sid, func(lg *session.Log) error {
		if err := lg.Append("user/message", session.UserMessage([]spi.Block{
			{Type: "text", Text: "两种没有真值的形状"},
		})); err != nil {
			return err
		}
		// ① 老/外来日志：只有正文，三个信封键都不在。
		if err := lg.Append("tool/result", map[string]any{
			"message": map[string]any{
				"role":    "tool",
				"content": []map[string]any{session.TextBlock("没有信封的老结果")},
			},
		}); err != nil {
			return err
		}
		// ② 显式 false / null：不是真值，仍要整个省略。
		return lg.Append("tool/result", map[string]any{
			"turn": 1, "step": 2,
			"message": map[string]any{
				"role":       "tool",
				"toolCallId": "call_w190_ok",
				"content":    []map[string]any{session.TextBlock("成功但没元数据")},
				"isError":    false,
			},
			"meta":       nil,
			"structured": nil,
		})
	})

	msgs, _, err := newSessionSource(home, cwd).MessagesTail(sid, 50)
	if err != nil {
		t.Fatal(err)
	}
	tools := toolWires(t, msgs)
	if len(tools) != 2 {
		t.Fatalf("应有 2 条 tool 消息，实际 %d：%+v", len(tools), msgs)
	}
	for i, tool := range tools {
		for _, key := range []string{"isError", "meta", "structured"} {
			if got, exists := tool[key]; exists {
				t.Fatalf("第 %d 条 tool 消息出现了 %q（%s）：没有真值就不许写，false/null 也是编",
					i, key, got)
			}
		}
		if text, _ := json.Marshal(tool["text"]); string(text) == `""` || string(text) == "" {
			t.Fatalf("第 %d 条 tool 消息的正文丢了：%s", i, text)
		}
	}
}

// 真有 structured 就原样透传：宿主不解释它（形状由工具自己定），
// 也不因为"不认识"就丢掉——那是工具结果的另一半。
func TestLoadPassesToolResultStructuredThrough(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	const sid = "session-load-tool-structured"
	seedTurnSession(t, home, cwd, sid, func(lg *session.Log) error {
		if err := lg.Append("user/message", session.UserMessage([]spi.Block{
			{Type: "text", Text: "带结构化结果"},
		})); err != nil {
			return err
		}
		return lg.Append("tool/result", map[string]any{
			"turn": 4, "step": 5,
			"message": map[string]any{
				"role":       "tool",
				"toolCallId": "call_w190_struct",
				"content":    []map[string]any{session.TextBlock("退出码 2")},
				"isError":    true,
			},
			"meta":       map[string]any{"cwd": cwd},
			"structured": map[string]any{"exitCode": 2, "stderr": "boom"},
		})
	})

	msgs, _, err := newSessionSource(home, cwd).MessagesTail(sid, 50)
	if err != nil {
		t.Fatal(err)
	}
	tools := toolWires(t, msgs)
	if len(tools) != 1 {
		t.Fatalf("应有 1 条 tool 消息，实际 %d：%+v", len(tools), msgs)
	}
	var structured struct {
		ExitCode int    `json:"exitCode"`
		Stderr   string `json:"stderr"`
	}
	if err := json.Unmarshal(tools[0]["structured"], &structured); err != nil {
		t.Fatalf("structured 必须原样透传，实际 %s：%v", tools[0]["structured"], err)
	}
	if structured.ExitCode != 2 || structured.Stderr != "boom" {
		t.Fatalf("structured 被改写了：%+v", structured)
	}
	if got := string(tools[0]["isError"]); got != "true" {
		t.Fatalf("isError 必须是 true，实际 %q", got)
	}
}

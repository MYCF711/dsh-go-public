package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/llm"
	"dshgo/internal/session"
	"dshgo/internal/spi"
	"dshgo/internal/transport"
)

// 这一组钉住 modelRunner 的历史组装：**"这条 assistant 消息有没有内容"不能只看正文**。
//
// 真实来路不是"随手造一条怪消息"：`dshd run`（serve 默认走的就是它）写进 v4 的
// assistant/message 里，工具调用是 content 里的 `tool-call` 块，正文可以是空的；
// 紧接着是 tool/result。serve 的 modelRunner 再把这份日志读回来当历史。
// 只按 Text 过滤，那条 assistant 就消失，而它的 tool/result 还在——
// 下一轮模型拿到的正是"有工具结果、却没有对应调用"的孤立结果。

// seedTurnSession 建一份会话并写进给定事件，返回文件路径。
func seedTurnSession(t *testing.T, home, cwd, id string, appends func(*session.Log) error) string {
	t.Helper()
	lg, err := session.CreateSession(home, cwd, id, "default")
	if err != nil {
		t.Fatalf("建会话: %v", err)
	}
	path := lg.Path
	if err := appends(lg); err != nil {
		lg.Close()
		t.Fatalf("写事件: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("关会话: %v", err)
	}
	return path
}

// wireMessage 是线上 JSON 的消息形状（llm.Message 的 Content 字段 json:"-"，只能按 wire 断言）。
type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// ContentText 是 content 解成字符串（没有 content 键时为空）。
	ContentText string `json:"-"`
	ToolCalls   []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
	ToolCallID string `json:"tool_call_id"`
}

// trail 把消息序列画成一行，失败时贴出来才看得出"少了哪一条"。
func trail(msgs []wireMessage) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			ids := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				ids = append(ids, tc.ID+":"+tc.Function.Name)
			}
			if m.ContentText != "" {
				parts = append(parts, fmt.Sprintf("assistant[%s]+%q", strings.Join(ids, ","), m.ContentText))
			} else {
				parts = append(parts, fmt.Sprintf("assistant[%s]", strings.Join(ids, ",")))
			}
		case m.Role == "assistant" && m.ContentText == "":
			parts = append(parts, "assistant<空>")
		case m.Role == "tool":
			parts = append(parts, fmt.Sprintf("tool(%s):%q", m.ToolCallID, m.ContentText))
		default:
			parts = append(parts, fmt.Sprintf("%s:%q", m.Role, m.ContentText))
		}
	}
	return strings.Join(parts, " → ")
}

// newWireRecorder 起一个假的 OpenAI 兼容端点，把每一轮的 messages 原样留下来。
func newWireRecorder(t *testing.T, sse string) (base string, requests <-chan []wireMessage) {
	t.Helper()
	seen := make(chan []wireMessage, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []wireMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("解码请求: %v", err)
		}
		for i := range body.Messages {
			var s string
			if json.Unmarshal(body.Messages[i].Content, &s) == nil {
				body.Messages[i].ContentText = s
			}
		}
		seen <- body.Messages
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1", seen
}

// sseTextOnly 是"模型说了一句话"的最小流（含 finish_reason，否则会被判半截流）。
const sseTextOnly = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

// sseReasoningOnly 是"模型只思考、没输出正文"的流：正是旧代码会静默丢掉的那种回答。
const sseReasoningOnly = "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"先想一下\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

func runTurnMessages(t *testing.T, r *modelRunner, requests <-chan []wireMessage, sid, prompt string) []wireMessage {
	t.Helper()
	if err := r.Run(context.Background(), sid, "turn-"+prompt, prompt); err != nil {
		t.Fatalf("Run(%s): %v", sid, err)
	}
	select {
	case msgs := <-requests:
		if len(msgs) == 0 || msgs[len(msgs)-1].ContentText != prompt {
			t.Fatalf("请求没有消费本轮 prompt %q：%s", prompt, trail(msgs))
		}
		return msgs
	default:
		t.Fatalf("模型端点没收到请求")
		return nil
	}
}

// 红：旧实现按 m.Text == "" 过滤，丢掉"只有 tool_calls"的 assistant 消息，
// 于是工具结果成了孤立的一条（有 tool 没有对应 assistant tool_calls）。
// 绿：新实现保留它，且 tool 与 assistant 的 callId 对得上。
func TestModelRunnerHistoryKeepsToolCallOnlyAssistant(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	seedTurnSession(t, home, cwd, "session-toolcall", func(lg *session.Log) error {
		if err := lg.Append("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: "看看目录里有几个文件"}})); err != nil {
			return err
		}
		// 只有工具调用、没有正文：真实语料里很常见（模型直接开干）。
		if err := lg.Append("assistant/message", session.AssistantMessageEvent(1, 1,
			[]map[string]any{session.ToolCallBlock("call_1", "bash", `{"cmd":"ls"}`)}, "tool_calls", nil)); err != nil {
			return err
		}
		return lg.Append("tool/result", session.ToolResultEvent(1, 1, "call_1",
			[]spi.Block{{Type: "text", Text: "68 个文件"}}, false, cwd, nil))
	})

	base, requests := newWireRecorder(t, sseTextOnly)
	r := &modelRunner{home: home, base: base, model: "m"}
	msgs := runTurnMessages(t, r, requests, "session-toolcall", "接着干")

	got := trail(msgs)
	want := `user:"看看目录里有几个文件" → assistant[call_1:bash] → tool(call_1):"68 个文件" → user:"接着干"`
	if got != want {
		t.Fatalf("历史组装不对：\n得到 %s\n要   %s", got, want)
	}

	// 更强的判据：历史里每一条 tool 结果都必须能找到对应的 assistant tool_calls。
	// 这就是"孤立工具结果"的定义；旧实现红在这里（tool(call_1) 前面没有 call_1）。
	calls := map[string]bool{}
	for _, m := range msgs {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				calls[tc.ID] = true
			}
		}
		if m.Role == "tool" && !calls[m.ToolCallID] {
			t.Fatalf("历史里有孤立的工具结果 %q（前面没有对应的 assistant tool_calls）：%s", m.ToolCallID, got)
		}
	}
}

// 红：只有思考块、没有正文的 assistant 消息同样被旧实现丢掉。
// 绿：保留（位置不丢，下一轮模型看到的轮次顺序才是对的）。
func TestModelRunnerHistoryKeepsReasoningOnlyAssistant(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	seedTurnSession(t, home, cwd, "session-reasoning", func(lg *session.Log) error {
		if err := lg.Append("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: "先想想"}})); err != nil {
			return err
		}
		return lg.Append("assistant/message", session.AssistantMessageEvent(1, 1,
			[]map[string]any{session.ReasoningBlock("先想一下：用户要的是目录里文件数")}, "stop", nil))
	})

	base, requests := newWireRecorder(t, sseTextOnly)
	r := &modelRunner{home: home, base: base, model: "m"}
	msgs := runTurnMessages(t, r, requests, "session-reasoning", "想好了吗")

	// 线上这条 assistant 没有正文也没有 tool_calls（llm.Message 没有 Reasoning 字段），
	// 但它**必须在**：判据是"有思考块也算有内容"，不是"能不能把它发出去"。
	want := `user:"先想想" → assistant<空> → user:"想好了吗"`
	if got := trail(msgs); got != want {
		t.Fatalf("只有思考块的消息被丢了：\n得到 %s\n要   %s", got, want)
	}
}

// 绿（不许误伤）：三样都没有的 assistant 消息仍然要过滤。
func TestModelRunnerHistoryDropsTrulyEmptyAssistant(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	seedTurnSession(t, home, cwd, "session-empty", func(lg *session.Log) error {
		if err := lg.Append("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: "空一轮"}})); err != nil {
			return err
		}
		// 无正文、无思考块、无 tool_calls：content 里只有一个既不认得的块。
		return lg.Append("assistant/message", session.AssistantMessageEvent(1, 1,
			[]map[string]any{{"type": "usage-note", "text": ""}}, "stop", nil))
	})

	base, requests := newWireRecorder(t, sseTextOnly)
	r := &modelRunner{home: home, base: base, model: "m"}
	msgs := runTurnMessages(t, r, requests, "session-empty", "还在吗")

	want := `user:"空一轮" → user:"还在吗"`
	if got := trail(msgs); got != want {
		t.Fatalf("真正的空消息应当被过滤：\n得到 %s\n要   %s", got, want)
	}
}

// 正文照旧：有 text 的 assistant 不能被新判据误伤。
func TestModelRunnerHistoryKeepsTextOnlyAssistant(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	seedTurnSession(t, home, cwd, "session-text", func(lg *session.Log) error {
		if err := lg.Append("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: "一加一"}})); err != nil {
			return err
		}
		return lg.Append("assistant/message", session.AssistantMessageEvent(1, 1,
			[]map[string]any{session.TextBlock("二")}, "stop", nil))
	})

	base, requests := newWireRecorder(t, sseTextOnly)
	r := &modelRunner{home: home, base: base, model: "m"}
	msgs := runTurnMessages(t, r, requests, "session-text", "那二加二呢")

	want := `user:"一加一" → assistant:"二" → user:"那二加二呢"`
	if got := trail(msgs); got != want {
		t.Fatalf("有正文的历史不该被动：\n得到 %s\n要   %s", got, want)
	}
}

// 红：assistant/message 帧原来写死 Seq:1 并直接 pub，绕过 journal——
// 客户端会看到重复 seq、没有 time，补洞时也读不到这一帧。
// 绿：走 emitFrame，seq 由 journal 接着发。这里先垫一帧，真实 step/start/text
// 分别是 2/3，因此 assistant/message 必须是 4，不能写死或绕过新增事件。
func TestModelRunnerAssistantFrameUsesJournalSeq(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	seedTurnSession(t, home, cwd, "session-journal", func(lg *session.Log) error {
		return lg.Append("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: "说句话"}}))
	})

	base, requests := newWireRecorder(t, sseTextOnly)
	journals := transport.NewJournals(nil)
	sid := "session-journal"
	// 垫一帧：给 journal 一个"已经发生过的事"，确保硬编码的 1 与真实序号不同。
	journals.Get(sid).Append("user/message", json.RawMessage(`{"text":"说句话"}`), 1)

	var pushed []transport.ServerFrame
	r := &modelRunner{home: home, base: base, model: "m", journals: journals,
		pub: func(_ string, f transport.ServerFrame) { pushed = append(pushed, f) }}
	_ = runTurnMessages(t, r, requests, sid, "说句话")

	var assist *transport.ServerFrame
	for i := range pushed {
		if pushed[i].Type == "assistant/message" {
			assist = &pushed[i]
		}
	}
	if assist == nil {
		t.Fatalf("没推出 assistant/message 帧：%+v", pushed)
	}
	if assist.Seq != 4 {
		t.Fatalf("assistant/message 帧的 seq = %d，要 4（垫帧 + step/start + text；写死或绕序都不对）", assist.Seq)
	}
	var frameTypes []string
	for _, frame := range pushed {
		frameTypes = append(frameTypes, frame.Type)
	}
	wantTypes := []string{"step/start", "assistant/text", "assistant/message", "step/end", TurnDone}
	if !reflect.DeepEqual(frameTypes, wantTypes) {
		t.Fatalf("modelRunner 完整帧序列=%v want=%v", frameTypes, wantTypes)
	}
	if assist.Time == 0 {
		t.Fatalf("assistant/message 帧没有 time——硬编码 ServerFrame 的典型症状：%+v", *assist)
	}
	// 帧必须真的进了 journal：session.history 读的是那一份，不然补洞补不到它。
	frames, lastSeq, _ := journals.Get(sid).Since(0, 10)
	found := false
	for _, f := range frames {
		if f.Type == "assistant/message" && f.Seq == assist.Seq {
			found = true
		}
	}
	if !found {
		t.Fatalf("assistant/message 没进 journal（lastSeq=%d）：%+v", lastSeq, frames)
	}
}

// 红：只有 reasoning 的回答被 `resp.Content != ""` 挡在落盘之外——
// "看见了但没存"，比没回答更难查。
// 绿：按共享判据落盘，且思考块以 reasoning 块进 v4（与 run 那条路同形）。
func TestModelRunnerPersistsReasoningOnlyResponse(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	path := seedTurnSession(t, home, cwd, "session-reason-persist", func(*session.Log) error { return nil })

	base, requests := newWireRecorder(t, sseReasoningOnly)
	r := &modelRunner{home: home, base: base, model: "m"}
	if err := r.Run(context.Background(), "session-reason-persist", "turn-1", "说点什么"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	<-requests

	log, err := v4.ReadFile(path)
	if err != nil {
		t.Fatalf("读回会话: %v", err)
	}
	var assistData json.RawMessage
	for _, ev := range log.Events {
		if ev.Type == "assistant/message" {
			assistData = ev.Data
		}
	}
	if len(assistData) == 0 {
		t.Fatalf("只有 reasoning 的回答没落盘（事件 %d 条，没有 assistant/message）", len(log.Events))
	}
	var d struct {
		Message struct {
			Content []block `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(assistData, &d); err != nil {
		t.Fatalf("解 assistant/message: %v", err)
	}
	reasoning := ""
	for _, b := range d.Message.Content {
		if b.Type == "reasoning" {
			reasoning = b.Text
		}
	}
	if reasoning != "先想一下" {
		t.Fatalf("落盘的思考块 = %q，要 %q；content=%+v", reasoning, "先想一下", d.Message.Content)
	}
}

// 反方向的孤立：assistant 有 tool_calls、却没有对应的 tool/result。
// maxSteps 用满停在工具调用上、或日志被半截修补，都会留下这种残局；
// 带着它发请求，上游要么 400，要么把下一条用户消息当成工具结果。
// 修法是把没有结果的调用摘掉；摘完这条消息若也空了，就整条不发。
func TestModelRunnerHistoryDropsCallWithoutResult(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	seedTurnSession(t, home, cwd, "session-dangling-call", func(lg *session.Log) error {
		if err := lg.Append("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: "跑一下"}})); err != nil {
			return err
		}
		return lg.Append("assistant/message", session.AssistantMessageEvent(1, 1,
			[]map[string]any{session.ToolCallBlock("call_9", "bash", `{"cmd":"ls"}`)}, "tool_calls", nil))
	})

	base, requests := newWireRecorder(t, sseTextOnly)
	r := &modelRunner{home: home, base: base, model: "m"}
	msgs := runTurnMessages(t, r, requests, "session-dangling-call", "还在吗")

	want := `user:"跑一下" → user:"还在吗"`
	if got := trail(msgs); got != want {
		t.Fatalf("没有结果的工具调用应当被摘掉：\n得到 %s\n要   %s", got, want)
	}
}

// 调试路径（DSHD_SERVE_MODEL_RUNNER=1）也要写轮边界。
//
// 以前 persist 只写 user/assistant 两条消息、从不写 turn/start：日志里没有轮次，
// history 的消费者只能看到一串没有边界的消息；一旦中途失败，还会留下"开了轮没结局"
// 的悬挂态。这条钉住：跑一轮要有 turn/start，跑完要有且只有一条 turn/end。
func TestModelRunnerPersistWritesTurnBoundaries(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	path := seedTurnSession(t, home, cwd, "session-turnboundary", func(lg *session.Log) error {
		return nil // 只建一个空会话：persist 需要文件存在
	})

	r := &modelRunner{home: home}
	resp := &llm.Response{Content: "好的", FinishReason: "stop"}
	if err := r.persist("session-turnboundary", "你好", resp); err != nil {
		t.Fatalf("写不进去：%v", err)
	}

	log, err := v4.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var starts, ends int
	for _, ev := range log.Events {
		switch ev.Type {
		case "turn/start":
			starts++
		case "turn/end":
			ends++
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("轮边界不对：turn/start=%d turn/end=%d（都要恰好 1 条）", starts, ends)
	}

	// 第二轮：号码必须接着走，不能又是 1
	if err := r.persist("session-turnboundary", "再说一次", resp); err != nil {
		t.Fatal(err)
	}
	log, err = v4.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var turns []int
	for _, ev := range log.Events {
		if ev.Type != "turn/start" {
			continue
		}
		var d struct {
			Turn int `json:"turn"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		turns = append(turns, d.Turn)
	}
	if len(turns) != 2 || turns[0] == turns[1] {
		t.Fatalf("两轮的 turn 号必须不同：%v", turns)
	}
}

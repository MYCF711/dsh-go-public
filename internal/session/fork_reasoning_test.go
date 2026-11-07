package session

import (
	"encoding/json"
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/spi"
)

// 这一组钉住 fork 的**投影保真**：Surface 只装 text + tool_calls（llm.Message 没有
// reasoning 字段），所以 fork 必须自己回原始事件（v4.Log.Events）取思考块，
// 否则新会话看起来"什么都对"——只是模型再也不记得它为什么这么干。
//
// 与 cmd/dshd/serve_turn.go 的 assistantBlocks 同一份顺序：思考 → 正文 → 工具调用。

// buildReasoningSrc 造一个带思考块的源会话：两次回答都想过，第二条**只有思考**。
func buildReasoningSrc(t *testing.T, home string) *v4.Log {
	t.Helper()
	l, err := CreateSession(home, "/tmp/src", "src-reason", "standard")
	if err != nil {
		t.Fatal(err)
	}
	must := func(ev string, d any) {
		t.Helper()
		if err := l.Append(ev, d); err != nil {
			t.Fatal(err)
		}
	}
	must("system/message", SystemMessageData(1, 1, "你是 dsh-go"))
	must("user/message", UserMessage([]spi.Block{{Type: "text", Text: "数文件"}}))
	must("assistant/message", AssistantMessageEvent(1, 1, []map[string]any{
		ReasoningBlock("先想一下：用户要的是文件数"),
		TextBlock("我先数一下"),
		ToolCallBlock("c1", "glob", `{"pattern":"*.go"}`),
	}, "tool_calls", nil))
	must("tool/call", ToolCallEvent(1, 1, "c1", "glob", `{"pattern":"*.go"}`))
	must("tool/result", ToolResultEvent(1, 1, "c1", []spi.Block{{Type: "text", Text: "68 个"}}, false, "/tmp/src", []int64{2}))
	must("assistant/message", AssistantMessageEvent(1, 1, []map[string]any{
		ReasoningBlock("68 个，够了"),
		TextBlock("一共 68 个"),
	}, "stop", nil))
	// 只有思考、没有正文也没有工具调用的回答：正是以前 fork 会**整条丢成空块**的形状。
	must("assistant/message", AssistantMessageEvent(1, 2, []map[string]any{
		ReasoningBlock("用户其实想知道目录大小"),
	}, "stop", nil))
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	back, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	return back
}

// eventBlocks 取出一条事件里模型可见的内容块。
// 取块本身不在这里判：它直接调 ContentBlocks（同一份实现，message.content 优先、
// 顶层 content 兜底）。这个 helper 只补 session 投影不关心的 role / toolCallId。
func eventBlocks(t *testing.T, ev v4.Event) (role, toolCallID string, blocks []ContentBlock) {
	t.Helper()
	var d struct {
		Message struct {
			Role       string `json:"role"`
			ToolCallID string `json:"toolCallId"`
		} `json:"message"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("事件 %s（seq %d）解不开: %v", ev.Type, ev.Seq, err)
	}
	var ok bool
	blocks, ok = ContentBlocks(ev.Data, PreferNested)
	if !ok {
		t.Fatalf("事件 %s（seq %d）的内容块取不出来", ev.Type, ev.Seq)
	}
	return d.Message.Role, d.Message.ToolCallID, blocks
}

func blockTypeList(blocks []ContentBlock) string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.Type)
	}
	return strings.Join(out, ",")
}

// eventFingerprint 把一条事件压成"模型可见语义"的一串：事件类型 + 角色 + 内容块。
//
// 等价口径（"不许误伤"那条判据用的就是它）：**忽略每次写入都会变的字段**——
// user/message 与 tool/result 里随机生成的 id、turn/step、time、meta.cwd。
// 留在比对里的是"模型最后看到的东西"：块类型、块正文、工具名/参数、toolCallId。
func eventFingerprint(t *testing.T, ev v4.Event) string {
	t.Helper()
	role, toolCallID, blocks := eventBlocks(t, ev)
	parts := make([]string, 0, len(blocks)+3)
	parts = append(parts, ev.Type, role)
	if toolCallID != "" {
		parts = append(parts, "toolCallId:"+toolCallID)
	}
	for _, b := range blocks {
		switch b.Type {
		case "tool-call":
			parts = append(parts, "tool-call:"+b.ID+":"+b.Name+":"+b.Arguments)
		default:
			parts = append(parts, b.Type+":"+b.Text)
		}
	}
	return strings.Join(parts, "|")
}

func TestForkKeepsReasoningBlocks(t *testing.T) {
	home := t.TempDir()
	src := buildReasoningSrc(t, home)

	forked, err := Fork(src, home, "/tmp/fork-reason", "fork-reason")
	if err != nil {
		t.Fatal(err)
	}
	if err := forked.Close(); err != nil {
		t.Fatal(err)
	}
	back, err := v4.ReadFile(forked.Path)
	if err != nil {
		t.Fatalf("fork 产物读不回来: %v", err)
	}

	var assistants []v4.Event
	for _, ev := range back.Events {
		if ev.Type == "assistant/message" {
			assistants = append(assistants, ev)
		}
	}
	if len(assistants) != 3 {
		t.Fatalf("fork 产物里有 %d 条 assistant/message，期望 3", len(assistants))
	}

	// 第一条：思考 → 正文 → 工具调用（顺序与 assistantBlocks 一致）。
	_, _, b0 := eventBlocks(t, assistants[0])
	if got, want := blockTypeList(b0), "reasoning,text,tool-call"; got != want {
		t.Fatalf("第 1 条 assistant 的块顺序 = %q，期望 %q", got, want)
	}
	if b0[0].Text != "先想一下：用户要的是文件数" {
		t.Fatalf("第 1 条 assistant 的思考正文 = %q", b0[0].Text)
	}
	if b0[1].Text != "我先数一下" || b0[2].ID != "c1" || b0[2].Name != "glob" {
		t.Fatalf("正文/工具调用跟着变了: %+v", b0[1:])
	}

	// 第二条：思考 → 正文。
	_, _, b1 := eventBlocks(t, assistants[1])
	if got, want := blockTypeList(b1), "reasoning,text"; got != want {
		t.Fatalf("第 2 条 assistant 的块顺序 = %q，期望 %q", got, want)
	}
	if b1[0].Text != "68 个，够了" || b1[1].Text != "一共 68 个" {
		t.Fatalf("第 2 条 assistant 的正文变了: %+v", b1)
	}

	// 第三条：只有思考的回答也必须带着思考过去，不能变成一条空消息。
	_, _, b2 := eventBlocks(t, assistants[2])
	if got, want := blockTypeList(b2), "reasoning"; got != want {
		t.Fatalf("只有思考的 assistant 块类型 = %q，期望 %q（整条被丢或被补了正文）", got, want)
	}
	if b2[0].Text != "用户其实想知道目录大小" {
		t.Fatalf("只有思考的 assistant 正文 = %q", b2[0].Text)
	}

	// 顺带确认：思考块在 fork 产物里**读得回来**（不是只有 Surface 那一层认得）。
	if !strings.Contains(string(back.Events[2].Data), `"type":"reasoning"`) {
		t.Fatalf("fork 产物里第 1 条 assistant 没有 reasoning 块: %s", back.Events[2].Data)
	}
}

// TestForkWithoutReasoningKeepsEventSequence 是"不许误伤"那条：
// 没有思考块的会话 fork 后，事件序列与源会话的 surface 逐条等价。
func TestForkWithoutReasoningKeepsEventSequence(t *testing.T) {
	home := t.TempDir()
	src := buildForkSrc(t, home)

	forked, err := Fork(src, home, "/tmp/fork-plain", "fork-plain")
	if err != nil {
		t.Fatal(err)
	}
	if err := forked.Close(); err != nil {
		t.Fatal(err)
	}
	back, err := v4.ReadFile(forked.Path)
	if err != nil {
		t.Fatalf("fork 产物读不回来: %v", err)
	}

	bySeq := make(map[int64]v4.Event, len(src.Events))
	for _, ev := range src.Events {
		bySeq[ev.Seq] = ev
	}

	// 源侧按 surface 取"模型可见的那几条"，与 fork 写入的一一对应。
	var want, got []string
	for _, n := range Surface(src) {
		ev, ok := bySeq[n.Seq]
		if !ok {
			t.Fatalf("surface 节点 seq %d 在源日志里找不到", n.Seq)
		}
		want = append(want, eventFingerprint(t, ev))
	}
	for _, ev := range back.Events {
		got = append(got, eventFingerprint(t, ev))
	}

	if len(got) != len(want) {
		t.Fatalf("fork 写了 %d 条事件，期望 %d 条", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条事件不等价：\n got: %s\nwant: %s", i, got[i], want[i])
		}
	}
}

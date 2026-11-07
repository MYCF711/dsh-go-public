package session

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/spi"
)

func TestSessionWritesReadableLog(t *testing.T) {
	home := t.TempDir()
	cwd := `D:\work\demo`
	l, err := CreateSession(home, cwd, "session-test-1", "standard")
	if err != nil {
		t.Fatal(err)
	}

	must := func(ev string, d any) {
		t.Helper()
		if err := l.Append(ev, d); err != nil {
			t.Fatalf("append %s: %v", ev, err)
		}
	}
	must("turn/start", TurnStartData(1))
	must("user/message", UserMessage([]spi.Block{{Type: "text", Text: "你好"}}))
	must("assistant/message", AssistantMessageEvent(1, 1,
		[]map[string]any{TextBlock("我去读文件"), ToolCallBlock("call_1", "read", `{"file_path":"a.txt"}`)},
		"tool_calls", map[string]any{"inputTokens": 10, "outputTokens": 3, "totalTokens": 13}))
	must("tool/call", ToolCallEvent(1, 1, "call_1", "read", `{"file_path":"a.txt"}`))
	must("tool/result", ToolResultEvent(1, 1, "call_1", []spi.Block{{Type: "text", Text: "读到了"}}, false, cwd, []int64{3}))
	must("turn/end", TurnEndData(1, "completed", ""))
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// 路径规则：<home>/sessions/<projectKey>/<segment>/session.v4.jsonl.zstd
	want := filepath.Join(home, "sessions", "--D-work-demo--", "session-test-1", "session.v4.jsonl.zstd")
	if l.Path != want {
		t.Fatalf("路径 = %s，期望 %s", l.Path, want)
	}

	back, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatalf("自己写的文件读不回来: %v", err)
	}
	if back.Header.Cwd != cwd || back.Header.AgentPreset != "standard" || back.Header.Version != 4 {
		t.Fatalf("header = %+v", back.Header)
	}
	if len(back.Events) != 6 {
		t.Fatalf("事件数 = %d，期望 6", len(back.Events))
	}
	wantTypes := []string{"turn/start", "user/message", "assistant/message", "tool/call", "tool/result", "turn/end"}
	for i, ev := range back.Events {
		if ev.Type != wantTypes[i] {
			t.Fatalf("第 %d 个事件类型 = %q，期望 %q", i, ev.Type, wantTypes[i])
		}
		if ev.Seq != int64(i) {
			t.Fatalf("第 %d 个事件 seq = %d", i, ev.Seq)
		}
	}
}

func TestMessageEventsCarrySurfaceOp(t *testing.T) {
	home := t.TempDir()
	l, err := CreateSession(home, "/tmp/x", "s2", "standard")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append("user/message", UserMessage([]spi.Block{{Type: "text", Text: "hi"}})); err != nil {
		t.Fatal(err)
	}
	if err := l.Append("tool/result", ToolResultEvent(1, 1, "c1", []spi.Block{{Type: "text", Text: "r"}}, false, "/tmp/x", []int64{0})); err != nil {
		t.Fatal(err)
	}
	l.Close()

	back, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range back.Events {
		var m map[string]any
		if err := json.Unmarshal(ev.Data, &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["surfaceOp"]; !ok {
			t.Fatalf("%s 缺 surfaceOp", ev.Type)
		}
	}
}

func TestToolCallArgumentsAreStrings(t *testing.T) {
	home := t.TempDir()
	l, err := CreateSession(home, "/tmp/x", "s3", "standard")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append("tool/call", ToolCallEvent(1, 2, "c9", "read", `{"file_path":"a.txt"}`)); err != nil {
		t.Fatal(err)
	}
	l.Close()

	back, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Data struct {
			Arguments json.RawMessage `json:"arguments"`
		} `json:"data"`
	}
	if err := json.Unmarshal(back.Events[0].Raw, &m); err != nil {
		t.Fatal(err)
	}
	var s string
	if err := json.Unmarshal(m.Data.Arguments, &s); err != nil {
		t.Fatalf("arguments 必须是 JSON 字符串，得到 %s", m.Data.Arguments)
	}
	if s != `{"file_path":"a.txt"}` {
		t.Fatalf("arguments = %q", s)
	}
}

// 没有 type、或负载是 null 的事件不许落盘。
//
// 它们会变成文件里真实存在的字节 + 真实推进的 seq，但 surface/History 一条都读不出来：
// 日志"写成功了"，读回来什么都没有——这是本仓最贵的那类静默成功。
// 拒绝必须发生在写盘之前：seq 不许动，文件里也不许多一行。
func TestAppendRejectsEmptyEvent(t *testing.T) {
	home := t.TempDir()
	l, err := CreateSession(home, "/tmp/x", "s-empty", "standard")
	if err != nil {
		t.Fatal(err)
	}
	before := l.Seq()

	if err := l.Append("", TurnStartData(1)); err == nil {
		t.Fatal("空事件类型必须报错")
	}
	if err := l.Append("turn/start", nil); err == nil {
		t.Fatal("nil 负载必须报错（会落成 data:null）")
	}
	if l.Seq() != before {
		t.Fatalf("被拒绝的事件推进了 seq：%d → %d", before, l.Seq())
	}
	// 合法事件不受影响。
	if err := l.Append("turn/start", TurnStartData(1)); err != nil {
		t.Fatalf("合法事件不该被拦：%v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	back, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Events) != 1 || back.Events[0].Type != "turn/start" {
		t.Fatalf("文件里应当只有那一条合法事件，实际 %+v", back.Events)
	}
}

// 截断的回合不许记成 completed：turn/end 是客户端判断"这一轮做完了吗"的依据。
//
// 旧行为：三个入口都硬写 `TurnEndData(turn, "completed", "")`，于是被 max_tokens
// 截断的回合在日志里也是「完成」——assistant/message 里的 finish=max-tokens 被它盖住，
// 客户端（以及回看日志的人）会以为模型说完了。
func TestTurnEndKindRecordsTruncation(t *testing.T) {
	cases := map[string]string{
		"":              "completed",
		"stop":          "completed",
		"tool_calls":    "completed",
		"awaiting_user": "completed",
		"length":        "max-tokens",
		"aborted":       "aborted",
		"error":         "error",
		"max-tokens":    "max-tokens",
	}
	for finish, want := range cases {
		if got := TurnEndKind(finish); got != want {
			t.Fatalf("finish=%q → %q，期望 %q", finish, got, want)
		}
	}
	// 落进事件里的形状：reason.kind 必须是 max-tokens，而不是 completed。
	data := TurnEndData(1, TurnEndKind("length"), "")
	reason, ok := data["reason"].(map[string]any)
	if !ok || reason["kind"] != "max-tokens" {
		t.Fatalf("截断回合的 turn/end 载荷不对：%v", data)
	}
}

// H3：工具的 ViewSpec 要能落到事件里；没有视图时**不出现 view 键**（不是 null）。
func TestToolResultEventCarriesView(t *testing.T) {
	view := json.RawMessage(`{"kind":"plain","body":"内容"}`)
	ev := ToolResultEventWithView(1, 2, "c1", nil, false, "/w", nil, view)
	got, ok := ev["view"].(json.RawMessage)
	if !ok || len(got) == 0 {
		t.Fatalf("带视图时应当有 view 键，实际 %v", ev["view"])
	}

	bare := ToolResultEventWithView(1, 2, "c1", nil, false, "/w", nil, nil)
	if _, has := bare["view"]; has {
		t.Fatalf("没有视图时不该出现 view 键，实际 %v", bare["view"])
	}
}

package session

import (
	"encoding/json"
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/spi"
)

// 这一组钉住 fork 的**事实保真**，与 fork_reasoning_test.go 的"投影保真"是同一个判据：
//
//	Surface 只装"模型看得见的内容"（text + tool_calls），装不下的是**结论**——
//	这条回答是怎么收工的（finish）、这个工具体是不是失败（isError）、在哪跑的（cwd）、
//	属于哪一轮哪一步（turn/step）、界面该画什么（view）、工具增删声明长什么样（developer）。
//
// 这些字段以前被 copyNode 填成默认值（stop / false / "" / 1,1 / 不写 / 整类跳过），
// 于是一个被 max-tokens 截断的回合 fork 之后看起来"正常收工"，失败的工具体变成成功。
// 那不是"少一个字段"，是**把事实改写成结论**，而且全链路不报错。
//
// 做法与上一轮 reasoning 相同：按 seq 回原始事件取（forkFactsBySeq），
// SurfaceMsg 的公开形状一个字段不加。

// forkSrcBuilder 造源会话：一个 add 一条事件，done 落盘并读回。
type forkSrcBuilder struct {
	t *testing.T
	l *Log
}

func newForkSrc(t *testing.T, home, id string) *forkSrcBuilder {
	t.Helper()
	l, err := CreateSession(home, "/tmp/src", id, "standard")
	if err != nil {
		t.Fatal(err)
	}
	return &forkSrcBuilder{t: t, l: l}
}

func (b *forkSrcBuilder) add(ev string, data any) *forkSrcBuilder {
	b.t.Helper()
	if err := b.l.Append(ev, data); err != nil {
		b.t.Fatal(err)
	}
	return b
}

func (b *forkSrcBuilder) done() *v4.Log {
	b.t.Helper()
	if err := b.l.Close(); err != nil {
		b.t.Fatal(err)
	}
	back, err := v4.ReadFile(b.l.Path)
	if err != nil {
		b.t.Fatal(err)
	}
	return back
}

// forkRead 跑一次 Fork 并把产物读回来。
func forkRead(t *testing.T, home, id string, src *v4.Log) *v4.Log {
	t.Helper()
	forked, err := Fork(src, home, "/tmp/forked", id)
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
	return back
}

func eventsOfType(evs []v4.Event, typ string) []v4.Event {
	var out []v4.Event
	for _, ev := range evs {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// assistantFinishKind 读 assistant/message 的 finish 事实（v4 的 stream finish 记录）。
func assistantFinishKind(t *testing.T, ev v4.Event) string {
	t.Helper()
	var d struct {
		Stream []struct {
			Type   string `json:"type"`
			Reason struct {
				Kind string `json:"kind"`
			} `json:"reason"`
		} `json:"stream"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("assistant 事件解不开: %v（%s）", err, ev.Data)
	}
	for _, r := range d.Stream {
		if r.Type == "finish" {
			return r.Reason.Kind
		}
	}
	return ""
}

// TestForkKeepsAssistantFinishReason：判据 1 —— finish="length" 的会话 fork 后仍是截断。
//
// 源事件的 kind 是 max-tokens（length 的 v4 写法）；旧 copyNode 恒写 "stop"，
// 落盘就成了 completed —— 半个回答在 fork 之后变成"正常收工"。
func TestForkKeepsAssistantFinishReason(t *testing.T) {
	home := t.TempDir()
	src := newForkSrc(t, home, "src-finish").
		add("system/message", SystemMessageData(1, 1, "你是 dsh-go")).
		add("user/message", UserMessage([]spi.Block{{Type: "text", Text: "讲个长故事"}})).
		add("assistant/message", AssistantMessageEvent(2, 1,
			[]map[string]any{TextBlock("从前有座山，山里有座庙")}, "length",
			map[string]any{"inputTokens": 12, "outputTokens": 3, "totalTokens": 15})).
		done()

	back := forkRead(t, home, "fork-finish", src)
	as := eventsOfType(back.Events, "assistant/message")
	if len(as) != 1 {
		t.Fatalf("fork 产物里有 %d 条 assistant/message，期望 1", len(as))
	}
	if got, want := assistantFinishKind(t, as[0]), "max-tokens"; got != want {
		t.Fatalf("fork 后的 finish = %q，期望 %q（源里是 length）：被 max-tokens 截断的回合不许变成正常收工", got, want)
	}
	// 同一个事实边界：源里记了 usage，fork 不许抹成"没记"。
	var d struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(as[0].Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Usage == nil || d.Usage["totalTokens"] != float64(15) {
		t.Fatalf("fork 后的 usage = %v，期望 totalTokens=15", d.Usage)
	}
}

// TestForkKeepsTopLevelFinishFallback：没有 stream 的老语料，顶层 finish 也要跟着走。
//
// 真实语料两种形状都有：v4 的 `stream:[{type:"finish",reason:{kind}}]`，
// 以及更早/别处产生的 `finish` / `finishReason` 顶层字段。只认一种就会让另一种
// 退回默认值——那正是这条缺陷的原始形态。
func TestForkKeepsTopLevelFinishFallback(t *testing.T) {
	home := t.TempDir()
	src := newForkSrc(t, home, "src-finish2").
		add("system/message", SystemMessageData(1, 1, "你是 dsh-go")).
		add("user/message", UserMessage([]spi.Block{{Type: "text", Text: "讲个长故事"}})).
		add("assistant/message", map[string]any{
			"turn": 1, "step": 1,
			"message":   map[string]any{"role": "assistant", "content": []map[string]any{TextBlock("半句")}},
			"finish":    "length",
			"surfaceOp": "append",
		}).
		done()

	back := forkRead(t, home, "fork-finish2", src)
	as := eventsOfType(back.Events, "assistant/message")
	if len(as) != 1 {
		t.Fatalf("fork 产物里有 %d 条 assistant/message，期望 1", len(as))
	}
	if got, want := assistantFinishKind(t, as[0]), "max-tokens"; got != want {
		t.Fatalf("顶层 finish=length 没跟过去：fork 后 kind = %q，期望 %q", got, want)
	}
}

// TestForkKeepsToolResultIsError：判据 2 —— 失败的工具体 fork 后仍是失败。
func TestForkKeepsToolResultIsError(t *testing.T) {
	home := t.TempDir()
	src := newForkSrc(t, home, "src-err").
		add("system/message", SystemMessageData(1, 1, "你是 dsh-go")).
		add("user/message", UserMessage([]spi.Block{{Type: "text", Text: "读那个文件"}})).
		add("assistant/message", AssistantMessageEvent(1, 1,
			[]map[string]any{ToolCallBlock("c1", "read_file", `{"path":"/nope"}`)}, "tool_calls", nil)).
		add("tool/call", ToolCallEvent(1, 1, "c1", "read_file", `{"path":"/nope"}`)).
		add("tool/result", ToolResultEvent(1, 1, "c1",
			[]spi.Block{{Type: "text", Text: "打不开 /nope"}}, true, "/work/real", []int64{2})).
		done()

	back := forkRead(t, home, "fork-err", src)
	tr := eventsOfType(back.Events, "tool/result")
	if len(tr) != 1 {
		t.Fatalf("fork 产物里有 %d 条 tool/result，期望 1", len(tr))
	}
	var d struct {
		Message struct {
			IsError bool `json:"isError"`
		} `json:"message"`
	}
	if err := json.Unmarshal(tr[0].Data, &d); err != nil {
		t.Fatal(err)
	}
	if !d.Message.IsError {
		t.Fatalf("fork 后的 isError = false，期望 true：失败的工具体不许在分叉后变成成功")
	}
}

// TestForkKeepsToolResultCwd：判据 3 —— 工具在哪跑的，fork 后不丢。
func TestForkKeepsToolResultCwd(t *testing.T) {
	home := t.TempDir()
	src := newForkSrc(t, home, "src-cwd").
		add("system/message", SystemMessageData(1, 1, "你是 dsh-go")).
		add("user/message", UserMessage([]spi.Block{{Type: "text", Text: "数文件"}})).
		add("assistant/message", AssistantMessageEvent(1, 1,
			[]map[string]any{ToolCallBlock("c1", "glob", `{"pattern":"*.go"}`)}, "tool_calls", nil)).
		add("tool/call", ToolCallEvent(1, 1, "c1", "glob", `{"pattern":"*.go"}`)).
		add("tool/result", ToolResultEvent(1, 1, "c1",
			[]spi.Block{{Type: "text", Text: "68 个"}}, false, "/work/real", []int64{2})).
		done()

	back := forkRead(t, home, "fork-cwd", src)
	tr := eventsOfType(back.Events, "tool/result")
	if len(tr) != 1 {
		t.Fatalf("fork 产物里有 %d 条 tool/result，期望 1", len(tr))
	}
	var d struct {
		Meta struct {
			Cwd string `json:"cwd"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(tr[0].Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Meta.Cwd != "/work/real" {
		t.Fatalf("fork 后的 meta.cwd = %q，期望 %q：工具结果在哪跑的，分叉后不许变成空", d.Meta.Cwd, "/work/real")
	}
}

// TestForkKeepsSystemTurnStep：system/message 的 turn/step 若源里有，就沿用。
func TestForkKeepsSystemTurnStep(t *testing.T) {
	home := t.TempDir()
	src := newForkSrc(t, home, "src-sys").
		add("system/message", SystemMessageData(4, 2, "你是 dsh-go")).
		add("user/message", UserMessage([]spi.Block{{Type: "text", Text: "数文件"}})).
		done()

	back := forkRead(t, home, "fork-sys", src)
	sys := eventsOfType(back.Events, "system/message")
	if len(sys) != 1 {
		t.Fatalf("fork 产物里有 %d 条 system/message，期望 1", len(sys))
	}
	var d struct {
		Turn int `json:"turn"`
		Step int `json:"step"`
	}
	if err := json.Unmarshal(sys[0].Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Turn != 4 || d.Step != 2 {
		t.Fatalf("fork 后的 system turn/step = %d,%d，期望 4,2（源里写的就是这个）", d.Turn, d.Step)
	}
}

// TestForkKeepsDeveloperMessage：工具增删声明整类不许在 fork 时消失。
//
// developer/message 的内容是 tool-addition / tool-removal 块，Surface 只留
// joinText 之后的正文（通常为空），形状在那一步就没了；所以要原样搬 message。
func TestForkKeepsDeveloperMessage(t *testing.T) {
	home := t.TempDir()
	src := newForkSrc(t, home, "src-dev").
		add("system/message", SystemMessageData(1, 1, "你是 dsh-go")).
		add("developer/message", map[string]any{
			"turn": 4, "step": 2,
			"message": map[string]any{
				"role": "developer",
				"content": []map[string]any{{
					"type":   "tool-addition",
					"name":   "read_file",
					"schema": map[string]any{"type": "object"},
				}},
			},
			"surfaceOp": "append",
		}).
		add("user/message", UserMessage([]spi.Block{{Type: "text", Text: "数文件"}})).
		done()

	back := forkRead(t, home, "fork-dev", src)
	dev := eventsOfType(back.Events, "developer/message")
	if len(dev) != 1 {
		t.Fatalf("fork 产物里有 %d 条 developer/message，期望 1：工具增删声明整类丢了", len(dev))
	}
	var d struct {
		Turn    int `json:"turn"`
		Step    int `json:"step"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(dev[0].Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Turn != 4 || d.Step != 2 {
		t.Fatalf("developer/message 的 turn/step = %d,%d，期望 4,2", d.Turn, d.Step)
	}
	if d.Message.Role != "developer" {
		t.Fatalf("developer/message 的 role = %q，期望 developer", d.Message.Role)
	}
	if len(d.Message.Content) != 1 || d.Message.Content[0].Type != "tool-addition" || d.Message.Content[0].Name != "read_file" {
		t.Fatalf("developer/message 的内容块形状变了: %+v", d.Message.Content)
	}
}

// TestForkKeepsToolResultView：H3 的卡片声明（view）是工具产出的一部分，fork 不许丢。
func TestForkKeepsToolResultView(t *testing.T) {
	home := t.TempDir()
	view := json.RawMessage(`{"kind":"card","title":"68 个文件"}`)
	src := newForkSrc(t, home, "src-view").
		add("system/message", SystemMessageData(1, 1, "你是 dsh-go")).
		add("user/message", UserMessage([]spi.Block{{Type: "text", Text: "数文件"}})).
		add("assistant/message", AssistantMessageEvent(1, 1,
			[]map[string]any{ToolCallBlock("c1", "glob", `{"pattern":"*.go"}`)}, "tool_calls", nil)).
		add("tool/call", ToolCallEvent(1, 1, "c1", "glob", `{"pattern":"*.go"}`)).
		add("tool/result", ToolResultEventWithView(1, 1, "c1",
			[]spi.Block{{Type: "text", Text: "68 个"}}, false, "/tmp/src", []int64{2}, view)).
		done()

	back := forkRead(t, home, "fork-view", src)
	tr := eventsOfType(back.Events, "tool/result")
	if len(tr) != 1 {
		t.Fatalf("fork 产物里有 %d 条 tool/result，期望 1", len(tr))
	}
	var d struct {
		View json.RawMessage `json:"view"`
	}
	if err := json.Unmarshal(tr[0].Data, &d); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(d.View)) != strings.TrimSpace(string(view)) {
		t.Fatalf("fork 后的 view = %s，期望 %s", d.View, view)
	}
}

// TestForkWithoutFactsKeepsEventSequence：判据 4 —— 不许误伤。
//
// 源里本来就没有这些事实（finish=stop、isError=false、cwd=""、turn/step=1,1）时，
// fork 之后事件序列与源 surface 逐条等价，事实也逐条相同。
// 比对口径沿用 fork_reasoning_test.go 的 eventFingerprint：事件类型 + 角色 + 内容块
// （text 正文 / tool-call 的 id+name+args）+ toolCallId；忽略随机 id、turn/step、time、meta.cwd。
// 这里额外把"事实字段"也比一遍（finish kind、isError、cwd、turn/step），
// 证明新代码在**没有事实可搬**时不会凭空发明别的值。
func TestForkWithoutFactsKeepsEventSequence(t *testing.T) {
	home := t.TempDir()
	src := newForkSrc(t, home, "src-plain").
		add("system/message", SystemMessageData(1, 1, "你是 dsh-go")).
		add("user/message", UserMessage([]spi.Block{{Type: "text", Text: "数文件"}})).
		// finish 取 "stop"（v4 的 completed）：源里**没有任何非默认事实**，
		// 这条测试要证明的是"新代码不会凭空发明值"，所以它修前修后都该是绿的。
		add("assistant/message", AssistantMessageEvent(1, 1,
			[]map[string]any{ToolCallBlock("c1", "glob", `{"pattern":"*.go"}`)}, "stop", nil)).
		add("tool/call", ToolCallEvent(1, 1, "c1", "glob", `{"pattern":"*.go"}`)).
		add("tool/result", ToolResultEvent(1, 1, "c1",
			[]spi.Block{{Type: "text", Text: "68 个"}}, false, "", nil)).
		add("assistant/message", AssistantMessageEvent(1, 1,
			[]map[string]any{TextBlock("一共 68 个")}, "stop", nil)).
		done()

	back := forkRead(t, home, "fork-plain", src)

	bySeq := make(map[int64]v4.Event, len(src.Events))
	for _, ev := range src.Events {
		bySeq[ev.Seq] = ev
	}
	var want, got []string
	var wantEvs []v4.Event
	for _, n := range Surface(src) {
		ev, ok := bySeq[n.Seq]
		if !ok {
			t.Fatalf("surface 节点 seq %d 在源日志里找不到", n.Seq)
		}
		wantEvs = append(wantEvs, ev)
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

	// 事实字段：源侧与 fork 侧必须一致（都是默认值）。
	for i, ev := range back.Events {
		assertSameFacts(t, i, wantEvs[i], ev)
	}
}

// assertSameFacts 比较两条事件里 fork 现在会搬的那几个事实字段。
func assertSameFacts(t *testing.T, i int, srcEv, forkEv v4.Event) {
	t.Helper()
	if srcEv.Type != forkEv.Type {
		t.Fatalf("第 %d 条事件类型 %q ≠ %q", i, forkEv.Type, srcEv.Type)
	}
	var s, f struct {
		Turn int `json:"turn"`
		Step int `json:"step"`
		Meta struct {
			Cwd string `json:"cwd"`
		} `json:"meta"`
		Message struct {
			IsError bool `json:"isError"`
		} `json:"message"`
	}
	if err := json.Unmarshal(srcEv.Data, &s); err != nil {
		t.Fatalf("源事件解不开: %v", err)
	}
	if err := json.Unmarshal(forkEv.Data, &f); err != nil {
		t.Fatalf("fork 事件解不开: %v", err)
	}
	if s.Turn != f.Turn || s.Step != f.Step {
		t.Fatalf("第 %d 条 %s 的 turn/step 变了：%d,%d → %d,%d", i, srcEv.Type, s.Turn, s.Step, f.Turn, f.Step)
	}
	if s.Meta.Cwd != f.Meta.Cwd {
		t.Fatalf("第 %d 条 %s 的 meta.cwd 变了：%q → %q", i, srcEv.Type, s.Meta.Cwd, f.Meta.Cwd)
	}
	if s.Message.IsError != f.Message.IsError {
		t.Fatalf("第 %d 条 %s 的 isError 变了：%v → %v", i, srcEv.Type, s.Message.IsError, f.Message.IsError)
	}
	if srcEv.Type == "assistant/message" {
		if got, want := assistantFinishKind(t, forkEv), assistantFinishKind(t, srcEv); got != want {
			t.Fatalf("第 %d 条 assistant 的 finish 变了：%q → %q", i, want, got)
		}
	}
}

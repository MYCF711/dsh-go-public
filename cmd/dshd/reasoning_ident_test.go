package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
)

// 这一组钉住两件事：
//
//  1. **识别思考块的规则只有一份**（session.ReasoningBlocks / ReasoningBlocksBySeq），
//     serve 那条路只转发、不再自己解一遍 JSON；
//  2. 两个**口径**各归各位：fork / ACP load 要"全带"（第二条思考块不许被当重复丢掉），
//     serve 的历史组装只要"第一条"（既有行为，合并实现不许顺带改掉）。
//
// 为什么要有 (2)：合并两份实现时最容易犯的错就是"顺手统一口径"——让 fork 只取第一条
// （父会话的第二次思考丢失），或者让 serve 返回全部（历史组装的判据跟着变）。两种都
// 不会报错，只是悄悄改了行为。所以这里把两个口径**同时**钉住。

// assistantWithReasoning 造一条 message.content 形状的 assistant/message 事件。
func assistantWithReasoning(t *testing.T, seq int64, blocks ...map[string]any) v4.Event {
	t.Helper()
	data, err := json.Marshal(session.AssistantMessageEvent(1, 1, blocks, "stop", nil))
	if err != nil {
		t.Fatalf("铺事件失败: %v", err)
	}
	return v4.Event{Type: "assistant/message", Seq: seq, Data: data}
}

// reasoningCorpus 覆盖识别规则的每一条分支。
func reasoningCorpus(t *testing.T) *v4.Log {
	t.Helper()
	return &v4.Log{Events: []v4.Event{
		// 1：一条回答里两块思考——两个口径在这里分岔。
		assistantWithReasoning(t, 1,
			session.ReasoningBlock("第一块：先看目录"),
			session.ReasoningBlock("第二块：再数文件"),
			session.TextBlock("一共 68 个"),
		),
		// 2：空白块在前、真块在后。空白不算思考，也不许占住"第一条"。
		assistantWithReasoning(t, 2,
			session.ReasoningBlock("   \n\t "),
			session.ReasoningBlock("空白之后的真思考"),
		),
		// 3：没有思考块。
		assistantWithReasoning(t, 3, session.TextBlock("只有正文")),
		// 4：老形状——content 挂在**顶层**而不是 message.content（兜底那一条）。
		{
			Type: "assistant/message", Seq: 4,
			Data: []byte(`{"turn":1,"step":1,"content":[{"type":"reasoning","text":"顶层兜底的思考"}]}`),
		},
		// 5：只有空白思考块——与"没有"等价。
		assistantWithReasoning(t, 5, session.ReasoningBlock("\n\n")),
		// 6：非 assistant/message 事件就算带了 reasoning 块也不算。
		{
			Type: "tool/result", Seq: 6,
			Data: []byte(`{"content":[{"type":"reasoning","text":"不属于这里"}]}`),
		},
	}}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReasoningIdentificationFirstVsAll(t *testing.T) {
	log := reasoningCorpus(t)

	// 口径 A（fork / ACP load）：**全带**，按原顺序，空白块被过滤。
	wantAll := map[int64][]string{
		1: {"第一块：先看目录", "第二块：再数文件"},
		2: {"空白之后的真思考"},
		4: {"顶层兜底的思考"},
	}
	all := session.ReasoningBlocksBySeq(log)
	if len(all) != len(wantAll) {
		t.Fatalf("ReasoningBlocksBySeq 命中 %d 条事件，期望 %d 条: %v", len(all), len(wantAll), all)
	}
	for seq, want := range wantAll {
		if got := all[seq]; !equalStrings(got, want) {
			t.Fatalf("seq %d 的思考块 = %q，期望 %q（「全带」被改成了别的口径？）", seq, got, want)
		}
	}

	// 唯一实现：批量版只是把事件级判据铺开，两者必须逐条一致。
	for _, ev := range log.Events {
		got := session.ReasoningBlocks(ev)
		want := wantAll[ev.Seq]
		if !equalStrings(got, want) {
			t.Fatalf("seq %d：ReasoningBlocks = %q，但 ReasoningBlocksBySeq 给的是 %q", ev.Seq, got, want)
		}
	}

	// 口径 B（serve 的历史组装）：**只取第一条**，且必须是第一块。
	first := reasoningBySeq(log)
	wantFirst := map[int64]string{
		1: "第一块：先看目录",
		2: "空白之后的真思考",
		4: "顶层兜底的思考",
	}
	if len(first) != len(wantFirst) {
		t.Fatalf("reasoningBySeq 命中 %d 条事件，期望 %d 条: %v", len(first), len(wantFirst), first)
	}
	for seq, want := range wantFirst {
		if got := first[seq]; got != want {
			t.Fatalf("seq %d 的首条思考 = %q，期望 %q（「只取第一条」被合并改掉了？）", seq, got, want)
		}
	}

	// 反面：不许把两块拼起来，也不许取最后一块。
	if got := first[1]; got != "第一块：先看目录" {
		t.Fatalf("seq 1 首条思考 = %q：既不是第一块，也不是 sole 的那条", got)
	}

	// 口径 C（history_search 的按块顺序拼正文）：同一份块级判据。
	if _, ok := session.ReasoningText("reasoning", "  "); ok {
		t.Fatalf("空白思考块被 ReasoningText 认成了内容")
	}
	if text, ok := session.ReasoningText("reasoning", " 真思考 "); !ok || text != " 真思考 " {
		t.Fatalf("ReasoningText 对真思考块返回 (%q, %v)，期望原文与 true", text, ok)
	}
	if _, ok := session.ReasoningText("text", "正文"); ok {
		t.Fatalf("ReasoningText 把 text 块认成了思考块")
	}
}

// mustReadSource 读一个源码文件（结构判据只在仓库里跑，路径相对本包目录）。
func mustReadSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("读不到 %s（不在仓库目录里跑？）: %v", path, err)
	}
	return string(b)
}

// funcBody 截出一个函数从签名到收尾大括号的源码。
func funcBody(t *testing.T, src, sig string) string {
	t.Helper()
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("源码里找不到 %q", sig)
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}\n"); j >= 0 {
		return rest[:j+3]
	}
	return rest
}

// TestReasoningIdentificationHasOneSource 是"唯一一份实现"的结构性证据。
//
// 它挡的是**回潮**：谁哪天图省事在调用点再解一遍 ev.Data、再写一遍
// `b.Type == "reasoning"`，这里就红。行为测试挡不住这种回潮——两处实现只要
// 当前一致，行为测试全绿，直到其中一处改了才发作。
func TestReasoningIdentificationHasOneSource(t *testing.T) {
	serve := mustReadSource(t, "serve_turn.go")
	history := mustReadSource(t, "history.go")
	fork := mustReadSource(t, filepath.Join("..", "..", "internal", "session", "fork.go"))
	surface := mustReadSource(t, filepath.Join("..", "..", "internal", "session", "surface.go"))

	// 1) serve 的 reasoningBySeq 必须转发给唯一实现，自身不再解 JSON、不再写判据。
	body := funcBody(t, serve, "func reasoningBySeq(")
	if !strings.Contains(body, "session.ReasoningBlocksBySeq(log)") {
		t.Fatalf("reasoningBySeq 没有转发给 session.ReasoningBlocksBySeq——识别规则又成两份了:\n%s", body)
	}
	for _, banned := range []string{"json.Unmarshal", `b.Type == "reasoning"`, "TrimSpace(b.Text)"} {
		if strings.Contains(body, banned) {
			t.Fatalf("reasoningBySeq 里还留着识别规则的私有实现 %q:\n%s", banned, body)
		}
	}

	// 2) fork / Surface 也不许再写一份，也不许再长出 reasoningBlocksBySeq 这个副本。
	for name, src := range map[string]string{"fork.go": fork, "surface.go": surface} {
		for _, banned := range []string{`b.Type == "reasoning"`, "func reasoningBlocksBySeq("} {
			if strings.Contains(src, banned) {
				t.Fatalf("%s 里还有第二份思考块识别实现 %q", name, banned)
			}
		}
	}
	if !strings.Contains(fork, "ReasoningBlocksBySeq(src)") {
		t.Fatalf("fork.go 没有调 ReasoningBlocksBySeq")
	}

	// 3) history_search 的块级判据也走同一份（它不能改用 ReasoningBlocks：
	//    那里要按块顺序拼 text / reasoning / tool-call，见 eventText 的注释）。
	if !strings.Contains(history, "session.ReasoningText(b.Type, b.Text)") {
		t.Fatalf("history.go 的 case \"reasoning\" 没有走 session.ReasoningText")
	}
}

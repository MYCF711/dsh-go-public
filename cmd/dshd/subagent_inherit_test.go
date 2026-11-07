package main

import (
	"context"
	"strings"
	"testing"

	"dshgo/internal/session"
	"dshgo/internal/spi"
)

// 这一组钉住子代理继承的**转写**：`subagent_fork` 拿到的是给子代理看的纯文本，
// 不是模型的 messages 数组——所以它有自己的问题：**哪些内容算"这条消息有东西"**。
//
// 2026-11-04 的 `serve_turn.go` 修过同一个坑（`m.Text == ""` 丢掉只有 tool_calls
// 的 assistant），但继承这条路还留着原样的判断。它的症状比 serve 那边更难发现：
// 子代理不会报错，只是"不知道父会话干了什么"，然后重新做一遍已经做过的事。

// 判据（任务书写死的三条）：只有 tool_calls 的 assistant、只有 reasoning 的
// assistant，两条都必须出现在转写里；三样都没有的那条才算空。
func TestSubagentInheritKeepsToolCallsAndReasoning(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	const sid = "session-inherit"

	seedTurnSession(t, home, cwd, sid, func(lg *session.Log) error {
		// 正文里带换行：转写必须一条消息一行（见下面对行的断言）。
		if err := lg.Append("user/message", session.UserMessage([]spi.Block{
			{Type: "text", Text: "看看目录里有几个文件\n顺便说一下大小"},
		})); err != nil {
			return err
		}
		// 只有工具调用、没有正文：模型直接开干时最常见的一种回答。
		if err := lg.Append("assistant/message", session.AssistantMessageEvent(1, 1,
			[]map[string]any{session.ToolCallBlock("call_1", "bash", `{"command":"ls"}`)},
			"tool_calls", nil)); err != nil {
			return err
		}
		if err := lg.Append("tool/result", session.ToolResultEvent(1, 1, "call_1",
			[]spi.Block{{Type: "text", Text: "68 个文件"}}, false, cwd, nil)); err != nil {
			return err
		}
		// 只有思考块、没有正文：模型想了、只是没输出正文。
		if err := lg.Append("assistant/message", session.AssistantMessageEvent(1, 2,
			[]map[string]any{session.ReasoningBlock("先想一下：用户要的是文件数")},
			"stop", nil)); err != nil {
			return err
		}
		// 真空：无正文、无思考块、无 tool_calls。三条里只有它该被跳过。
		return lg.Append("assistant/message", session.AssistantMessageEvent(1, 3,
			[]map[string]any{{"type": "usage-note", "text": ""}}, "stop", nil))
	})

	got := inheritFromCtx(home)(spi.WithSession(context.Background(), sid))
	t.Logf("转写：\n%s", got)

	if !strings.Contains(got, "看看目录里有几个文件 顺便说一下大小") {
		t.Fatalf("用户消息没进转写（或多行正文没压成一行）：\n%s", got)
	}
	// 判据一：只有 tool_calls 的 assistant 必须出现，且调用要变成一句人话。
	if !strings.Contains(got, `[调用 bash：{"command":"ls"}]`) {
		t.Fatalf("只有 tool_calls 的 assistant 丢了（子代理会以为父会话没查过目录）：\n%s", got)
	}
	// 判据二：只有 reasoning 的 assistant 必须出现。
	if !strings.Contains(got, "先想一下：用户要的是文件数") {
		t.Fatalf("只有 reasoning 的 assistant 丢了：\n%s", got)
	}
	// 判据三：真空那条才算空——assistant 恰好两条，不是三条、也不是一条。
	if n := strings.Count(got, "assistant: "); n != 2 {
		t.Fatalf("assistant 行数 = %d，要 2（tool_calls 那条 + reasoning 那条；真空那条该跳过）：\n%s", n, got)
	}
	if !strings.Contains(got, "tool: 68 个文件") {
		t.Fatalf("工具结果丢了：\n%s", got)
	}

	// 一条消息一行：子代理结果里的「继承 N 条历史」按非空行数算（inheritNote）。
	lines := nonEmptyLines(got)
	if len(lines) != 4 {
		t.Fatalf("转写有 %d 行，要 4 行（user / assistant+调用 / tool / assistant+思考）：\n%s\n%v",
			len(lines), got, lines)
	}
	if !strings.HasPrefix(lines[1], "assistant: ") || !strings.HasPrefix(lines[3], "assistant: ") {
		t.Fatalf("assistant 行没有角色前缀，inheritNote 与模型都读不出来：%v", lines)
	}
}

// 没有父会话（ctx 里没有会话 id）时返回空串，而不是一段空转写。
// 空转写的后果是结果摘要说"继承 0 条"，与"读不到"分不开——前者像正常，后者要能被看出来。
func TestSubagentInheritWithoutSession(t *testing.T) {
	if got := inheritFromCtx(t.TempDir())(context.Background()); got != "" {
		t.Fatalf("没有会话时应当返回空串，得到 %q", got)
	}
}

func nonEmptyLines(s string) []string {
	out := make([]string, 0, 8)
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

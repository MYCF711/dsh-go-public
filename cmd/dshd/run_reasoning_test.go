package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
)

// 这一组钉住 `run` 这条路的**写侧**：v4 里的 `assistant/message` 块顺序必须是
// 思考 → 正文 → 调用，而且铺块逻辑只能有一份
// （`serve_turn.assistantBlocksFrom`，serve / acp 已经在调它）。
//
// 为什么还要单独钉一次：`run` 是三条路（run / serve / acp）里最后一份自己铺块的。
// 昨天一天里"同一条规则两处实现各犯一次"发生了两次（`CompactSchema`、
// `stripDescriptions` 都删名叫 `description` 的参数），所以这里不只测结果，
// 也测"结果来自共用函数"：思考取自 `agent.AssistantTurn.Reasoning`。
// 旧实现自己攒一个 `stepReasoning`，于是思考来源多出一份——两份迟早漂。
//
// 判据取自任务书：落盘块顺序 思考→正文→调用；没有思考的一轮**字节不动**。

// runSSE 起一个假网关；第 1 步的流由 first 给，之后每一步用 rest。
//
// 为什么要分步：一条带工具调用的轮次必须走到第 2 步（第 1 步只调工具、
// 第 2 步才收工），第 1 步的 `assistant/message` 才是"思考+正文+调用"那条。
func runSSE(t *testing.T, first, rest string) string {
	t.Helper()
	return d1SSE(t, func(w http.ResponseWriter, n int) {
		body := rest
		if n == 1 {
			body = first
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
		w.(http.Flusher).Flush()
	})
}

// runReasoningCall 是"第 1 步：先想、再说一句、然后调工具"的流。
//
// 两片思考是刻意的：`llm.Response.Reasoning` 是**增量拼起来**的，只有一片时
// "拼"与"取第一片"看不出区别，落盘的思考正文对不对就测不出来。
// 工具名用 `nope`：最小 manifest 里没有它，执行会失败但循环继续——
// 这条用例要的是**块顺序**，不想要一个真工具把测试绑到某个模块上。
const runReasoningCall = "" +
	"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"先想一下：\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"用户要的是文件数\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"我先数一下\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\"," +
	"\"function\":{\"name\":\"nope\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: [DONE]\n\n"

// runPlainText 是唯一的收工流：一句正文、没有思考、没有调用、没有 usage。
const runPlainText = "data: {\"choices\":[{\"delta\":{\"content\":\"好\"},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

// runWhitespaceOnly 是"只想了空白、也只说了空白"的流。
// 它是共用判据（`TrimSpace`）与 run 旧判据（`Len() > 0` / `!= ""`）唯一分歧的那种输入。
const runWhitespaceOnly = "" +
	"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"  \"},\"finish_reason\":null}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\" \"},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

// runLogPath 是 `run` 这一轮的会话文件名。
func runLogPath(t *testing.T, home, cwd, sid string) string {
	t.Helper()
	path, err := session.SessionPath(home, cwd, sid)
	if err != nil {
		t.Fatalf("算不出会话文件路径: %v", err)
	}
	return path
}

// runAssistantDataAt 取第 step 步 assistant/message 的**原始字节**（没有就 nil）。
func runAssistantDataAt(t *testing.T, path string, step int) json.RawMessage {
	t.Helper()
	log, err := v4.ReadFile(path)
	if err != nil {
		t.Fatalf("读回会话失败（%s）: %v", path, err)
	}
	for _, ev := range log.Events {
		if ev.Type != "assistant/message" {
			continue
		}
		var d struct {
			Step int `json:"step"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("assistant/message 的 data 解不开: %v（%s）", err, ev.Data)
		}
		if d.Step == step {
			return ev.Data
		}
	}
	return nil
}

// runBlocksAt 取第 step 步 assistant/message 的 content 块。
func runBlocksAt(t *testing.T, path string, step int) []persistedBlock {
	t.Helper()
	raw := runAssistantDataAt(t, path, step)
	if raw == nil {
		t.Fatalf("第 %d 步没有落 assistant/message 事件（写侧没写，后面怎么投影都读不到东西）", step)
	}
	var d struct {
		Message struct {
			Content []persistedBlock `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("message.content 解不开: %v（%s）", err, raw)
	}
	return d.Message.Content
}

// TestRunPersistsReasoningBlocks 钉住 run 写侧：思考落成 `reasoning` 块，
// 排在正文与调用**之前**，且正文就是事件流里拼出来的那段（不另攒一份）。
func TestRunPersistsReasoningBlocks(t *testing.T) {
	home, cwd := d1Workspace(t, "run-reason")
	base := runSSE(t, runReasoningCall, runPlainText)
	if err := d1Run(t, home, "run-reason", base); err != nil {
		t.Fatalf("这一轮应当正常收工: %v", err)
	}

	path := runLogPath(t, home, cwd, "run-reason")
	blocks := runBlocksAt(t, path, 1)
	if got := blockTypes(blocks); got != "reasoning→text→tool-call" {
		t.Fatalf("落盘块顺序 = %q，期望 reasoning→text→tool-call"+
			"（与 serve/acp 同序：思考 → 正文 → 调用）", got)
	}
	// 思考正文必须逐字等于模型吐的两片之和：拼少了 = 只取了第一片，
	// 拼错了 = 又攒了一份平行副本。
	if want := "先想一下：用户要的是文件数"; blocks[0].Text != want {
		t.Fatalf("落盘的思考正文 = %q，期望 %q", blocks[0].Text, want)
	}
	if blocks[1].Text != "我先数一下" {
		t.Fatalf("落盘的正文 = %q，期望 %q", blocks[1].Text, "我先数一下")
	}
	if blocks[2].Type != "tool-call" || blocks[2].ID != "call-1" || blocks[2].Name != "nope" {
		t.Fatalf("调用块丢了身份：%+v", blocks[2])
	}
}

// TestRunNoReasoningPayloadBytesUnchanged 钉住"不许误伤"。
//
// 口径：**`assistant/message` 的 `data` 原始字节**。
//   - 不比整行：行信封里有墙钟 `time`，两次跑不可能相等；
//   - 不比解出来的结构：那会把键序、空数组、多余字段一起放过。
//
// 基线是没有思考、没有调用、没有 usage 的一轮（`runPlainText`）：它正是最常见的
// 那种回答，落盘长什么样必须与**改前**逐字节一样。期望值是改前代码跑出来的
// 字面量，所以这条用例在 HEAD 隔离副本上就绿，改完仍绿——两边都绿才叫"没动它"。
func TestRunNoReasoningPayloadBytesUnchanged(t *testing.T) {
	home, cwd := d1Workspace(t, "run-plain")
	base := runSSE(t, runPlainText, runPlainText)
	if err := d1Run(t, home, "run-plain", base); err != nil {
		t.Fatalf("这一轮应当正常收工: %v", err)
	}

	raw := runAssistantDataAt(t, runLogPath(t, home, cwd, "run-plain"), 1)
	if raw == nil {
		t.Fatal("没有思考的一轮也必须有 assistant/message")
	}
	const want = `{"message":{"content":[{"text":"好","type":"text"}],"role":"assistant"},` +
		`"step":1,"stream":[{"reason":{"kind":"completed"},"type":"finish"}],` +
		`"surfaceOp":"append","turn":1}`
	if string(raw) != want {
		t.Fatalf("没有思考的一轮，落盘字节变了。\n实际: %s\n期望: %s", raw, want)
	}
}

// TestRunWhitespaceOnlyFollowsSharedPredicate 钉住**唯一的行为差**（已知且刻意）：
// 只有空白字符的思考/正文，以前 run 会各写一个块（`Len() > 0` / `!= ""`），
// 现在按共用函数的 `TrimSpace` 判据不写。
//
// 为什么不做特例保留：`serve_turn.assistantBlocksFrom` 的判据是三条路共用的，
// 为 run 的旧口径开一个分支，就等于把"同一条规则两处实现"又请回来。
// 这类回答在 `assistantHasPayload` 那里本来就是"没有内容"，写进去也没有任何投影
// 读得出来；docs/silent-success-audit.md §「残留清零」已经把这条差异登记在案
// （ACP 当时也是同一处收敛）。
func TestRunWhitespaceOnlyFollowsSharedPredicate(t *testing.T) {
	home, cwd := d1Workspace(t, "run-blank")
	base := runSSE(t, runWhitespaceOnly, runWhitespaceOnly)
	if err := d1Run(t, home, "run-blank", base); err != nil {
		t.Fatalf("这一轮应当正常收工: %v", err)
	}

	blocks := runBlocksAt(t, runLogPath(t, home, cwd, "run-blank"), 1)
	if got := blockTypes(blocks); got != "" {
		t.Fatalf("只有空白的回答不该落块（共用判据是 TrimSpace），实际 = %q", got)
	}
	if !strings.Contains(string(runAssistantDataAt(t, runLogPath(t, home, cwd, "run-blank"), 1)), `"content":[]`) {
		t.Fatal("空块应当落成空数组，而不是缺字段")
	}
}

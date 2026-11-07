package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/agent"
	"dshgo/internal/modes"
	"dshgo/internal/session"
)

// 这一组钉住 ACP `Prompt` 的**写侧**：一趟带思考的 prompt 跑完，v4 里的
// `assistant/message` 必须带 `reasoning` 块，且顺序与 serve/run/fork 那条路一致
// （思考 → 正文 → 调用）。
//
// 为什么这比 `acp_load_reasoning_test.go` 更靠根：那一组钉的是**读**（Load 会认
// 思考块），前提是日志里真有。写侧不写的话，从零开始的一条 ACP 会话永远没有
// 思考可读——`history`、`fork`、`session.load` 三个投影都对着一份没写的源，
// 各自"正确地"什么都不显示。这种"每一环都对、整条链没有数据"最难查。
//
// 判据取自任务书：落盘块顺序 思考→正文→调用；没有思考的一轮**字节不动**。

// sseReasoningTurn 起一个假的 OpenAI 兼容端点。
//
// 第 1 步：先吐两片思考（`reasoning_content`），再吐一段正文；tool 非空时
// 最后吐一个到它的调用，否则直接收尾。第 2 步只吐一句正文。
//
// 两片思考是刻意的：`llm.Response.Reasoning` 是**增量拼起来**的，只有一片时
// "拼"与"取第一片"看不出区别，落盘的思考正文对不对就测不出来。
func sseReasoningTurn(t *testing.T, tool string) string {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		write := func(choice map[string]any) {
			b, err := json.Marshal(map[string]any{"choices": []any{choice}})
			if err != nil {
				t.Errorf("拼假网关的分片失败: %v", err)
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			f.Flush()
		}
		delta := func(d map[string]any) map[string]any { return map[string]any{"delta": d} }

		if atomic.AddInt32(&n, 1) == 1 {
			write(delta(map[string]any{"reasoning_content": "先想一下："}))
			write(delta(map[string]any{"reasoning_content": "用户要的是文件数"}))
			write(delta(map[string]any{"content": "我先数一下"}))
			if tool != "" {
				write(map[string]any{
					"delta": map[string]any{"tool_calls": []any{map[string]any{
						"index": 0, "id": "call-1", "type": "function",
						"function": map[string]any{"name": tool, "arguments": "{}"},
					}}},
					"finish_reason": "tool_calls",
				})
			} else {
				write(map[string]any{"delta": map[string]any{}, "finish_reason": "stop"})
			}
		} else {
			write(map[string]any{"delta": map[string]any{"content": "好"}, "finish_reason": "stop"})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// sseReasoningOnly 起一个只吐思考、不吐正文的端点——"模型想了但没说"的那种回答。
//
// 它在旧写侧下会落一条 `content: []` 的 assistant/message：事件在、内容空，
// 读回来是一条空气泡（history/fork/load 都按"三样都没有"过滤掉它）。
func acpReasoningOnlyServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 流体复用 serve 那条路已经在用的 sseReasoningOnly：同一段"只想了、没说"
		// 的语料两条入口都跑，免得各写一份、各测各的形状。
		fmt.Fprint(w, sseReasoningOnly)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// ssePlainText 起一个最普通的端点：一句正文、带 usage、没有思考也没有调用。
// 它是"字节等价"用例的基线：这一轮在改动前后必须逐字节一样。
func ssePlainText(t *testing.T, text string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(
			`{"choices":[{"delta":{"content":%q},"finish_reason":"stop"}],`+
				`"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`, text))
		fmt.Fprint(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// promptOnceAt 跑一轮并把事件流收下来。
//
// cwd 由调用方固定：结束后要按**同一个** (home, cwd, id) 算出会话文件在哪。
// acp_test.go 的 promptOnce 每次给一个新的 t.TempDir()，读不回落盘。
func promptOnceAt(t *testing.T, r *acpRunner, sessionID, cwd string) []agent.Event {
	t.Helper()
	var evs []agent.Event
	if _, _, err := r.Prompt(context.Background(), sessionID, cwd, "做点事",
		func(e agent.Event) error {
			evs = append(evs, e)
			return nil
		}); err != nil {
		t.Fatalf("Prompt 出错: %v", err)
	}
	return evs
}

// persistedBlock 是落盘 content 块的三种字段（够断言顺序与正文，不是全量 schema）。
type persistedBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// readPersisted 关掉写句柄，把整份 v4 日志读回来。
//
// 先关再读：Windows 上句柄没关就删 t.TempDir 会失败，而且这里要的是"落盘的字节"，
// 不是"内存里攒着还没刷的东西"。
func readPersisted(t *testing.T, r *acpRunner, sessionID, cwd string) *v4.Log {
	t.Helper()
	path, err := session.SessionPath(r.home, cwd, sessionID)
	if err != nil {
		t.Fatalf("算不出会话文件路径: %v", err)
	}
	if err := r.Shutdown(); err != nil {
		t.Fatalf("关写句柄失败: %v", err)
	}
	log, err := v4.ReadFile(path)
	if err != nil {
		t.Fatalf("读回会话失败（%s）: %v", path, err)
	}
	return log
}

// assistantDataAt 取第 step 步 assistant/message 的**原始字节**（没有就 nil）。
func assistantDataAt(t *testing.T, log *v4.Log, step int) json.RawMessage {
	t.Helper()
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

// blocksAt 取第 step 步 assistant/message 的 content 块。
func blocksAt(t *testing.T, log *v4.Log, step int) []persistedBlock {
	t.Helper()
	raw := assistantDataAt(t, log, step)
	if raw == nil {
		// 这一条本身就是缺陷：写侧没写，后面的投影再好也读不到东西。
		t.Fatalf("第 %d 步没有落 assistant/message 事件", step)
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

// blockTypes 是"顺序"这件事的可读形式。
func blockTypes(blocks []persistedBlock) string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.Type)
	}
	return strings.Join(out, "→")
}

// streamedReasoning 把事件流里的思考增量拼起来——**这就是模型想过的东西**，
// 落盘的思考块必须与之逐字一致。
func streamedReasoning(evs []agent.Event) string {
	var b strings.Builder
	for _, e := range evs {
		if e.Kind == agent.EventReasoning {
			b.WriteString(e.Text)
		}
	}
	return b.String()
}

// TestACPPromptPersistsReasoningBlocks 钉住写侧：ACP 这一轮的思考要落成
// `assistant/message` 里的 `reasoning` 块，且排在正文与调用**之前**。
func TestACPPromptPersistsReasoningBlocks(t *testing.T) {
	t.Run("思考+正文：思考→正文", func(t *testing.T) {
		r, _, _ := newPromptHarness(t, sseReasoningTurn(t, ""), modes.DefaultID, nil)
		cwd := t.TempDir()
		evs := promptOnceAt(t, r, "s-reason", cwd)
		log := readPersisted(t, r, "s-reason", cwd)

		blocks := blocksAt(t, log, 1)
		if got := blockTypes(blocks); got != "reasoning→text" {
			t.Fatalf("落盘块顺序 = %q，期望 reasoning→text（思考在前，正文在后）", got)
		}
		want := streamedReasoning(evs)
		if want == "" {
			t.Fatal("假网关没产出任何思考事件：这个用例证明不了写侧，先修夹具")
		}
		if blocks[0].Text != want {
			t.Fatalf("落盘的思考正文 = %q，事件流里的是 %q——写进去的不是模型想过的那段",
				blocks[0].Text, want)
		}
		if blocks[1].Text != "我先数一下" {
			t.Fatalf("落盘的正文 = %q", blocks[1].Text)
		}
	})

	t.Run("只有思考：思考块就是这条回答的全部内容", func(t *testing.T) {
		r, _, _ := newPromptHarness(t, acpReasoningOnlyServer(t), modes.DefaultID, nil)
		cwd := t.TempDir()
		evs := promptOnceAt(t, r, "s-reason-only", cwd)
		log := readPersisted(t, r, "s-reason-only", cwd)

		blocks := blocksAt(t, log, 1)
		if got := blockTypes(blocks); got != "reasoning" {
			t.Fatalf("落盘块 = %q，期望 reasoning（正文为空时不能只有一条空 content）", got)
		}
		if want := streamedReasoning(evs); blocks[0].Text != want || want == "" {
			t.Fatalf("落盘的思考正文 = %q，事件流里的是 %q", blocks[0].Text, want)
		}
	})

	t.Run("思考+正文+调用：思考→正文→调用", func(t *testing.T) {
		r, _, _ := newPromptHarness(t, sseReasoningTurn(t, "w"), modes.DefaultID, nil)
		cwd := t.TempDir()
		evs := promptOnceAt(t, r, "s-reason-tool", cwd)
		log := readPersisted(t, r, "s-reason-tool", cwd)

		blocks := blocksAt(t, log, 1)
		if got := blockTypes(blocks); got != "reasoning→text→tool-call" {
			t.Fatalf("落盘块顺序 = %q，期望 reasoning→text→tool-call（与 serve/run/fork 同序）", got)
		}
		if want := streamedReasoning(evs); blocks[0].Text != want || want == "" {
			t.Fatalf("落盘的思考正文 = %q，事件流里的是 %q", blocks[0].Text, want)
		}
		if blocks[2].Name != "w" || blocks[2].ID != "call-1" {
			t.Fatalf("调用块丢了身份：%+v", blocks[2])
		}
	})
}

// TestACPPromptNoReasoningPayloadBytesUnchanged 钉住"不许误伤"。
//
// 口径：**`assistant/message` 的 data 原始字节**（不是整行——行信封里带墙钟 time，
// 两次跑不可能相等；也不是"解出来的结构"，那会把键序/空数组/多余字段一起放过）。
// 基线是没有思考、没有调用、带 usage 的一轮：这正是最常见的那种回答，它落盘长什么样
// 必须与改动前**逐字节**一样。
func TestACPPromptNoReasoningPayloadBytesUnchanged(t *testing.T) {
	r, _, _ := newPromptHarness(t, ssePlainText(t, "好"), modes.DefaultID, nil)
	cwd := t.TempDir()
	evs := promptOnceAt(t, r, "s-plain", cwd)
	log := readPersisted(t, r, "s-plain", cwd)

	raw := assistantDataAt(t, log, 1)
	if raw == nil {
		t.Fatal("没有思考的一轮也必须有 assistant/message")
	}
	// 这一轮确实没有思考：否则这条用例就不是"没有思考的一轮"，基线等于没设。
	if got := streamedReasoning(evs); got != "" {
		t.Fatalf("假网关吐了思考（%q），这条用例就不再是「没有思考的一轮」，基线等于没设", got)
	}
	const want = `{"message":{"content":[{"text":"好","type":"text"}],"role":"assistant"},` +
		`"step":1,"stream":[{"reason":{"kind":"completed"},"type":"finish"}],` +
		`"surfaceOp":"append","turn":1,` +
		`"usage":{"inputTokens":10,"outputTokens":2,"totalTokens":12}}`
	if string(raw) != want {
		t.Fatalf("没有思考的一轮，落盘字节变了。\n实际: %s\n期望: %s", raw, want)
	}
}

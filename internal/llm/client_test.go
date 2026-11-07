package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseServer 起一个假的 OpenAI 兼容端点，按给定的分片回放 SSE。
func sseServer(t *testing.T, chunks ...string) (*Client, *[]Request) {
	return sseServerStatus(t, http.StatusOK, chunks...)
}

func sseServerStatus(t *testing.T, status int, chunks ...string) (*Client, *[]Request) {
	t.Helper()
	var seen []Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		seen = append(seen, req)

		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			f.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "k"), &seen
}

func chunkOf(delta string, finish string) string {
	return fmt.Sprintf(`{"choices":[{"delta":{%s},"finish_reason":%q}]}`, delta, finish)
}

func TestStreamAssemblesContent(t *testing.T) {
	c, seen := sseServer(t,
		chunkOf(`"content":"你"`, ""),
		chunkOf(`"content":"好"`, ""),
		chunkOf("", "stop"),
	)
	var events []Event
	resp, err := c.Stream(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}},
		func(e Event) error { events = append(events, e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "你好" {
		t.Fatalf("内容 = %q", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("finish = %q", resp.FinishReason)
	}
	if len(events) != 3 {
		t.Fatalf("事件数 = %d：%+v", len(events), events)
	}
	if events[0].Kind != EventContent || events[2].Kind != EventDone {
		t.Fatalf("事件顺序不对：%+v", events)
	}
	if (*seen)[0].Stream != true {
		t.Fatal("请求必须带 stream:true")
	}
	if (*seen)[0].StreamOptions == nil || !(*seen)[0].StreamOptions.IncludeUsage {
		t.Fatal("请求必须打开 include_usage")
	}
}

func TestStreamReasoningThenContent(t *testing.T) {
	c, _ := sseServer(t,
		chunkOf(`"reasoning_content":"想"`, ""),
		chunkOf(`"reasoning_content":"一下"`, ""),
		chunkOf(`"content":"答案"`, ""),
		chunkOf("", "stop"),
	)
	var kinds []EventKind
	resp, err := c.Stream(context.Background(), Request{Model: "m"}, func(e Event) error {
		kinds = append(kinds, e.Kind)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Reasoning != "想一下" {
		t.Fatalf("reasoning = %q", resp.Reasoning)
	}
	if resp.Content != "答案" {
		t.Fatalf("content = %q", resp.Content)
	}
	want := []EventKind{EventReasoning, EventReasoning, EventContent, EventDone}
	if strings.Join(kindStrings(kinds), ",") != strings.Join(kindStrings(want), ",") {
		t.Fatalf("事件序列 = %v，期望 %v", kinds, want)
	}
}

func TestStreamAssemblesToolCalls(t *testing.T) {
	c, _ := sseServer(t,
		chunkOf(`"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":""}}]`, ""),
		chunkOf(`"tool_calls":[{"index":0,"function":{"arguments":"{\"file_"}}]`, ""),
		chunkOf(`"tool_calls":[{"index":0,"function":{"arguments":"path\":\"a.txt\"}"}}]`, ""),
		chunkOf("", "tool_calls"),
	)
	resp, err := c.Stream(context.Background(), Request{Model: "m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("工具调用数 = %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "read" {
		t.Fatalf("调用 = %+v", tc)
	}
	if tc.Function.Arguments != `{"file_path":"a.txt"}` {
		t.Fatalf("参数没拼对: %q", tc.Function.Arguments)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", resp.FinishReason)
	}
}

func TestStreamUsage(t *testing.T) {
	c, _ := sseServer(t,
		chunkOf(`"content":"x"`, ""),
		`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`,
	)
	resp, err := c.Stream(context.Background(), Request{Model: "m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.TotalTokens != 13 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

// 401 是"认证"，不是"随便一个 HTTP 错误"——给客户端的说法要能直接去改 key。
func TestStreamHTTPError(t *testing.T) {
	c, _ := sseServerStatus(t, http.StatusUnauthorized)
	_, err := c.Stream(context.Background(), Request{Model: "m"}, nil)
	if err == nil {
		t.Fatal("401 必须报错")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "认证") {
		t.Fatalf("401 应该说认证问题: %v", err)
	}
	// 网关的原始响应体不该出现在给客户端的文案里
	if strings.Contains(err.Error(), "bad key") {
		t.Fatalf("不该把网关响应体原样抛出: %v", err)
	}
}

// 其它 4xx 才把响应体带出来（截断），因为那些原因只能从正文看。
func TestStreamHTTPErrorKeepsBodyForOtherCodes(t *testing.T) {
	c, _ := sseServerStatus(t, http.StatusBadRequest)
	_, err := c.Stream(context.Background(), Request{Model: "m"}, nil)
	if err == nil {
		t.Fatal("400 必须报错")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("应包含状态码: %v", err)
	}
}

func TestStreamMalformedChunk(t *testing.T) {
	c, _ := sseServer(t, `{"choices":[{"delta":{"content":"ok"}}]}`, `{not json`)
	_, err := c.Stream(context.Background(), Request{Model: "m"}, nil)
	if err == nil {
		t.Fatal("坏分片必须报错，不能静默跳过")
	}
}

func TestCallbackErrorStops(t *testing.T) {
	c, _ := sseServer(t,
		chunkOf(`"content":"a"`, ""),
		chunkOf(`"content":"b"`, ""),
	)
	sentinel := fmt.Errorf("下游不要了")
	_, err := c.Stream(context.Background(), Request{Model: "m"}, func(Event) error { return sentinel })
	if err != sentinel {
		t.Fatalf("回调错误应原样返回，得到 %v", err)
	}
}

func TestContextCancel(t *testing.T) {
	c, _ := sseServer(t, chunkOf(`"content":"a"`, ""))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Stream(ctx, Request{Model: "m"}, nil); err == nil {
		t.Fatal("已取消的 ctx 必须报错")
	}
}

// 流在 finish 之前断了（网关重启、代理超时、上游 502 之后直接关连接），
// 不能当成功：半句话会被记成 turn completed、退出码 0。
//
// 判据：整条流必须至少出现过一次 finish_reason 或 [DONE]；两者都没有，
// 说明这一次回答没有「完结」的凭据。半截内容仍然带回来（供落盘与诊断），
// 但调用方必须看到 error。
func TestStreamWithoutFinishOrDoneIsTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"半句\"}}]}\n\n")
		w.(http.Flusher).Flush()
		// handler 返回 = 连接被关掉，没有 finish_reason、没有 [DONE]。
	}))
	t.Cleanup(srv.Close)

	resp, err := New(srv.URL, "k").Stream(context.Background(), Request{Model: "m"}, nil)
	if err == nil {
		t.Fatalf("流没有 finish 就结束，必须报错；实际把 %q 当成了完整回答", resp.Content)
	}
	if !strings.Contains(err.Error(), "不完整") && !strings.Contains(err.Error(), "断开") {
		t.Fatalf("要说清是流断了：%v", err)
	}
	if resp == nil || resp.Content != "半句" {
		t.Fatalf("半截内容也要带回来（落盘/诊断要用），得到 %+v", resp)
	}
}

// 200 + 一条内容都没有：这正是「网关列着模型、上游坏了」的形状。
// 它不是「模型说了句空话」，而是这次调用根本没成。
func TestStreamEmpty200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	_, err := New(srv.URL, "k").Stream(context.Background(), Request{Model: "m"}, nil)
	if err == nil {
		t.Fatal("200 + 空流必须报错，不能记成「assistant 说了空话 + turn completed」")
	}
	if !strings.Contains(err.Error(), "空响应") || !strings.Contains(err.Error(), "dshd models") {
		t.Fatalf("要说清是什么并指路：%v", err)
	}
}

// 有些网关只给 finish_reason、不发 [DONE]。那也算完整，不许误伤。
func TestStreamFinishWithoutDoneIsComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"完整\"},\"finish_reason\":\"stop\"}]}\n\n")
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)

	resp, err := New(srv.URL, "k").Stream(context.Background(), Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("有 finish_reason 就是完整回答：%v", err)
	}
	if resp.Content != "完整" || resp.FinishReason != "stop" {
		t.Fatalf("响应 = %+v", resp)
	}
}

func kindStrings(ks []EventKind) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = string(k)
	}
	return out
}

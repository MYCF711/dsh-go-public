package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// 这组测试用「严格网关」验证 W184 的可选字段协商：它按 OpenAPI schema 校验请求体，
// 多一个不认识的字段就 400/422 拒掉。要求是：
//
//   - 只在我们真的发了那个可选字段、且 400/422 的响应体明确点名它时才回退一次；
//   - 每次只改一个字段，401/403/404/429/5xx 与普通参数错误一律不重试；
//   - 有上界、无死循环；
//   - 回退之后 usage / tool_call / reasoning 的流仍然解析得出来；
//   - 默认请求体逐字节不变。

type wireCall struct {
	raw    []byte
	keys   map[string]json.RawMessage
	path   string
	auth   string
	accept string
}

func (c wireCall) has(key string) bool {
	_, ok := c.keys[key]
	return ok
}

func (c wireCall) field(key string) string {
	raw, ok := c.keys[key]
	if !ok {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func (c wireCall) stringField(t *testing.T, key string) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(c.keys[key], &value); err != nil {
		t.Fatalf("字段 %s 不是字符串: %v", key, err)
	}
	return value
}

// newStrictGateway 起一个会记录每次请求原始体的假网关。
func newStrictGateway(t *testing.T, handle func(w http.ResponseWriter, call wireCall)) (*httptest.Server, func() []wireCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []wireCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		keys := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &keys); err != nil {
			http.Error(w, "body is not a JSON object", http.StatusBadRequest)
			return
		}
		call := wireCall{
			raw: raw, keys: keys, path: r.URL.Path,
			auth: r.Header.Get("Authorization"), accept: r.Header.Get("Accept"),
		}
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
		handle(w, call)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []wireCall {
		mu.Lock()
		defer mu.Unlock()
		out := make([]wireCall, len(calls))
		copy(out, calls)
		return out
	}
}

// writeSSE 回放一段完整的流（含 [DONE]）。
func writeSSE(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	f := w.(http.Flusher)
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
		f.Flush()
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	f.Flush()
}

func rejectJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}

const toolCallFinishDelta = `"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]`

const usageChunk = `{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`

// 默认形状必须与历史实现逐字节相同：没有能力信息时，发出的 JSON 一个字节都不许漂。
func TestDefaultWireBodyIsByteIdentical(t *testing.T) {
	img := MessageImage{MIME: "image/png", DataBase64: "QUJD"}
	req := Request{
		Model: "m",
		Messages: []Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "看图", Images: []MessageImage{img}},
			{Role: "user", Parts: []MessagePart{TextPart("a"), ImagePart(img), TextPart("b")}},
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID: "call_1", Type: "function",
				Function: Func{Name: "read", Arguments: `{"file_path":"a.txt"}`},
			}}},
			{Role: "tool", Content: "ok", ToolCallID: "call_1"},
		},
		Tools: []ToolDef{{
			Type: "function",
			Function: ToolFuncDef{
				Name: "read", Description: "读文件",
				Parameters: json.RawMessage(`{"type":"object"}`),
			},
		}},
		Stream:        true,
		StreamOptions: &StreamOptions{IncludeUsage: true},
		MaxTokens:     42,
		Reasoning:     "high",
	}
	want, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := marshalWireRequest(req, defaultWireProfile())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("默认形状必须逐字节不变\n got=%s\nwant=%s", got, want)
	}
}

// 走真实 Stream 路径的默认请求体：同样是逐字节一致的。
func TestStreamDefaultWireBodyMatchesLegacyMarshal(t *testing.T) {
	resetCapabilityCache()
	srv, calls := newStrictGateway(t, func(w http.ResponseWriter, _ wireCall) {
		writeSSE(w, chunkOf(`"content":"ok"`, ""), chunkOf("", "stop"))
	})
	req := Request{
		Model:     "m",
		Messages:  []Message{{Role: "user", Content: "hi"}},
		Tools:     []ToolDef{{Type: "function", Function: ToolFuncDef{Name: "read"}}},
		Stream:    true,
		MaxTokens: 9,
		Reasoning: "medium",
	}
	expected := req
	expected.StreamOptions = &StreamOptions{IncludeUsage: true}
	want, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := New(srv.URL+"/v1", "k").Stream(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content=%q", resp.Content)
	}
	got := calls()
	if len(got) != 1 {
		t.Fatalf("默认形状不该重试，实际发了 %d 次", len(got))
	}
	if got[0].path != "/v1/chat/completions" {
		t.Fatalf("请求路径 = %q，必须是 /v1/chat/completions", got[0].path)
	}
	if string(got[0].raw) != string(want) {
		t.Fatalf("实际发出的请求体与历史实现不一致\n got=%s\nwant=%s", got[0].raw, want)
	}
}

// 严格网关把 stream_options 当未知参数：去掉它、重发一次就成，且仍是标准字段齐全的请求。
func TestStrictGatewayRejectsStreamOptionsThenSucceedsOnce(t *testing.T) {
	resetCapabilityCache()
	srv, calls := newStrictGateway(t, func(w http.ResponseWriter, call wireCall) {
		if call.has("stream_options") {
			rejectJSON(w, http.StatusBadRequest,
				`{"error":{"message":"Unrecognized request argument supplied: stream_options","param":"stream_options","type":"invalid_request_error"}}`)
			return
		}
		writeSSE(w, chunkOf(`"content":"兼容"`, ""), chunkOf("", "stop"), usageChunk)
	})

	var events []Event
	resp, err := New(srv.URL+"/v1", "k").Stream(context.Background(),
		Request{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 5},
		func(e Event) error { events = append(events, e); return nil })
	if err != nil {
		t.Fatalf("回退一次后应当成功: %v", err)
	}
	if resp.Content != "兼容" || resp.FinishReason != "stop" {
		t.Fatalf("响应 = %+v", resp)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.TotalTokens != 13 {
		t.Fatalf("去掉 stream_options 之后仍要能解析 usage: %+v", resp.Usage)
	}
	got := calls()
	if len(got) != 2 {
		t.Fatalf("应当恰好重发一次，实际 %d 次", len(got))
	}
	if !got[0].has("stream_options") {
		t.Fatal("第一次请求必须带上 stream_options")
	}
	if got[1].has("stream_options") {
		t.Fatalf("重试必须去掉 stream_options: %s", got[1].raw)
	}
	// 标准字段一个都不能丢：model / messages / stream（以及我们发过的 max_tokens）。
	for _, key := range []string{"model", "messages", "stream"} {
		if !got[1].has(key) {
			t.Fatalf("重试丢了标准字段 %s: %s", key, got[1].raw)
		}
	}
	if got[1].stringField(t, "model") != "m" {
		t.Fatalf("重试的 model 变了: %s", got[1].raw)
	}
	if got[1].field("max_tokens") != "5" {
		t.Fatalf("max_tokens 不该被这次回退动到: %s", got[1].raw)
	}
	if got[1].path != "/v1/chat/completions" || got[1].auth != "Bearer k" || got[1].accept != "text/event-stream" {
		t.Fatalf("重试的路径/认证/Accept 不对: path=%q auth=%q accept=%q", got[1].path, got[1].auth, got[1].accept)
	}
	if len(events) == 0 || events[len(events)-1].Kind != EventDone {
		t.Fatalf("事件流没有正常收尾: %+v", events)
	}
}

// 严格网关拒绝 reasoning_effort（422 形状）：去掉它重发；回退之后
// reasoning / content / tool_call / usage 仍然全部解析得出来。
func TestStrictGatewayRejectsReasoningEffortThenStreamsEverything(t *testing.T) {
	resetCapabilityCache()
	srv, calls := newStrictGateway(t, func(w http.ResponseWriter, call wireCall) {
		if call.has("reasoning_effort") {
			rejectJSON(w, http.StatusUnprocessableEntity,
				`{"detail":[{"loc":["body","reasoning_effort"],"msg":"extra fields not permitted","type":"value_error.extra"}]}`)
			return
		}
		writeSSE(w,
			chunkOf(`"reasoning_content":"想"`, ""),
			chunkOf(`"content":"答"`, ""),
			chunkOf(toolCallFinishDelta, ""),
			usageChunk,
			chunkOf("", "tool_calls"),
		)
	})

	resp, err := New(srv.URL+"/v1", "k").Stream(context.Background(), Request{
		Model: "m", Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:     []ToolDef{{Type: "function", Function: ToolFuncDef{Name: "read"}}},
		Reasoning: "high", MaxTokens: 8,
	}, nil)
	if err != nil {
		t.Fatalf("去掉 reasoning_effort 后应当成功: %v", err)
	}
	if resp.Reasoning != "想" || resp.Content != "答" {
		t.Fatalf("回退后 reasoning/content 解析不对: reasoning=%q content=%q", resp.Reasoning, resp.Content)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Name != "read" || resp.FinishReason != "tool_calls" {
		t.Fatalf("回退后 tool_call 解析不对: %+v finish=%q", resp.ToolCalls, resp.FinishReason)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.TotalTokens != 13 {
		t.Fatalf("回退后 usage 解析不对: %+v", resp.Usage)
	}
	got := calls()
	if len(got) != 2 {
		t.Fatalf("应当恰好重发一次，实际 %d 次", len(got))
	}
	if got[1].has("reasoning_effort") {
		t.Fatalf("重试必须去掉 reasoning_effort: %s", got[1].raw)
	}
	if !got[1].has("stream_options") {
		t.Fatalf("这次回退只该动 reasoning_effort，stream_options 必须还在: %s", got[1].raw)
	}
	if got[1].field("max_tokens") != "8" {
		t.Fatalf("max_tokens 不该被这次回退动到: %s", got[1].raw)
	}
}

// 严格网关只认 max_completion_tokens：把 max_tokens 换成官方现行拼写，其余不动。
func TestStrictGatewayRequiresMaxCompletionTokens(t *testing.T) {
	resetCapabilityCache()
	srv, calls := newStrictGateway(t, func(w http.ResponseWriter, call wireCall) {
		if call.has("max_tokens") {
			rejectJSON(w, http.StatusBadRequest,
				`{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","param":"max_tokens","code":"unsupported_parameter"}}`)
			return
		}
		if !call.has("max_completion_tokens") {
			rejectJSON(w, http.StatusBadRequest, `{"error":{"message":"max_completion_tokens is required"}}`)
			return
		}
		writeSSE(w, chunkOf(`"content":"ok"`, ""), chunkOf("", "stop"))
	})

	_, err := New(srv.URL+"/v1", "k").Stream(context.Background(),
		Request{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 42}, nil)
	if err != nil {
		t.Fatalf("改拼写后应当成功: %v", err)
	}
	got := calls()
	if len(got) != 2 {
		t.Fatalf("应当恰好重发一次，实际 %d 次", len(got))
	}
	if got[0].field("max_tokens") != "42" {
		t.Fatalf("第一次必须还是 max_tokens（默认形状不变）: %s", got[0].raw)
	}
	if got[1].has("max_tokens") {
		t.Fatalf("重试必须去掉 max_tokens: %s", got[1].raw)
	}
	if got[1].field("max_completion_tokens") != "42" {
		t.Fatalf("重试必须带 max_completion_tokens=42: %s", got[1].raw)
	}
}

// 响应体点名的不是我们发的可选字段（或根本没发）：一次都不重试。
func TestStrictGatewayUnknownParameterOutsideOurFieldsDoesNotRetry(t *testing.T) {
	resetCapabilityCache()
	cases := []struct {
		name string
		body string
		req  Request
	}{
		{
			name: "未发送的 temperature",
			body: `{"error":{"message":"Unknown parameter: 'temperature'","param":"temperature"}}`,
			req:  Request{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 3, Reasoning: "high"},
		},
		{
			name: "max_tokens 没发却被点名",
			body: `{"error":{"message":"Unsupported parameter: max_tokens"}}`,
			req:  Request{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}},
		},
		{
			name: "tools 是标准字段，绝不回退",
			body: `{"error":{"message":"Unsupported parameter: tools, this model does not support tools"}}`,
			req: Request{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}},
				Tools: []ToolDef{{Type: "function", Function: ToolFuncDef{Name: "read"}}}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetCapabilityCache()
			srv, calls := newStrictGateway(t, func(w http.ResponseWriter, _ wireCall) {
				rejectJSON(w, http.StatusBadRequest, c.body)
			})
			_, err := New(srv.URL+"/v1", "k").Stream(context.Background(), c.req, nil)
			if err == nil {
				t.Fatal("必须原样失败")
			}
			if len(calls()) != 1 {
				t.Fatalf("不该重试，实际发了 %d 次", len(calls()))
			}
			if !strings.Contains(err.Error(), "400") {
				t.Fatalf("错误里要留状态码: %v", err)
			}
		})
	}
}

// 可选字段协商**绝不**在 401/403/404/429/5xx 与普通参数错误上发生——哪怕响应体里
// 点了可选字段的名字。
//
// 429/5xx 与这里其它几个的区别在于「要不要重发同一条请求」：它们是抖动，
// 会被重试层（retry.go）按退避重放；401/403/404/400/422 是确定性拒绝，一次都不重试。
// 两者**都不许**改可选字段：形状没问题，是网关/上游的问题。
func TestStrictGatewayNeverRetriesNonSchemaFailures(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		retryable bool
	}{
		{"401 认证", http.StatusUnauthorized, `{"error":{"message":"unknown field: stream_options"}}`, false},
		{"403 权限", http.StatusForbidden, `{"error":{"message":"unsupported parameter: reasoning_effort"}}`, false},
		{"404 路由", http.StatusNotFound, `{"error":{"message":"unknown parameter max_tokens"}}`, false},
		{"429 限流", http.StatusTooManyRequests, `{"error":{"message":"unknown parameter stream_options, retry later"}}`, true},
		{"500 上游", http.StatusInternalServerError, `{"error":{"message":"unsupported parameter stream_options"}}`, true},
		{"502 代理", http.StatusBadGateway, `{"error":{"message":"unrecognized field max_tokens"}}`, true},
		{"400 值越界", http.StatusBadRequest, `{"error":{"message":"max_tokens must be <= 4096"}}`, false},
		{"400 结构错误", http.StatusBadRequest, `{"error":{"message":"messages[0].role must be user or assistant"}}`, false},
		{"422 值错误", http.StatusUnprocessableEntity, `{"error":{"message":"temperature must be between 0 and 2","param":"temperature"}}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetCapabilityCache()
			srv, calls := newStrictGateway(t, func(w http.ResponseWriter, _ wireCall) {
				rejectJSON(w, c.status, c.body)
			})
			// 毫秒级退避：这里要验的是「发了几次」，不是「真的等了几秒」。
			_, err := fastRetryClient(t, srv.URL+"/v1", "k").Stream(context.Background(), Request{
				Model: "m", Messages: []Message{{Role: "user", Content: "hi"}},
				Reasoning: "high", MaxTokens: 3,
			}, nil)
			if err == nil {
				t.Fatalf("%d 必须报错", c.status)
			}
			// 确定性拒绝只发 1 次；抖动类发满重试上界（1 首发 + 重试），仍然有界。
			want := 1
			if c.retryable {
				want = defaultRetryAttempts
			}
			if len(calls()) != want {
				t.Fatalf("%d 应当发 %d 次，实际发了 %d 次", c.status, want, len(calls()))
			}
			var gwErr *GatewayError
			if !errors.As(err, &gwErr) {
				t.Fatalf("错误类型 = %T", err)
			}
			if gwErr.Attempts != want || len(gwErr.Adjust) != 0 {
				t.Fatalf("不该有回退记录: attempts=%d adjust=%v", gwErr.Attempts, gwErr.Adjust)
			}
		})
	}
}

// 网关坏到没有任何形状能过：次数有上界、每次只改一个字段、最后把试过的回退说清。
func TestStrictGatewayNegotiationTerminates(t *testing.T) {
	resetCapabilityCache()
	srv, calls := newStrictGateway(t, func(w http.ResponseWriter, call wireCall) {
		switch {
		case call.has("stream_options"):
			rejectJSON(w, http.StatusBadRequest, `{"error":{"message":"unknown parameter: stream_options"}}`)
		case call.has("reasoning_effort"):
			rejectJSON(w, http.StatusBadRequest, `{"error":{"message":"unknown parameter: reasoning_effort"}}`)
		case call.has("max_tokens"):
			rejectJSON(w, http.StatusBadRequest, `{"error":{"message":"unsupported parameter: max_tokens"}}`)
		case call.has("max_completion_tokens"):
			rejectJSON(w, http.StatusBadRequest, `{"error":{"message":"unknown parameter: max_completion_tokens"}}`)
		default:
			rejectJSON(w, http.StatusBadRequest, `{"error":{"message":"still broken"}}`)
		}
	})

	_, err := New(srv.URL+"/v1", "k").Stream(context.Background(), Request{
		Model: "m", Messages: []Message{{Role: "user", Content: "hi"}},
		Reasoning: "high", MaxTokens: 3,
	}, nil)
	if err == nil {
		t.Fatal("所有形状都被拒时必须报错")
	}
	got := calls()
	if len(got) > maxWireAttempts {
		t.Fatalf("重试次数超过上界 %d：实际 %d 次", maxWireAttempts, len(got))
	}
	if len(got) != maxWireAttempts {
		t.Fatalf("三个可选字段各能收缩一次，应当恰好 %d 次请求，实际 %d 次", maxWireAttempts, len(got))
	}
	var gwErr *GatewayError
	if !errors.As(err, &gwErr) {
		t.Fatalf("错误类型 = %T", err)
	}
	if gwErr.Attempts != maxWireAttempts || len(gwErr.Adjust) != maxWireAttempts-1 {
		t.Fatalf("回退记录不对: attempts=%d adjust=%v", gwErr.Attempts, gwErr.Adjust)
	}
	for _, want := range []string{"stream_options", "reasoning_effort", "max_completion_tokens"} {
		if !strings.Contains(strings.Join(gwErr.Adjust, "、"), want) {
			t.Fatalf("回退记录缺 %s: %v", want, gwErr.Adjust)
		}
	}
	if !strings.Contains(err.Error(), "已做 3 次可选字段回退") {
		t.Fatalf("错误体要说清试过什么: %v", err)
	}
	// 每次只动一个字段：第二轮还在带 reasoning_effort？不应该——它已经在前一轮被去掉了。
	if got[1].has("stream_options") {
		t.Fatalf("第一轮回退后 stream_options 必须消失: %s", got[1].raw)
	}
	if !got[1].has("reasoning_effort") {
		t.Fatalf("第一轮只该去掉 stream_options: %s", got[1].raw)
	}
}

// 被真正接受过的形状会按 (base, model) 记住：同一端点的后续请求不再先吃一个 400。
func TestNegotiatedShapeIsReusedForSameModel(t *testing.T) {
	resetCapabilityCache()
	srv, calls := newStrictGateway(t, func(w http.ResponseWriter, call wireCall) {
		if call.has("stream_options") {
			rejectJSON(w, http.StatusBadRequest,
				`{"error":{"message":"Unrecognized request argument supplied: stream_options"}}`)
			return
		}
		writeSSE(w, chunkOf(`"content":"ok"`, ""), chunkOf("", "stop"))
	})
	client := New(srv.URL+"/v1", "k")
	req := Request{Model: "remembered-model", Messages: []Message{{Role: "user", Content: "hi"}}}
	for i := 0; i < 2; i++ {
		if _, err := client.Stream(context.Background(), req, nil); err != nil {
			t.Fatalf("第 %d 次请求失败: %v", i+1, err)
		}
	}
	got := calls()
	if len(got) != 3 {
		t.Fatalf("第一次 2 发、第二次 1 发，共 3 次；实际 %d 次", len(got))
	}
	if got[2].has("stream_options") {
		t.Fatalf("记住的形状不该再带 stream_options: %s", got[2].raw)
	}
	// 不同 model 不共享记忆：换一个 model 的第一发仍然是默认形状
	// （所以它还得再吃一次 400，然后才回退）。
	if _, err := client.Stream(context.Background(), Request{
		Model: "other-model", Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil); err != nil {
		t.Fatalf("换 model 的请求失败: %v", err)
	}
	got = calls()
	if len(got) != 5 {
		t.Fatalf("另一个 model 应当重新走一遍 2 发，共 5 次；实际 %d 次", len(got))
	}
	if !got[3].has("stream_options") {
		t.Fatalf("另一个 model 的第一发不该复用别的 model 的形状: %s", got[3].raw)
	}
}

// 能力已知时（官方端点）直接走兼容形状，不必先撞一次 400。
func TestKnownProviderUsesCompatibleShapeWithoutRoundTrip(t *testing.T) {
	resetCapabilityCache()
	srv, calls := newStrictGateway(t, func(w http.ResponseWriter, call wireCall) {
		if !call.has("max_completion_tokens") || call.has("max_tokens") {
			rejectJSON(w, http.StatusBadRequest,
				`{"error":{"message":"Unsupported parameter: 'max_tokens'. Use 'max_completion_tokens' instead.","param":"max_tokens"}}`)
			return
		}
		writeSSE(w, chunkOf(`"content":"ok"`, ""), chunkOf("", "stop"))
	})
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := New("https://api.openai.com/v1", "k")
	client.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.URL.Scheme = u.Scheme
		clone.URL.Host = u.Host
		clone.Host = u.Host
		return http.DefaultTransport.RoundTrip(clone)
	})}
	if _, err := client.Stream(context.Background(), Request{
		Model: "gpt-5-known", Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 7,
	}, nil); err != nil {
		t.Fatalf("已知端点第一次请求就该成功: %v", err)
	}
	got := calls()
	if len(got) != 1 {
		t.Fatalf("能力已知时不该先撞 400，实际发了 %d 次", len(got))
	}
	if !got[0].has("max_completion_tokens") || got[0].has("max_tokens") {
		t.Fatalf("官方端点必须直接发 max_completion_tokens: %s", got[0].raw)
	}
}

// 错误体脱敏，但保留原始可诊断片段。
func TestStrictGatewayErrorBodyIsRedactedAndDiagnosable(t *testing.T) {
	resetCapabilityCache()
	const secret = "super-secret-key-literal"
	srv, _ := newStrictGateway(t, func(w http.ResponseWriter, _ wireCall) {
		rejectJSON(w, http.StatusBadRequest,
			fmt.Sprintf(`{"error":{"message":"unknown parameter: temperature (key %s)"}}`, secret))
	})
	_, err := New(srv.URL+"/v1", secret).Stream(context.Background(), Request{
		Model: "m", Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err == nil {
		t.Fatal("必须报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误体泄露了 credential: %v", err)
	}
	var gwErr *GatewayError
	if !errors.As(err, &gwErr) {
		t.Fatalf("错误类型 = %T", err)
	}
	if !strings.Contains(gwErr.Body, "[redacted]") {
		t.Fatalf("诊断片段里 credential 要替换成 [redacted]: %q", gwErr.Body)
	}
	if !strings.Contains(gwErr.Body, "unknown parameter: temperature") {
		t.Fatalf("诊断片段要保留原始措辞: %q", gwErr.Body)
	}
}

// 协商路径本身要能并发跑（-race 下不能有数据竞争）。
func TestStrictGatewayConcurrentNegotiation(t *testing.T) {
	resetCapabilityCache()
	srv, _ := newStrictGateway(t, func(w http.ResponseWriter, call wireCall) {
		if call.has("stream_options") {
			rejectJSON(w, http.StatusBadRequest,
				`{"error":{"message":"unknown parameter: stream_options"}}`)
			return
		}
		writeSSE(w, chunkOf(`"content":"ok"`, ""), chunkOf("", "stop"))
	})
	client := New(srv.URL+"/v1", "k")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Stream(context.Background(), Request{
				Model: "concurrent-model", Messages: []Message{{Role: "user", Content: "hi"}},
			}, nil)
			if err != nil {
				errs <- err
				return
			}
			if resp.Content != "ok" {
				errs <- fmt.Errorf("content=%q", resp.Content)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发请求失败: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// 诊断请求（models.catalog.test）同样要和严格网关谈：只认 max_completion_tokens
// 的端点上，它必须回退一次，而不是报"这个模型不能用"。
func TestCatalogTestNegotiatesMaxCompletionTokens(t *testing.T) {
	resetCapabilityCache()
	srv, calls := newStrictGateway(t, func(w http.ResponseWriter, call wireCall) {
		if call.has("max_tokens") {
			rejectJSON(w, http.StatusBadRequest,
				`{"error":{"message":"Unsupported parameter: 'max_tokens'. Use 'max_completion_tokens' instead.","param":"max_tokens"}}`)
			return
		}
		if !call.has("max_completion_tokens") {
			rejectJSON(w, http.StatusBadRequest, `{"error":{"message":"max_completion_tokens is required"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"catalog-model","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
	})
	t.Setenv("W184_CATALOG_KEY", "w184-key")
	store := OpenModelCatalog(t.TempDir())
	credential := "w184-key"
	if _, err := store.Upsert(json.RawMessage(fmt.Sprintf(
		`{"id":"catalog-model","api":"openai-completions","baseURL":%q,"apiKeyEnv":"W184_CATALOG_KEY"}`, srv.URL+"/v1",
	)), &credential, nil); err != nil {
		t.Fatal(err)
	}
	result, err := store.Test(context.Background(), "catalog-model", ModelTestOverride{})
	if err != nil || !result.OK {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	got := calls()
	if len(got) != 2 {
		t.Fatalf("应当回退一次，实际 %d 次", len(got))
	}
	if got[0].field("max_tokens") != "1" {
		t.Fatalf("第一发必须还是默认形状: %s", got[0].raw)
	}
	if got[1].has("max_tokens") || got[1].field("max_completion_tokens") != "1" {
		t.Fatalf("第二发必须换成 max_completion_tokens: %s", got[1].raw)
	}
}

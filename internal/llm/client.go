package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client 是一个 OpenAI 兼容端点。
type Client struct {
	BaseURL string // 例如 http://127.0.0.1:3425/v1
	APIKey  string
	HTTP    *http.Client
	// UserAgent 是发给网关的 UA。
	//
	// 不设的话 Go 默认发 `Go-http-client/1.1`，网关的客户端列表里就会出现一个
	// 不知名的 "Go-http-client"——用户看到的第一反应是"这是谁在偷偷调模型"。
	// 这是我们的请求，就该署我们的名（2026-11-03 用户截图反馈）。
	UserAgent string

	// retry 是这条 client 的重试参数；nil = 生产默认（见 retry.go 的 defaultRetryConfig）。
	//
	// 故意不导出：重试的次数/预算/退避只能按 client、按请求各自持有。留一个
	// 全局可写开关，就会出现"A 会话退避过 3 次，B 会话第一次重试就没配额了"。
	retry *retryConfig
}

// New 造一个客户端。HTTP 超时故意设长：流式响应的空闲由 ctx 控制，不由 Transport 控制。
func New(baseURL, apiKey string) *Client {
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	return &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 0, Transport: http.DefaultTransport},
	}
}

// RedactCredential 是 catalog test 与真实 runner 共用的秘密文本清洗。
// 只清洗准备交给日志、Transport 或 HTTP 的文本；credential 本身不进入错误链。
func RedactCredential(message string, credentials ...string) string {
	for _, credential := range credentials {
		if credential != "" {
			message = strings.ReplaceAll(message, credential, "[redacted]")
		}
	}
	return message
}

// Stream 发起一次流式请求。
//
// 事件顺序保证：reasoning 全部先于 content（DeepSeek 系模型的行为），
// tool_call 在 content 之后；onEvent 返回错误即中止。
//
// 请求形状：model/messages/tools/stream 永远原样发出；可选字段的差异由
// compat.go 处理——默认形状逐字节不变，能力已知时直接走兼容形状，只有在
// 400/422 的响应体明确指向某个可选字段时，才做一次最小回退（最多 3 次，
// 每次只动一个字段，已试过的形状不再重试）。
//
// 重试（retry.go）发生在这个循环里、**流开始之前**：429/5xx/连接层瞬时错误按
// 退避重放，401/403/404/400/422 与"已经往外发过字之后"的失败一律不重放。
// 对上层（agent 的 step、serve 的 turn 与 SSE）来说，这里仍然只是**一次**调用：
// 重试不新增 step、不新增帧、不重复写历史。
func (c *Client) Stream(ctx context.Context, req Request, onEvent func(Event) error) (*Response, error) {
	req.Stream = true
	if req.StreamOptions == nil {
		req.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	profile := c.requestProfile(req.Model)
	attempted := map[wireProfile]bool{profile: true}
	var adjustments []string

	// 重试状态只活在这条调用里：两个 Client 实例、两条并发流各算各的。
	state := newRetryState(c.retryPolicy())
	// profileTries 只数"形状尝试"（首发 + 可选字段回退），与重试次数分开记账：
	// 触发了重试不代表协商的上界也该跟着放宽。
	profileTries := 1
	// emitted 记录这条流是否真的往外发过事件；发过之后绝不重放。
	emitted := 0

	for {
		// 已取消/已超时就别再发新请求了：保留 ctx 的原因，别把它包成"连不上网关"。
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		body, err := marshalWireRequest(req, profile)
		if err != nil {
			return nil, fmt.Errorf("序列化请求失败: %w", err)
		}
		state.sent++
		resp, err := c.post(ctx, body)
		if err != nil {
			// 连接层失败。错误本身已经是 GatewayError(GatewayConnect)，这里补上
			// "共发了几次"，再看这次抖动要不要重放。
			markAttempts(err, state.sent)
			if !transientConnectError(err) {
				return nil, err
			}
			wait, ok := state.plan(0)
			if !ok {
				return nil, err
			}
			if waitErr := state.wait(ctx, wait); waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		if resp.StatusCode == http.StatusOK {
			// 记下被这个端点真正接受过的形状：同一端点的后续请求（包括别的
			// Client 实例）不必再吃一次 400。
			rememberCapability(c.BaseURL, req.Model, profile)
			snap, err := consumeStream(resp, countingOnEvent(onEvent, &emitted))
			if err == nil {
				return snap, nil
			}
			// 已经往外发过字：绝不重放（半句话不能拼成两段）。一条内容都没发过
			// 时的瞬时网络错误（连接被重置/对端关连接）可以重来一次。
			if emitted > 0 || !transientConnectError(err) {
				return snap, err
			}
			wait, ok := state.plan(0)
			if !ok {
				return snap, err
			}
			if waitErr := state.wait(ctx, wait); waitErr != nil {
				// 这一轮半成品不是回答：重试被取消时不能把它交回去当结果。
				return nil, waitErr
			}
			continue
		}

		// 网关的响应体可能有几 KB（HTML 错误页、堆栈、内部主机名）。先读到一个
		// 上限：协商要看里面点了哪个参数，交给客户端的只是截断后的片段。
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		// 头必须在关 body 之前取；解析失败就当没给（走自身退避）。
		retryAfter, _ := parseRetryAfter(resp.Header, time.Now())
		_ = resp.Body.Close()

		if next, change, ok := negotiateWire(profile, sentFields(req, profile), resp.StatusCode, string(raw)); ok &&
			profileTries < maxWireAttempts && !attempted[next] {
			profile = next
			attempted[next] = true
			profileTries++
			adjustments = append(adjustments, change)
			continue
		}

		gwErr := &GatewayError{
			Kind: GatewayStatus, Base: c.BaseURL,
			Code: resp.StatusCode, Body: errorSnippet(raw, c.APIKey),
			Attempts: state.sent, Adjust: adjustments,
		}
		// 429/5xx 是"可能只是抖了一下"：按退避与 Retry-After 重放；预算/次数/
		// 单次等待任一超界就立刻把这条错误交出去（不吞错、不把 429 说成成功）。
		if retryableStatus(resp.StatusCode) {
			if wait, ok := state.plan(retryAfter); ok {
				if waitErr := state.wait(ctx, wait); waitErr != nil {
					return nil, waitErr
				}
				continue
			}
		}
		return nil, gwErr
	}
}

// post 只发**一次**请求，自己不认识重试：429/5xx 的重放、连接层抖动的重放
// 统一在 Stream 里决定（那里才知道"流有没有开始往外发字"）。
//
// 这层保持"一次调用 = 一次 HTTP 请求"，重试次数才有一个唯一记账点
// （retryState.sent），不至于两处各数一遍。
func (c *Client) post(ctx context.Context, body []byte) (*http.Response, error) {
	url := strings.TrimSuffix(c.BaseURL, "/") + "/chat/completions"
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "text/event-stream")
	ua := c.UserAgent
	if ua == "" {
		ua = "dshd (dsh-go)"
	}
	hr.Header.Set("User-Agent", ua)
	if c.APIKey != "" {
		hr.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTP.Do(hr)
	if err != nil {
		// 这条错误会原样出现在 ACP 客户端上。原来带的是完整 URL 和 Go 的原始
		// 网络错误（`dial tcp ... connectex: ...`），客户端拿到只会觉得莫名其妙。
		// 这里只留"连不上哪个网关"，底层原因用 %w 留给日志。
		return nil, &GatewayError{Kind: GatewayConnect, Base: c.BaseURL, Err: err}
	}
	return resp, nil
}

// consumeStream 读 200 的 SSE 响应并拼成完整响应。
func consumeStream(resp *http.Response, onEvent func(Event) error) (*Response, error) {
	defer resp.Body.Close()

	acc := newAccumulator(onEvent)
	// 整条流必须有一个「完结」的凭据：finish_reason 或 `[DONE]`。
	//
	// 只看「有没有解析出错」不够：网关/代理在回答中途断掉时，连接是**正常关闭**的，
	// readSSE 返回 nil。于是半句话会被当成完整回答记进会话，turn 标 completed、
	// 退出码 0——1 字节错误都没有。真机踩过这种形状（上游 502 后网关直接关流）。
	sawDone := false
	if err := readSSE(resp.Body, func(payload []byte) error {
		if bytes.Equal(payload, []byte("[DONE]")) {
			sawDone = true
			return nil
		}
		var chunk chunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			return fmt.Errorf("SSE 分片不是合法 JSON: %w", err)
		}
		return acc.feed(chunk)
	}); err != nil {
		return acc.snapshot(), err
	}
	snap := acc.snapshot()
	if !sawDone && snap.FinishReason == "" {
		// 分两种说清，因为下一步完全不同。
		if snap.Content == "" && snap.Reasoning == "" && len(snap.ToolCalls) == 0 {
			return snap, fmt.Errorf("模型返回了空响应（HTTP 200，但流里一条内容都没有）：" +
				"这个模型 id 可能不可用——用 `dshd models` 看有哪些、或在 --model 上换一个；不要在这上面反复重试")
		}
		return snap, fmt.Errorf("模型流在 finish 之前断开了（已收到 %d 字正文，但没有 finish_reason 也没有 [DONE]）："+
			"这次回答不完整，先重试一次；反复出现就查网关/代理的超时", len([]rune(snap.Content)))
	}
	acc.emitDone()
	return snap, nil
}

// readSSE 解析 text/event-stream：以空行分隔事件，data: 后是负载，':' 开头是注释。
func readSSE(r io.Reader, onData func([]byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var data []string
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		return onData([]byte(payload))
	}

	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
			// 注释/心跳
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			// event:/id:/retry: 之类的字段目前用不到，忽略。
		}
	}
	if err := flush(); err != nil {
		return err
	}
	return sc.Err()
}

// chunk 是 SSE 里的一片。只声明我们真正用到的字段。
type chunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

// accumulator 把碎片拼成完整响应，并往外发事件。
type accumulator struct {
	emit func(Event) error

	content      strings.Builder
	reasoning    strings.Builder
	order        []int
	calls        map[int]*ToolCall
	usage        Usage
	finishReason string
	cbErr        error
}

func newAccumulator(emit func(Event) error) *accumulator {
	if emit == nil {
		emit = func(Event) error { return nil }
	}
	return &accumulator{emit: emit, calls: make(map[int]*ToolCall)}
}

func (a *accumulator) feed(c chunk) error {
	if a.cbErr != nil {
		return a.cbErr
	}
	if c.Usage != nil {
		a.usage = *c.Usage
	}
	for _, ch := range c.Choices {
		d := ch.Delta
		reasoning := d.ReasoningContent
		if reasoning == "" {
			reasoning = d.Reasoning
		}
		if reasoning != "" {
			a.reasoning.WriteString(reasoning)
			a.forward(Event{Kind: EventReasoning, Text: reasoning})
		}
		if d.Content != "" {
			a.content.WriteString(d.Content)
			a.forward(Event{Kind: EventContent, Text: d.Content})
		}
		for _, tc := range d.ToolCalls {
			call, ok := a.calls[tc.Index]
			if !ok {
				call = &ToolCall{Index: tc.Index}
				a.calls[tc.Index] = call
				a.order = append(a.order, tc.Index)
			}
			if tc.ID != "" {
				call.ID = tc.ID
			}
			if tc.Type != "" {
				call.Type = tc.Type
			}
			if tc.Function.Name != "" {
				call.Function.Name += tc.Function.Name
			}
			call.Function.Arguments += tc.Function.Arguments
			a.forward(Event{Kind: EventToolCall, ToolCall: &ToolCall{
				Index: tc.Index, ID: tc.ID, Type: tc.Type,
				Function: Func{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
			}})
		}
		if ch.FinishReason != "" {
			a.finishReason = ch.FinishReason
		}
	}
	return a.cbErr
}

func (a *accumulator) forward(ev Event) {
	if a.cbErr != nil || a.emit == nil {
		return
	}
	a.cbErr = a.emit(ev)
}

func (a *accumulator) emitDone() {
	if a.finishReason != "" {
		a.forward(Event{Kind: EventDone, FinishReason: a.finishReason})
	}
}

func (a *accumulator) snapshot() *Response {
	resp := &Response{
		Content:      a.content.String(),
		Reasoning:    a.reasoning.String(),
		FinishReason: a.finishReason,
		Usage:        a.usage,
	}
	for _, i := range a.order {
		resp.ToolCalls = append(resp.ToolCalls, *a.calls[i])
	}
	return resp
}

// Timeout 是个小工具：给一次调用套一个上限（ACP/agent 层用得上）。
func Timeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

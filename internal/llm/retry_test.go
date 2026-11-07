package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// 这一组用例守的是「网关抖一下不等于这一轮失败」。
//
// 旧实现（client.go 里 post 的注释）写得很清楚：401/403/404/429/5xx 只发一次。
// 结果是 magpie/上游一抖，整轮 turn 直接红——用户看到的是「模型调用失败」，
// 而真实原因只是那一次 429 或 502。
//
// 重试只对**可安全重放**的请求做：chat/completions 是只读推理调用，重放不会
// 产生副作用；但**流已经开始往外吐字之后**绝不重放——那会把半句话拼成两段。

// retryProbe 是「这条请求一共发了几次」的计数器 + 可编排的假网关。
type retryProbe struct {
	calls int32
	srv   *httptest.Server
}

func (p *retryProbe) count() int { return int(atomic.LoadInt32(&p.calls)) }

// newRetryProbe 起一个假网关；handle 拿到「第几次请求」（从 1 起）并写响应。
func newRetryProbe(t *testing.T, handle func(w http.ResponseWriter, attempt int)) *retryProbe {
	t.Helper()
	probe := &retryProbe{}
	probe.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := int(atomic.AddInt32(&probe.calls, 1))
		handle(w, attempt)
	}))
	t.Cleanup(probe.srv.Close)
	return probe
}

// 429 是「稍等再试」，不是「这一轮到此为止」。
//
// 旧实现：只发 1 次，调用方拿到的是一条「模型网关限流（429）」——
// 网关抖一下，整轮就没了。新行为：按 429 退避重试，第二次成功就照常返回回答。
func TestStreamRetries429ThenSucceeds(t *testing.T) {
	probe := newRetryProbe(t, func(w http.ResponseWriter, attempt int) {
		if attempt == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		writeSSE(w, chunkOf(`"content":"稳住了"`, "stop"))
	})

	var texts []string
	resp, err := New(probe.srv.URL, "k").Stream(context.Background(), Request{Model: "m"},
		func(e Event) error {
			if e.Kind == EventContent {
				texts = append(texts, e.Text)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("429 之后成功必须拿到回答，而不是把 429 报成最终失败：%v（一共发了 %d 次）", err, probe.count())
	}
	if probe.count() != 2 {
		t.Fatalf("429 应该重试一次，实际发了 %d 次", probe.count())
	}
	if resp == nil || resp.Content != "稳住了" {
		t.Fatalf("响应 = %+v", resp)
	}
	// 重放发生在"流还没开始"之前，所以下游看到的内容只能有一份。
	if len(texts) != 1 || texts[0] != "稳住了" {
		t.Fatalf("重试不许把内容吐两遍，实际收到 %q", texts)
	}
}

// 5xx 是网关/上游自己的问题，同样可以安全重放；连续两次抖动之后仍要成功。
func TestStreamRetries5xxThenSucceeds(t *testing.T) {
	probe := newRetryProbe(t, func(w http.ResponseWriter, attempt int) {
		if attempt <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"error":{"message":"upstream boom"}}`)
			return
		}
		writeSSE(w, chunkOf(`"content":"好了"`, "stop"))
	})

	resp, err := New(probe.srv.URL, "k").Stream(context.Background(), Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("5xx 之后成功必须拿到回答：%v（一共发了 %d 次）", err, probe.count())
	}
	if probe.count() != 3 {
		t.Fatalf("两次 502 应该各重试一次，实际发了 %d 次", probe.count())
	}
	if resp == nil || resp.Content != "好了" {
		t.Fatalf("响应 = %+v", resp)
	}
}

// 连接层瞬时错误（对端直接关连接 → EOF）也算可重放：请求根本没被处理完。
func TestStreamRetriesTransientConnectError(t *testing.T) {
	probe := newRetryProbe(t, func(w http.ResponseWriter, attempt int) {
		if attempt == 1 {
			// 一个字节都不回就断开：client.Do 拿到 EOF。
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack 失败: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		writeSSE(w, chunkOf(`"content":"重连成"`, "stop"))
	})

	resp, err := New(probe.srv.URL, "k").Stream(context.Background(), Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("连接被断开后重连成功必须拿到回答：%v（一共发了 %d 次）", err, probe.count())
	}
	if probe.count() != 2 {
		t.Fatalf("连接层瞬时错误应该重试一次，实际发了 %d 次", probe.count())
	}
	if resp == nil || resp.Content != "重连成" {
		t.Fatalf("响应 = %+v", resp)
	}
}

// 401/403/404/400/422 是**确定性**拒绝：重试只会把同一个错误重放 4 次，
// 既浪费时间又掩盖真实原因（key 错了 / 路径错了 / 参数错了）。
func TestStreamNeverRetriesDeterministicRejections(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusBadRequest,
		http.StatusUnprocessableEntity,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			probe := newRetryProbe(t, func(w http.ResponseWriter, _ int) {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":{"message":"nope"}}`)
			})
			_, err := New(probe.srv.URL, "k").Stream(context.Background(), Request{Model: "m"}, nil)
			if err == nil {
				t.Fatalf("%d 必须报错", status)
			}
			if probe.count() != 1 {
				t.Fatalf("%d 是确定性拒绝，绝不该重试；实际发了 %d 次", status, probe.count())
			}
			if gw := asGatewayError(t, err); gw.Attempts != 1 {
				t.Fatalf("%d 的尝试次数 = %d", status, gw.Attempts)
			}
		})
	}
}

// 流已经开始往外吐字之后失败：**不许重放**。
//
// 重放会把「半句」和「重来的一整句」拼成两段，用户看到重复输出，
// 而会话里那条 assistant 消息会同时含有两遍内容。
func TestStreamDoesNotRetryAfterOutputStarted(t *testing.T) {
	probe := newRetryProbe(t, func(w http.ResponseWriter, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", chunkOf(`"content":"半句"`, ""))
		f.Flush()
		// handler 直接返回 = 连接关闭：没有 finish_reason、没有 [DONE]。
	})

	resp, err := New(probe.srv.URL, "k").Stream(context.Background(), Request{Model: "m"}, nil)
	if err == nil {
		t.Fatal("流没有 finish 就断了，必须报错")
	}
	if probe.count() != 1 {
		t.Fatalf("已经输出过内容，绝不能重放；实际发了 %d 次", probe.count())
	}
	if resp == nil || resp.Content != "半句" {
		t.Fatalf("半截内容仍要带回来（落盘/诊断要用），得到 %+v", resp)
	}
}

// 重试耗尽后：错误仍是**同一类**（*GatewayError + 原状态码，四字段契约照旧可翻译），
// 并且带上是几次尝试；429 绝不能被报成成功。
func TestStreamRetryExhaustedKeepsGatewayError(t *testing.T) {
	probe := newRetryProbe(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"still limited"}}`)
	})

	c := fastRetryClient(t, probe.srv.URL, "k")
	c.retry.sleep = (&recordingSleep{}).sleep

	_, err := c.Stream(context.Background(), Request{Model: "m"}, nil)
	if err == nil {
		t.Fatal("429 耗尽重试后仍然是失败，不能被报成成功")
	}
	gw := asGatewayError(t, err)
	if gw.Code != http.StatusTooManyRequests || gw.Kind != GatewayStatus {
		t.Fatalf("错误类别被换掉了: %+v", gw)
	}
	if gw.Attempts != probe.count() || gw.Attempts < 2 {
		t.Fatalf("错误要带上真实尝试次数：attempts=%d，实际发出 %d 次", gw.Attempts, probe.count())
	}
	// 文案仍是原来那句话（errors.go 的语义不动），客户端照旧能去查 key/限流。
	if !contains(gw.Error(), "限流") {
		t.Fatalf("429 的文案要保留「限流」：%q", gw.Error())
	}
}

// ---- 可注入的重试参数：用例不真的睡几秒 ----

// fastRetryClient 造一条毫秒级退避的 client。
//
// 生产默认是 500ms 起、最坏额外等 3.5 秒；用例要的是「断言退避的形状」，
// 不是「等 3.5 秒」，所以这里把参数压到毫秒。参数挂在 Client 自己身上——
// 不导出，也不存在包级开关。
func fastRetryClient(t *testing.T, baseURL, key string) *Client {
	t.Helper()
	c := New(baseURL, key)
	cfg := defaultRetryConfig()
	cfg.base = time.Millisecond
	cfg.maxWait = 20 * time.Millisecond
	cfg.budget = time.Second
	cfg.jitter = 0
	c.retry = &cfg
	return c
}

// recordingSleep 把「这次重试打算等多久」记下来，但立刻返回，不真的等。
type recordingSleep struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (r *recordingSleep) sleep(_ context.Context, d time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waits = append(r.waits, d)
	return nil
}

func (r *recordingSleep) all() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]time.Duration, len(r.waits))
	copy(out, r.waits)
	return out
}

// 429 带的 Retry-After 是**下限**：服务端说等 2 秒，就不能 1 毫秒后再撞一次。
func TestStreamRetryHonorsRetryAfterSeconds(t *testing.T) {
	probe := newRetryProbe(t, func(w http.ResponseWriter, attempt int) {
		if attempt == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"slow down"}}`)
			return
		}
		writeSSE(w, chunkOf(`"content":"等到了"`, "stop"))
	})
	rec := &recordingSleep{}
	c := fastRetryClient(t, probe.srv.URL, "k")
	c.retry.sleep = rec.sleep
	// 单次等待上限与总预算都要抬到 Retry-After 之上，否则会按「等不起」直接失败。
	c.retry.maxWait = 10 * time.Second
	c.retry.budget = time.Minute

	if _, err := c.Stream(context.Background(), Request{Model: "m"}, nil); err != nil {
		t.Fatalf("重试后应当成功：%v", err)
	}
	waits := rec.all()
	if len(waits) != 1 {
		t.Fatalf("应当只重试一次，实际等待 %v", waits)
	}
	if waits[0] < 2*time.Second {
		t.Fatalf("必须尊重 Retry-After: 2s，实际等了 %v", waits[0])
	}
}

// HTTP-date 形式的 Retry-After 同样要认。
func TestStreamRetryHonorsRetryAfterHTTPDate(t *testing.T) {
	deadline := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	probe := newRetryProbe(t, func(w http.ResponseWriter, attempt int) {
		if attempt == 1 {
			w.Header().Set("Retry-After", deadline)
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"message":"maintenance"}}`)
			return
		}
		writeSSE(w, chunkOf(`"content":"恢复了"`, "stop"))
	})
	rec := &recordingSleep{}
	c := fastRetryClient(t, probe.srv.URL, "k")
	c.retry.sleep = rec.sleep
	// 单次等待上限与总预算都要抬到 Retry-After 之上，否则会按「等不起」直接失败。
	c.retry.maxWait = 10 * time.Second
	c.retry.budget = time.Minute

	if _, err := c.Stream(context.Background(), Request{Model: "m"}, nil); err != nil {
		t.Fatalf("重试后应当成功：%v", err)
	}
	waits := rec.all()
	if len(waits) != 1 {
		t.Fatalf("应当只重试一次，实际等待 %v", waits)
	}
	// HTTP-date 只有秒精度，允许 1 秒的解析误差。
	if waits[0] < 1500*time.Millisecond || waits[0] > 3500*time.Millisecond {
		t.Fatalf("应当等约 3 秒，实际 %v", waits[0])
	}
}

// Retry-After 大得离谱（一小时后再来）：不在这个 turn 里等，立刻把 429 原样交出去。
// 提前重试等于不尊重它，阻塞一小时更荒谬——所以只能「不重试」。
func TestStreamRetryAfterBeyondCapStops(t *testing.T) {
	probe := newRetryProbe(t, func(w http.ResponseWriter, _ int) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"quota exhausted"}}`)
	})
	rec := &recordingSleep{}
	c := fastRetryClient(t, probe.srv.URL, "k")
	c.retry.sleep = rec.sleep

	_, err := c.Stream(context.Background(), Request{Model: "m"}, nil)
	if err == nil {
		t.Fatal("429 不能被报成成功")
	}
	if probe.count() != 1 {
		t.Fatalf("Retry-After 超过上限就不该重试，实际发了 %d 次", probe.count())
	}
	if len(rec.all()) != 0 {
		t.Fatalf("不该等：%v", rec.all())
	}
	if gw := asGatewayError(t, err); gw.Code != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d", gw.Code)
	}
}

// 总预算有界：每次都让等 5 秒、预算 6 秒——第二次就超了，立刻停。
func TestStreamRetryBudgetBounds(t *testing.T) {
	probe := newRetryProbe(t, func(w http.ResponseWriter, _ int) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"message":"busy"}}`)
	})
	rec := &recordingSleep{}
	c := fastRetryClient(t, probe.srv.URL, "k")
	c.retry.sleep = rec.sleep
	c.retry.maxWait = 10 * time.Second
	c.retry.budget = 6 * time.Second

	_, err := c.Stream(context.Background(), Request{Model: "m"}, nil)
	if err == nil {
		t.Fatal("预算耗尽后必须报错")
	}
	if probe.count() != 2 {
		t.Fatalf("预算只够重试一次，实际发了 %d 次", probe.count())
	}
	if got := rec.all(); len(got) != 1 || got[0] != 5*time.Second {
		t.Fatalf("等待计划 = %v", got)
	}
	gw := asGatewayError(t, err)
	if gw.Attempts != 2 || gw.Code != http.StatusServiceUnavailable {
		t.Fatalf("错误 = %+v", gw)
	}
}

// 抖动只往下抖：第 n 次重试落在 [(1-j)*d, d]，绝不顶破上限。
func TestStreamRetryJitterStaysInsideBounds(t *testing.T) {
	probe := newRetryProbe(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	})
	rec := &recordingSleep{}
	c := fastRetryClient(t, probe.srv.URL, "k")
	c.retry.sleep = rec.sleep
	c.retry.base = 100 * time.Millisecond
	c.retry.maxWait = 10 * time.Second
	c.retry.budget = time.Minute
	c.retry.jitter = 0.5

	if _, err := c.Stream(context.Background(), Request{Model: "m"}, nil); err == nil {
		t.Fatal("一直 500 必须报错")
	}
	waits := rec.all()
	if len(waits) != defaultRetryAttempts-1 {
		t.Fatalf("应当重试 %d 次，实际 %v", defaultRetryAttempts-1, waits)
	}
	for i, w := range waits {
		d := 100 * time.Millisecond << uint(i)
		low := d / 2
		if w < low || w > d {
			t.Fatalf("第 %d 次退避 = %v，应落在 [%v, %v]", i+1, w, low, d)
		}
	}
}

// 次数有界：一直 500 时恰好发 defaultRetryAttempts 次，然后如实报错。
func TestStreamRetryAttemptsBounded(t *testing.T) {
	probe := newRetryProbe(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	})
	rec := &recordingSleep{}
	c := fastRetryClient(t, probe.srv.URL, "k")
	c.retry.sleep = rec.sleep

	_, err := c.Stream(context.Background(), Request{Model: "m"}, nil)
	if err == nil {
		t.Fatal("必须报错")
	}
	if probe.count() != defaultRetryAttempts {
		t.Fatalf("尝试次数应当恰好 %d，实际 %d", defaultRetryAttempts, probe.count())
	}
	if gw := asGatewayError(t, err); gw.Attempts != defaultRetryAttempts {
		t.Fatalf("错误里的尝试次数 = %d", gw.Attempts)
	}
}

// 连接层一直失败：错误仍要带上尝试次数（否则上层只看到「连不上」，不知道试过几次）。
func TestStreamConnectErrorExhaustedKeepsGatewayError(t *testing.T) {
	probe := newRetryProbe(t, func(w http.ResponseWriter, _ int) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack 失败: %v", err)
			return
		}
		_ = conn.Close()
	})
	rec := &recordingSleep{}
	c := fastRetryClient(t, probe.srv.URL, "k")
	c.retry.sleep = rec.sleep

	_, err := c.Stream(context.Background(), Request{Model: "m"}, nil)
	if err == nil {
		t.Fatal("连不上必须报错")
	}
	gw := asGatewayError(t, err)
	if gw.Kind != GatewayConnect {
		t.Fatalf("错误类别 = %v", gw.Kind)
	}
	if gw.Attempts != defaultRetryAttempts || probe.count() != defaultRetryAttempts {
		t.Fatalf("attempts=%d，实际发出 %d 次", gw.Attempts, probe.count())
	}
}

// 取消是即时的：正在退避时按下取消，必须当场返回，不再发下一个请求。
func TestStreamCtxCancelDuringBackoff(t *testing.T) {
	probe := newRetryProbe(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"limited"}}`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := fastRetryClient(t, probe.srv.URL, "k")
	c.retry.sleep = func(_ context.Context, _ time.Duration) error {
		cancel() // 用户在这一刻按了取消
		return ctx.Err()
	}

	started := time.Now()
	_, err := c.Stream(ctx, Request{Model: "m"}, nil)
	if err == nil {
		t.Fatal("取消必须报错")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消要能 errors.Is(context.Canceled) 认出来，实际 %v", err)
	}
	if probe.count() != 1 {
		t.Fatalf("取消之后不许再发请求，实际发了 %d 次", probe.count())
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("取消要即时返回，实际花了 %v", elapsed)
	}
}

// 连请求都还没发就已经取消：一个请求都不该发。
func TestStreamCtxAlreadyCancelledSendsNothing(t *testing.T) {
	probe := newRetryProbe(t, func(w http.ResponseWriter, _ int) {
		writeSSE(w, chunkOf(`"content":"不该到这里"`, "stop"))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := New(probe.srv.URL, "k").Stream(ctx, Request{Model: "m"}, nil)
	if err == nil {
		t.Fatal("已取消的 ctx 必须报错")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应当是 context.Canceled，实际 %v", err)
	}
	if probe.count() != 0 {
		t.Fatalf("已取消就不该发请求，实际发了 %d 次", probe.count())
	}
}

// A/B 两个 client 实例互不串状态：A 重试过，B 的第一次失败仍然是第一次。
func TestStreamRetryStateIsPerClient(t *testing.T) {
	probeA := newRetryProbe(t, func(w http.ResponseWriter, attempt int) {
		if attempt == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"limited"}}`)
			return
		}
		writeSSE(w, chunkOf(`"content":"A"`, "stop"))
	})
	probeB := newRetryProbe(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
	})

	if _, err := fastRetryClient(t, probeA.srv.URL, "k").Stream(context.Background(), Request{Model: "m"}, nil); err != nil {
		t.Fatalf("A 应当重试后成功：%v", err)
	}
	_, errB := fastRetryClient(t, probeB.srv.URL, "k").Stream(context.Background(), Request{Model: "m"}, nil)
	if errB == nil {
		t.Fatal("B 必须报错（401 不重试）")
	}
	if probeA.count() != 2 {
		t.Fatalf("A 应当发 2 次，实际 %d", probeA.count())
	}
	if probeB.count() != 1 {
		t.Fatalf("B 应当发 1 次，实际 %d", probeB.count())
	}
	if gw := asGatewayError(t, errB); gw.Attempts != 1 {
		t.Fatalf("B 的尝试次数 = %d", gw.Attempts)
	}
}

// 同一个 Client 上的并发流互不串状态：每条流各自退避一次，计数与结果都不许串。
func TestStreamConcurrentRetriesDoNotShareState(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		seen[req.Model]++
		n := seen[req.Model]
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"limited"}}`)
			return
		}
		writeSSE(w, chunkOf(`"content":"ok"`, "stop"))
	}))
	t.Cleanup(srv.Close)

	c := fastRetryClient(t, srv.URL, "k")
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := c.Stream(context.Background(), Request{Model: fmt.Sprintf("model-%d", i)}, nil)
			if err == nil && (resp == nil || resp.Content != "ok") {
				err = fmt.Errorf("响应不对: %+v", resp)
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 条流失败：%v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != n {
		t.Fatalf("应当有 %d 条独立模型，实际 %v", n, seen)
	}
	for model, count := range seen {
		if count != 2 {
			t.Fatalf("%s 应当发 2 次（1 次 429 + 1 次成功），实际 %d", model, count)
		}
	}
}

// ---- 分类器单测（重试与否的判据只有这一份） ----

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		value  string
		want   time.Duration
		wantOK bool
	}{
		{name: "没给", value: "", wantOK: false},
		{name: "秒", value: "5", want: 5 * time.Second, wantOK: true},
		{name: "小数秒", value: "0.5", want: 500 * time.Millisecond, wantOK: true},
		{name: "0", value: "0", want: 0, wantOK: true},
		{name: "负数当没给", value: "-3", wantOK: false},
		{name: "HTTP-date", value: now.Add(30 * time.Second).Format(http.TimeFormat), want: 30 * time.Second, wantOK: true},
		{name: "过期的 HTTP-date", value: now.Add(-time.Hour).Format(http.TimeFormat), want: 0, wantOK: true},
		{name: "看不懂就当没给", value: "soon-ish", wantOK: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			if c.value != "" {
				h.Set("Retry-After", c.value)
			}
			got, ok := parseRetryAfter(h, now)
			if ok != c.wantOK || got != c.want {
				t.Fatalf("parseRetryAfter(%q) = (%v, %v)，期望 (%v, %v)", c.value, got, ok, c.want, c.wantOK)
			}
		})
	}
}

// timeoutError 是 net.Error 的超时实现（用来证明「超时不重试」）。
type timeoutError struct{}

func (timeoutError) Error() string   { return "read tcp 127.0.0.1:1: i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestTransientConnectError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"EOF", io.EOF, true},
		{"未预期的 EOF", io.ErrUnexpectedEOF, true},
		{"ECONNRESET", syscall.ECONNRESET, true},
		{"连接被重置（文本）", errors.New("read tcp 127.0.0.1:1: connection reset by peer"), true},
		{"Winsock 强关（文本）", errors.New("wsarecv: An existing connection was forcibly closed by the remote host"), true},
		{"代理关空闲连接（文本）", errors.New("server closed idle connection"), true},
		{"ctx 取消", context.Canceled, false},
		{"ctx 超时", context.DeadlineExceeded, false},
		{"网络超时", timeoutError{}, false},
		{"DNS 找不到", &net.DNSError{Err: "no such host", Name: "example.invalid"}, false},
		{"随便一个业务错误", errors.New("SSE 分片不是合法 JSON"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := transientConnectError(c.err); got != c.want {
				t.Fatalf("transientConnectError(%v) = %v，期望 %v", c.err, got, c.want)
			}
		})
	}
}

func TestRetryableStatus(t *testing.T) {
	cases := map[int]bool{
		http.StatusTooManyRequests:     true,
		http.StatusInternalServerError: true,
		http.StatusBadGateway:          true,
		http.StatusServiceUnavailable:  true,
		http.StatusUnauthorized:        false,
		http.StatusForbidden:           false,
		http.StatusNotFound:            false,
		http.StatusBadRequest:          false,
		http.StatusUnprocessableEntity: false,
	}
	for status, want := range cases {
		if got := retryableStatus(status); got != want {
			t.Fatalf("retryableStatus(%d) = %v，期望 %v", status, got, want)
		}
	}
}

// asGatewayError 断言错误链里还是那个 *GatewayError（四字段契约的翻译入口）。
func asGatewayError(t *testing.T, err error) *GatewayError {
	t.Helper()
	var gw *GatewayError
	if !errors.As(err, &gw) || gw == nil {
		t.Fatalf("错误类型 = %T（%v）：必须仍是 *GatewayError，否则协议层翻不成四字段", err, err)
	}
	return gw
}

package llm

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// 这一层是 0.2.2 `llm-retry`（官方编号 20）的宿主实现。
//
// 官方插件在「耐久 step 边界」上重放整步；宿主的重试点在更下、也更准的位置——
// **单次 HTTP 请求**：网关还没回出 200、流还没开始时的失败。这样重放不碰会话历史、
// 不碰 step/start、也不会让 Transport/SSE 多一帧：在 agent/loopRunner 看来，
// 这一步依然只调用了 Stream 一次。
//
// 只重放**可安全重放**的失败：
//
//   - 429（限流）与 5xx（网关/上游抖动）：请求通常根本没进模型，重放只多花一次调用；
//   - 连接层瞬时错误（连接被重置、对端直接关连接 EOF、管道断开）：请求没被处理完；
//   - 401/403/404/400/422 **一次都不重试**：key/路径/参数错了，重放 4 次还是同一个结果，
//     只会把真实原因埋掉，还白烧 3 次配额；
//   - **流已经开始往外吐字之后**，任何失败都不重放：半句话 + 重来的一整句会被拼成两段，
//     用户看到重复输出，会话里那条 assistant 消息也会同时含有两遍内容。
//
// 所有状态都挂在**这一次调用**里（retryState），没有包级可变全局：两个 Client 实例、
// 两条并发流各自算各自的次数、预算与退避，互不串味。

const (
	// defaultRetryAttempts 是一条请求最多发几次（含首发）：1 首发 + 3 重试。
	//
	// 3 次而不是"重试到成功"：网关真挂了的时候，用户要的是尽快看到错误，
	// 不是被一个死循环按在"正在生成"上。按默认退避，最坏额外等 ~3.5 秒。
	defaultRetryAttempts = 4

	// defaultRetryBase 是第 1 次重试的退避基值：base * 2^(n-1)。
	defaultRetryBase = 500 * time.Millisecond

	// defaultRetryMaxWait 是**单次**等待的上限，同时也是 Retry-After 的天花板：
	// 网关让我们等得比这更久，就别在这个 turn 里等了（见 plan）。
	defaultRetryMaxWait = 8 * time.Second

	// defaultRetryBudget 是一条请求用于重试等待的**总**预算。
	defaultRetryBudget = 20 * time.Second

	// defaultRetryJitter 是抖动比例：等 (1-jitter)*d ~ d。
	defaultRetryJitter = 0.5
)

// retryConfig 是一条 client 的重试参数。零值不可用：用 defaultRetryConfig()。
//
// 单独拎成结构体而不是散字段，是为了让用例能整份替换（毫秒级退避 + 假 sleep），
// 不必真的在测试里睡几秒；生产路径永远走默认值。
type retryConfig struct {
	attempts int           // 最多发几次（含首发）；<=1 表示不重试
	base     time.Duration // 退避基值：第 n 次重试等 base*2^(n-1)
	maxWait  time.Duration // 单次等待上限；Retry-After 超过它就不再重试
	budget   time.Duration // 这条请求用于重试等待的总预算
	jitter   float64       // 抖动比例 0..1（只往下抖，不会顶破 maxWait）
	// sleep 只在用例里注入；nil 表示真的等（select 在 timer 与 ctx.Done 之间）。
	sleep func(ctx context.Context, d time.Duration) error
}

func defaultRetryConfig() retryConfig {
	return retryConfig{
		attempts: defaultRetryAttempts,
		base:     defaultRetryBase,
		maxWait:  defaultRetryMaxWait,
		budget:   defaultRetryBudget,
		jitter:   defaultRetryJitter,
	}
}

// retryPolicy 取这条 client 的重试参数：没显式配置就是生产默认。
//
// 读的是 Client 自己的字段，不做任何进程级缓存——A/B 两个实例各读各的。
func (c *Client) retryPolicy() retryConfig {
	if c.retry != nil {
		return *c.retry
	}
	return defaultRetryConfig()
}

// retryState 是**一条 Stream 调用**的重试状态：次数、已等待时长、起点。
//
// 它不是全局的，也不挂在 Client 上：同一对 A/B 实例各调各的，同一条连接上
// 并发跑的两条流也各算各的——一个会话退避过 3 次，不该让另一个会话的第 1 次
// 重试就没有配额。
type retryState struct {
	cfg    retryConfig
	start  time.Time     // 这条请求的起点（总耗时由 budget 兜底）
	sent   int           // 已经发出的 HTTP 次数（含首发）
	waited time.Duration // 已经用于退避的总时长（按计划值记，便于用例断言）
}

func newRetryState(cfg retryConfig) *retryState {
	return &retryState{cfg: cfg, start: time.Now()}
}

// more 报告还能不能再发一次。
func (s *retryState) more() bool {
	return s.sent < s.cfg.attempts
}

// backoff 是第 n 次重试前的指数退避，带向下抖动。
//
// 只往下抖（(1-jitter)*d ~ d）而不是上下都抖：向上抖会顶破 maxWait，
// 也让"最多等多久"变成一个说不准的数。
func (s *retryState) backoff() time.Duration {
	base := s.cfg.base
	if base <= 0 {
		return 0
	}
	shift := s.sent - 1 // 首发之后第几次重试
	if shift < 0 {
		shift = 0
	}
	if shift > 16 { // 左移到这个量级早就撞 maxWait 了，防溢出即可
		shift = 16
	}
	d := base << uint(shift)
	if s.cfg.maxWait > 0 && d > s.cfg.maxWait {
		d = s.cfg.maxWait
	}
	if j := s.cfg.jitter; j > 0 {
		span := int64(float64(d) * j)
		if span > 0 {
			// math/rand 的全局源本身并发安全，这里只拿它出一个随机数，
			// 不承载任何跨请求的状态。
			d -= time.Duration(rand.Int63n(span + 1))
		}
	}
	return d
}

// plan 算出这次重试该等多久，并回答"到底还该不该等"。
//
// 等待取 max(自身退避, Retry-After)：Retry-After 是**下限**，服务端让我们
// 至少等这么久，提前重试等于不尊重它（只会再吃一个 429）。
//
// ok=false 表示不该再等，直接把错误交出去：
//   - 单次要等的时间超过 maxWait（服务端说 1 小时后再来——阻塞一小时更荒谬）；
//   - 加上这一次就超出总预算；
//   - 这条请求已经跑得比预算更久了。
func (s *retryState) plan(retryAfter time.Duration) (time.Duration, bool) {
	if !s.more() {
		return 0, false
	}
	wait := s.backoff()
	if retryAfter > wait {
		wait = retryAfter
	}
	if wait < 0 {
		wait = 0
	}
	if s.cfg.maxWait > 0 && wait > s.cfg.maxWait {
		return 0, false
	}
	if s.cfg.budget > 0 {
		if s.waited+wait > s.cfg.budget {
			return 0, false
		}
		if time.Since(s.start) > s.cfg.budget {
			return 0, false
		}
	}
	return wait, true
}

// wait 等一段时间，但**随时可以被 ctx 打断**：取消/超时必须即时返回，
// 不能等完退避再报错——用户按了取消，界面就该立刻停。
func (s *retryState) wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	s.waited += d
	if sleep := s.cfg.sleep; sleep != nil {
		return sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryableStatus 是"这次失败可能只是抖动"的状态码：429 与 5xx。
//
// 401/403/404/400/422 一个都不许进来：那是确定性拒绝，重试只会把同一个
// 错误重放 4 次。
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || (code >= 500 && code <= 599)
}

// transientConnectMarkers 是 Windows 上拿不到 errno 时的文本兜底。
//
// 同一件事在这里有三种写法：Go 的 syscall 常量（*nix）、Winsock 的
// `wsarecv: An existing connection was forcibly closed by the remote host`、
// 以及代理层自己拼的 `server closed idle connection`。
var transientConnectMarkers = []string{
	"connection reset", "connection refused", "connection aborted",
	"broken pipe", "unexpected eof", "server closed idle connection",
	"forcibly closed", "wsarecv", "wsasend", "connection closed by peer",
}

// transientConnectError 报告一次**请求建立阶段**的失败是不是瞬时的。
//
// 只收"对端/链路突然没了"这一类。**不收**：
//   - ctx 取消/超时（取消就是取消，超时就是超时，重试只会把超时翻倍）；
//   - net.Error 的 Timeout()（同上：超时不重试）；
//   - DNS/证书/TLS 失败（重试 4 次还是同一个结果，那是配置问题）。
func transientConnectError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, marker := range transientConnectMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// parseRetryAfter 解析 Retry-After：秒数（允许小数，有些代理会写 0.5）或 HTTP-date。
//
// 解析不了就当没给（返回 ok=false，走自身退避）：一个坏 header 不该把整轮打挂，
// 更不该因为"看不懂"就无限等。
func parseRetryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	if h == nil {
		return 0, false
	}
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(raw, 64); err == nil {
		if secs < 0 || secs != secs { // 负数或 NaN
			return 0, false
		}
		return time.Duration(secs * float64(time.Second)), true
	}
	if at, err := http.ParseTime(raw); err == nil {
		d := at.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// markAttempts 把"这条请求实际发了几次"写进 GatewayError。
//
// 连接层失败的错误是 post 造的（它看不见重试），这里补上次数；这样上层日志和
// 四字段错误体都能看出"重试过 4 次还是不行"，而不是一句没有上下文的"连不上"。
func markAttempts(err error, sent int) {
	var gw *GatewayError
	if errors.As(err, &gw) {
		gw.Attempts = sent
	}
}

// countingOnEvent 给回调套一层计数：不管调用方给没给回调，都按"这条流往外发了
// 几次"记账。重放只允许发生在**一次都没发过**的时候，所以这个计数是重试的
// 第二道闸（第一道是 HTTP 状态本身）。
func countingOnEvent(onEvent func(Event) error, count *int) func(Event) error {
	return func(ev Event) error {
		*count++
		if onEvent == nil {
			return nil
		}
		return onEvent(ev)
	}
}

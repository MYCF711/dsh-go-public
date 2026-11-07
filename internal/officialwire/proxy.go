package officialwire

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// LegacyClient 用**宿主内部服务凭据**调用既有 L1 `POST /api/<dotted>`。
//
// 为什么需要它：M1 的地基是"把本仓已有方法映射到官方 namespace/method"，
// 而不是把 145 个官方方法的业务逻辑再写一遍。直接复用既有 handler 有三条好处：
//   - 语义只有一份（session.page 的窗口/游标规则不会分叉成两份）
//   - 既有测试继续有效（它们测的还是同一条路径）
//   - 官方 wire 出问题时，L1 的行为不变，可以单独回滚
//
// 认证域**不互通**：官方 cookie（dsh-auth-…）在这里没有任何用处；本客户端
// 用 launch token 自己去换一份 L1 cookie，且只在宿主进程内部使用。
type LegacyClient struct {
	// Handler 是既有 L1 处理器（transport.Server 就是 http.Handler）。
	Handler http.Handler
	// TokenSource 返回 launch token，用于换 L1 cookie。
	//
	// **生产时序下它多半已经用掉了**：桌面壳启动时会先 `GET /connect` 兑一次，
	// launch token 是一次性的，这里再兑只会拿到 401，于是每个官方 unary 都变成
	// `remote-access/denied`（L2 E2E P0-2 实测）。所以优先用下面的 CookieSource。
	TokenSource func() string
	// CookieSource 返回一份**进程内有效**的 L1 cookie（不再消耗 launch token）。
	CookieSource func() string

	mu     sync.Mutex
	cookie string
}

// legacyError 是 L1 的四字段错误体。
type legacyError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Hint      string `json:"hint"`
	Retryable bool   `json:"retryable"`
}

// legacyEnvelope 是 L1 的响应信封（成功或失败二选一）。
type legacyEnvelope struct {
	Result json.RawMessage `json:"result"`
	Error  *legacyError    `json:"error"`
}

// Call 调一次 L1 方法，返回 `result` 原文或翻译后的官方错误。
func (c *LegacyClient) Call(ctx context.Context, dotted string, args json.RawMessage) (json.RawMessage, *Error) {
	if c == nil || c.Handler == nil {
		return nil, Errorf(CodeInternal, "officialwire: %v", errNoSink)
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	cookie := c.legacyCookie(ctx)
	status, envelope, err := c.post(ctx, dotted, args, cookie)
	if err != nil {
		return nil, Errorf(CodeInternal, "L1 %s 调用失败：%v", dotted, err)
	}
	if status == http.StatusUnauthorized {
		// cookie 过期/换过：刷新一次再试。生产里 L1 cookie 与进程同寿，
		// 这条分支主要是为了"宿主重启后旧 LegacyClient 还在"的形态。
		c.resetCookie()
		cookie = c.legacyCookie(ctx)
		status, envelope, err = c.post(ctx, dotted, args, cookie)
		if err != nil {
			return nil, Errorf(CodeInternal, "L1 %s 重试失败：%v", dotted, err)
		}
	}
	namespace := l1Namespace(dotted)
	if envelope.Error != nil {
		return nil, TranslateLegacy(namespace, status, envelope.Error.Code, envelope.Error.Message)
	}
	if status != http.StatusOK {
		return nil, TranslateLegacy(namespace, status, "", fmt.Sprintf("L1 %s 返回 %d", dotted, status))
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return nil, Errorf(CodeInternal, "L1 %s 返回了空 result", dotted)
	}
	return envelope.Result, nil
}

// Binding 造一个"args 原样转发、结果按需改写"的官方方法绑定。
//
// transform 为 nil 时把 result 解成 `any`（map/[]any），这样官方附件编码器
// 才能走进嵌套结构找到 []byte；原样返回 json.RawMessage 的话，字节数组
// 永远不会被抽成 multipart part。
func (c *LegacyClient) Binding(dotted string, descriptor *Descriptor, transform func(result json.RawMessage) (any, *Error)) Binding {
	return Binding{
		Descriptor: descriptor,
		Handle: func(ctx context.Context, args map[string]json.RawMessage) (any, *Error) {
			rawArgs, err := json.Marshal(args)
			if err != nil {
				return nil, Errorf(CodeInternal, "officialwire: args 重新序列化失败：%v", err)
			}
			result, failure := c.Call(ctx, dotted, rawArgs)
			if failure != nil {
				return nil, failure
			}
			if transform != nil {
				return transform(result)
			}
			return decodeAny(result)
		},
	}
}

// decodeAny 把 L1 result 解成 any 树。
func decodeAny(raw json.RawMessage) (any, *Error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, Errorf(CodeInternal, "officialwire: L1 result 不是合法 JSON：%v", err)
	}
	return value, nil
}

// post 打一次 L1 `/api/<dotted>`。
func (c *LegacyClient) post(ctx context.Context, dotted string, args json.RawMessage, cookie string) (int, legacyEnvelope, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://legacy/api/"+dotted, bytes.NewReader(args))
	if err != nil {
		return 0, legacyEnvelope{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Host = "legacy"
	if cookie != "" {
		request.AddCookie(&http.Cookie{Name: "dshd_session", Value: cookie})
	}
	recorder := newCapture()
	c.Handler.ServeHTTP(recorder, request)
	var envelope legacyEnvelope
	if recorder.body.Len() > 0 {
		if err := json.Unmarshal(recorder.body.Bytes(), &envelope); err != nil {
			return recorder.status, legacyEnvelope{}, fmt.Errorf("L1 响应不是 JSON 信封：%w", err)
		}
	}
	return recorder.status, envelope, nil
}

// legacyCookie 取（必要时先换）L1 cookie。
func (c *LegacyClient) legacyCookie(ctx context.Context) string {
	c.mu.Lock()
	if c.cookie != "" {
		cookie := c.cookie
		c.mu.Unlock()
		return cookie
	}
	c.mu.Unlock()

	cookie := c.exchangeToken(ctx)
	c.mu.Lock()
	c.cookie = cookie
	c.mu.Unlock()
	return cookie
}

// exchangeToken 用 launch token 换一份 L1 cookie。
//
// launch token 在 L1 是一次性的：换失败（已被别处换过）时返回空串，
// 调用方拿到 401 会走一次重试分支——那条分支同样拿不到新 cookie，
// 于是 L1 的错误会原样翻译成 remote-access/denied，而不是静默成功。
func (c *LegacyClient) exchangeToken(ctx context.Context) string {
	if c.CookieSource != nil {
		if value := strings.TrimSpace(c.CookieSource()); value != "" {
			return value
		}
	}
	if c.TokenSource == nil {
		return ""
	}
	token := strings.TrimSpace(c.TokenSource())
	if token == "" {
		return ""
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://legacy/connect?token="+token, nil)
	if err != nil {
		return ""
	}
	request.Host = "legacy"
	recorder := newCapture()
	c.Handler.ServeHTTP(recorder, request)
	response := &http.Response{Header: recorder.header}
	for _, cookie := range response.Cookies() {
		if cookie.Name == "dshd_session" {
			return cookie.Value
		}
	}
	return ""
}

func (c *LegacyClient) resetCookie() {
	c.mu.Lock()
	c.cookie = ""
	c.mu.Unlock()
}

// l1Namespace 取 L1 dotted 方法的第一段（`session.page` → `session`）。
func l1Namespace(dotted string) string {
	if index := strings.IndexByte(dotted, '.'); index > 0 {
		return dotted[:index]
	}
	if dotted == "" {
		return "gateway"
	}
	return dotted
}

// capture 是最小的 http.ResponseWriter 记录器。
//
// 不用 httptest.ResponseRecorder：那是测试包，出现在产品代码里会让"这个类型
// 只该在测试里出现"的边界消失（而且它默认补一堆测试专用行为）。
type capture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newCapture() *capture {
	return &capture{header: http.Header{}}
}

func (c *capture) Header() http.Header { return c.header }

func (c *capture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *capture) Write(payload []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(payload)
}

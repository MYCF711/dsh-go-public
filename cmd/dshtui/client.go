package main

// Transport 客户端：HTTP 做事、WS 收流。两层都用同一份 cookie 认证。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// client 是宿主的 Transport 客户端。
type client struct {
	base   string // 形如 http://127.0.0.1:8080
	hc     *http.Client
	cookie string

	// 测试注入点；生产为空时使用 dialSub 与 backoff。
	dialSubscription func(sid string, sinceSeq int) (subscriptionConn, error)
	reconnectBackoff func(attempt int) time.Duration
	nextSubscription atomic.Uint64
}

type envelope struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// newClient 连上宿主并换到 cookie。
//
// `/connect` 返回 303 + Set-Cookie；**不能跟随重定向**——跟了会去 GET 目标页，
// 拿不到 cookie，后面的每个请求都会 401，而现象看起来像"服务端不认我"。
func newClient(base, token string) (*client, error) {
	c := &client{
		base: strings.TrimRight(base, "/"),
		hc: &http.Client{
			Timeout: 60 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	u := c.base + "/connect?token=" + url.QueryEscape(token)
	resp, err := c.hc.Get(u)
	if err != nil {
		return nil, fmt.Errorf("连不上宿主 %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	for _, ck := range resp.Cookies() {
		if ck.Name != "" {
			c.cookie = ck.Name + "=" + ck.Value
			break
		}
	}
	if c.cookie == "" {
		return nil, fmt.Errorf("/connect 没给 cookie（HTTP %d）；token 对不对？", resp.StatusCode)
	}
	return c, nil
}

// post 发一个 method，返回 result 原文。
func (c *client) post(method string, body any) (json.RawMessage, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", c.base+"/api/"+method, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", c.cookie)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s 请求失败: %w", method, err)
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	var env envelope
	if err := json.Unmarshal(buf, &env); err != nil {
		return nil, fmt.Errorf("%s 的响应不是信封（HTTP %d）：%s", method, resp.StatusCode, tail(buf))
	}
	if len(env.Error) > 0 && string(env.Error) != "null" {
		// 错误是四字段信封，把 message 提出来给人看，别把整个 JSON 甩出去。
		var e struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Hint    string `json:"hint"`
		}
		_ = json.Unmarshal(env.Error, &e)
		msg := e.Message
		if msg == "" {
			msg = string(env.Error)
		}
		if e.Hint != "" {
			msg += "（" + e.Hint + "）"
		}
		return nil, fmt.Errorf("%s: %s", method, msg)
	}
	return env.Result, nil
}

// createSession 建一个会话，返回 id。
func (c *client) createSession() (string, error) {
	res, err := c.post("session.create", map[string]any{})
	if err != nil {
		return "", err
	}
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &r); err != nil || r.SessionID == "" {
		return "", fmt.Errorf("session.create 没给出 sessionId：%s", tail(res))
	}
	return r.SessionID, nil
}

// sessionRow 是 session.list 的一行。
type sessionRow struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updatedAt"`
}

// listSessions 最近在前的会话列表。
func (c *client) listSessions() ([]sessionRow, error) {
	res, err := c.post("session.list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var r struct {
		Sessions []sessionRow `json:"sessions"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return nil, err
	}
	return r.Sessions, nil
}

// message 是 session.load 里的一条消息（只取导出要用的字段）。
type message struct {
	Seq  int    `json:"seq"`
	Role string `json:"role"`
	Text string `json:"text"`
}

// loadMessages 取会话消息（最多 limit 条），给 /export 用。
func (c *client) loadMessages(sid string, limit int) ([]message, error) {
	res, err := c.post("session.load", map[string]any{"id": sid, "tailMessages": limit})
	if err != nil {
		return nil, err
	}
	var r struct {
		Messages []message `json:"messages"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return nil, err
	}
	return r.Messages, nil
}

// stats 是会话当前的用量快照（来自 session.load）。
type stats struct {
	ContextTokens int // 最近一条 assistant 的 totalTokens：当前上下文占多少
	ContextWindow int // 模型上限；0 = 不知道（不画进度条）
	OutputTotal   int // 全会话累计输出 token
	LastSeq       int
	// Model 是这条会话最近一次请求用的模型 id；空 = 不知道（界面显示 `?`）。
	Model string
	// CWD 是这条会话的工作目录（宿主给，**不是**本进程的 cwd）；空 = 不知道。
	CWD string
	// Effort 保留线上缺失（nil）与显式空串（非 nil、指向 ""）的区别。
	// 两种在状态行都显示 `?`，但客户端不能把空值误当成字段存在且为 off。
	Effort *string
}

// loadStats 取一次用量。取不到不是致命错误——状态行显示"?"比显示 0 诚实。
func (c *client) loadStats(sid string) (stats, error) {
	res, err := c.post("session.load", map[string]any{"id": sid, "tailMessages": 1})
	if err != nil {
		return stats{}, err
	}
	var r struct {
		ContextTokens int     `json:"contextTokens"`
		ContextWindow int     `json:"contextWindow"`
		LastSeq       int     `json:"lastSeq"`
		Model         string  `json:"model"`
		CWD           string  `json:"cwd"`
		Effort        *string `json:"effort"`
		UsageTotal    *struct {
			OutputTokens int `json:"outputTokens"`
		} `json:"usageTotal"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return stats{}, err
	}
	st := stats{
		ContextTokens: r.ContextTokens,
		ContextWindow: r.ContextWindow,
		LastSeq:       r.LastSeq,
		Model:         r.Model,
		CWD:           r.CWD,
		Effort:        r.Effort,
	}
	if r.UsageTotal != nil {
		st.OutputTotal = r.UsageTotal.OutputTokens
	}
	return st, nil
}

// promptAck 是宿主对 session.prompt 的接收确认。
// accepted=false 与 HTTP 错误是两回事：前者有完整响应信封，界面也必须按失败处理。
type promptAck struct {
	Accepted bool   `json:"accepted"`
	TurnID   string `json:"turnId"`
	RPCID    string `json:"rpcId"`
}

// requestPrompt 发一条用户消息并返回宿主的真实接收确认（不等回答）。
func (c *client) requestPrompt(sid, text string) (promptAck, error) {
	res, err := c.post("session.prompt", map[string]any{"id": sid, "text": text})
	if err != nil {
		return promptAck{}, err
	}
	var ack promptAck
	if err := json.Unmarshal(res, &ack); err != nil {
		return promptAck{}, fmt.Errorf("session.prompt 的确认不是合法 JSON：%w", err)
	}
	return ack, nil
}

// prompt 保留 --send 的简化入口；全屏界面需要 accepted/turnId，走 requestPrompt。
func (c *client) prompt(sid, text string) error {
	ack, err := c.requestPrompt(sid, text)
	if err != nil {
		return err
	}
	if !ack.Accepted {
		return fmt.Errorf("session.prompt 未被宿主接受")
	}
	return nil
}

// Subscribe 订阅一个会话的流，**断线会自动重连**。
//
// 重连为什么能是"续"而不是"重来"：宿主那边已经有两件东西（H14）——
//
//	① `sinceSeq`：订阅时带上"我收到第几条了"，服务端只补之后的；
//	② `session/reset`：世代对不上时它明说"基线作废"，客户端据此重建。
//
// 所以客户端要做的只是：记住 lastSeq、断开就重订阅、看到 reset 就把基线清零。
//
// 不做重连的后果不是"少几条消息"，是**界面从此不动了**，而人不知道是网断了还是模型卡了。
func (c *client) Subscribe(sid string, sinceSeq int) (<-chan ServerFrame, func(), error) {
	return c.subscribe(sid, sinceSeq, nil)
}

// subscribe 是帧流与连接事件流的唯一重连实现。事件只在真实 dial/read 结果
// 发生时发出；订阅代际由 client 单调分配，连接代际在每次成功重连后递增。
func (c *client) subscribe(sid string, sinceSeq int, events chan<- connectionEvent) (<-chan ServerFrame, func(), error) {
	ch := make(chan ServerFrame, 64)
	done := make(chan struct{})
	subscription := c.nextSubscription.Add(1)
	dial := c.dialSubscription
	if dial == nil {
		dial = func(sid string, sinceSeq int) (subscriptionConn, error) {
			return c.dialSub(sid, sinceSeq)
		}
	}
	wait := c.reconnectBackoff
	if wait == nil {
		wait = backoff
	}

	// 先同步连一次：连不上要立刻报错，而不是启动一个永远连不上的后台循环。
	conn, err := dial(sid, sinceSeq)
	if err != nil {
		return nil, nil, err
	}

	var currentMu sync.Mutex
	current := conn
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(done)
			// 原实现只关 done，ReadText 可能一直阻塞；切换会话/退出时必须同时
			// 关当前 socket。由关闭引起的 read error 会看到 done，不报假 down。
			currentMu.Lock()
			if current != nil {
				_ = current.Close()
			}
			currentMu.Unlock()
		})
	}
	emit := func(kind connectionEventKind, generation uint64, cause error) {
		if events == nil {
			return
		}
		select {
		case events <- connectionEvent{kind: kind, sid: sid, subscription: subscription, generation: generation, err: cause}:
		case <-done:
		}
	}
	emit(wsOpen, 1, nil)

	go func() {
		defer close(ch)
		if events != nil {
			defer close(events)
		}
		defer func() { _ = conn.Close() }()
		lastSeq := sinceSeq
		generation := uint64(1)
		for {
			for {
				txt, err := conn.ReadText()
				if err != nil {
					select {
					case <-done:
						return // 主动切换/退出，不伪装成网络断线
					default:
					}
					emit(wsDown, generation, err)
					break // 断了或坏了，去重连
				}
				var f ServerFrame
				if json.Unmarshal([]byte(txt), &f) != nil {
					continue // 噪声帧：宁可少一条也不炸整条流
				}
				if f.Type == "session/reset" {
					// 服务端说基线作废：本地水位清零，后面的帧重新算。
					lastSeq = 0
				}
				if f.Seq > 0 {
					lastSeq = f.Seq
				}
				select {
				case ch <- f:
				case <-done:
					return
				}
			}
			_ = conn.Close()

			// 重连：退避 0.5s→1s→2s…上限 5s；每轮都带 lastSeq+1，接着上次往下收。
			for attempt := 0; ; attempt++ {
				select {
				case <-done:
					return
				case <-time.After(wait(attempt)):
				}
				nc, derr := dial(sid, lastSeq+1)
				if derr != nil {
					emit(wsDown, generation, derr)
					continue // 宿主可能还没起来：继续等，不放弃
				}
				currentMu.Lock()
				select {
				case <-done:
					currentMu.Unlock()
					_ = nc.Close()
					return
				default:
				}
				conn = nc
				current = nc
				currentMu.Unlock()
				generation++
				emit(wsOpen, generation, nil)
				break
			}
		}
	}()
	return ch, stop, nil
}

// dialSub 建一条 WS 并发出订阅帧。默认 offer permessage-deflate；
// 旧宿主不协商时 dialWSOptionalDeflate 返回原来的 *wsConn。
func (c *client) dialSub(sid string, sinceSeq int) (subscriptionWriteConn, error) {
	wsURL := strings.Replace(c.base, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	conn, err := dialWSOptionalDeflate(wsURL+"/ws", http.Header{"Cookie": []string{c.cookie}})
	if err != nil {
		return nil, err
	}
	sub := map[string]any{"t": "sub", "topic": "session:" + sid, "sinceSeq": sinceSeq}
	raw, _ := json.Marshal(sub)
	if err := conn.WriteText(string(raw)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("订阅写失败: %w", err)
	}
	return conn, nil
}

// backoff 是重连间隔：0.5s 起，翻倍，封顶 5s。
func backoff(attempt int) time.Duration {
	d := 500 * time.Millisecond << attempt
	if d > 5*time.Second || d <= 0 {
		d = 5 * time.Second
	}
	return d
}

// ServerFrame 是推给客户端的一帧事件。
type ServerFrame struct {
	Seq  int             `json:"seq"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
	Time int64           `json:"time"`
}

func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

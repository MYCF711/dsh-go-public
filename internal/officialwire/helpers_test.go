package officialwire

// 本文件是协议层测试的公共夹具：真 httptest 服务 + 真 cookie + 裸 TCP WebSocket
// 客户端。这里不使用内部函数直接调 handler —— 每条测试都走**真实 HTTP/WS 路径**，
// 否则"信封/状态码/关闭码"这些正是线上契约的东西就测不到。

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testToken = "launch-token-for-tests"

// testServerConfig 是确定性的协议层配置。
func testServerConfig() ServerConfig {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	return ServerConfig{
		Secret:      secret,
		TokenSource: func() string { return testToken },
		Home:        "/home/test",
		Now:         func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
}

// startWire 起一个真的 officialwire 服务（legacy 用 NotFoundHandler 顶替）。
func startWire(t *testing.T, cfg ServerConfig, bindings map[string]Binding) (*Server, *httptest.Server, *http.Client) {
	t.Helper()
	server, failure := NewServer(cfg)
	if failure != nil {
		t.Fatalf("NewServer: %v", failure)
	}
	for endpoint, binding := range bindings {
		if err := server.Handle(endpoint, binding); err != nil {
			t.Fatalf("Handle(%s): %v", endpoint, err)
		}
	}
	ts := httptest.NewServer(server.Handler(http.NotFoundHandler()))
	t.Cleanup(ts.Close)
	return server, ts, newTestClient(t)
}

func newTestClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // 302 本身就是要断言的东西
		},
	}
}

// connect 走官方引导：/connect?token=&interactions=…
func doConnect(t *testing.T, ts *httptest.Server, client *http.Client, interactions string) *http.Response {
	t.Helper()
	response, err := client.Get(ts.URL + "/connect?token=" + testToken + "&interactions=" + interactions)
	if err != nil {
		t.Fatalf("GET /connect: %v", err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

// connectAndCookies 完成引导并返回 cookie 列表。
func connectAndCookies(t *testing.T, ts *httptest.Server, client *http.Client, interactions string) []*http.Cookie {
	t.Helper()
	response := doConnect(t, ts, client, interactions)
	if response.StatusCode != http.StatusFound {
		t.Fatalf("/connect 应 302，实际 %d", response.StatusCode)
	}
	url, err := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	if err != nil {
		t.Fatalf("构造 cookie 查询请求: %v", err)
	}
	return client.Jar.Cookies(url.URL)
}

// envelopeBody 造一条官方 client-request。
func envelopeBody(rpcID, method, argsJSON string) string {
	if argsJSON == "" {
		argsJSON = "{}"
	}
	return `{"type":"client-request","rpcId":` + quoteJSON(rpcID) +
		`,"method":` + quoteJSON(method) +
		`,"payload":{"args":` + argsJSON + `}}`
}

func quoteJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// postEnvelope 发一条 unary 请求，返回状态码与原始正文。
func postEnvelope(t *testing.T, ts *httptest.Server, client *http.Client, endpoint, body string) (int, []byte) {
	t.Helper()
	response, err := client.Post(ts.URL+"/api/"+endpoint, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/%s: %v", endpoint, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读响应正文: %v", err)
	}
	return response.StatusCode, payload
}

// decodeEnvelope 把响应正文解成 map，并断言它确实是官方信封。
func decodeEnvelope(t *testing.T, status int, payload []byte) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("响应不是 JSON：%v 原文=%s", err, payload)
	}
	if status != http.StatusOK {
		t.Fatalf("官方 unary 业务响应必须是 200，实际 %d 原文=%s", status, payload)
	}
	if envelope["type"] != "server-response" {
		t.Fatalf("不是 server-response 信封：%s", payload)
	}
	return envelope
}

// resultOf 取 result 对象。
func resultOf(t *testing.T, envelope map[string]any) map[string]any {
	t.Helper()
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		t.Fatalf("信封缺 result 对象：%v", envelope)
	}
	return result
}

// failureOf 要求 result.ok=false 并返回 error 对象（同时校验 details 是对象）。
func failureOf(t *testing.T, envelope map[string]any) map[string]any {
	t.Helper()
	result := resultOf(t, envelope)
	if ok, _ := result["ok"].(bool); ok {
		t.Fatalf("期望 result.ok=false，实际 true：%v", envelope)
	}
	failure, ok := result["error"].(map[string]any)
	if !ok {
		t.Fatalf("result.ok=false 但没有 error 对象：%v", envelope)
	}
	for _, key := range []string{"code", "message", "details"} {
		if _, has := failure[key]; !has {
			t.Fatalf("error 缺 %s（官方 zod 要求 {code,message,details}）：%v", key, failure)
		}
	}
	if _, ok := failure["details"].(map[string]any); !ok {
		t.Fatalf("error.details 必须是对象，实际 %T", failure["details"])
	}
	return failure
}

// wsClient 是裸 TCP 的 WebSocket 客户端（服务端方向不掩码，客户端方向必须掩码）。
type wsClient struct {
	conn net.Conn
	br   *bufio.Reader
}

// dialMux 用真实握手连 /api/remote.mux，返回客户端与握手响应。
func dialMux(t *testing.T, ts *httptest.Server, cookies []*http.Cookie) (*wsClient, *http.Response) {
	t.Helper()
	address := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("连接 %s: %v", address, err)
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatalf("生成 Sec-WebSocket-Key: %v", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	var request strings.Builder
	fmt.Fprintf(&request, "GET /api/remote.mux HTTP/1.1\r\nHost: %s\r\n", address)
	request.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\n")
	fmt.Fprintf(&request, "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", key)
	for _, cookie := range cookies {
		fmt.Fprintf(&request, "Cookie: %s=%s\r\n", cookie.Name, cookie.Value)
	}
	request.WriteString("\r\n")
	if _, err := conn.Write([]byte(request.String())); err != nil {
		conn.Close()
		t.Fatalf("写握手请求: %v", err)
	}
	br := bufio.NewReader(conn)
	response, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		t.Fatalf("读握手响应: %v", err)
	}
	return &wsClient{conn: conn, br: br}, response
}

func (c *wsClient) Close() {
	if c == nil || c.conn == nil {
		return
	}
	_ = c.conn.Close()
}

// writeFrame 写一条客户端帧（必须带掩码）。
func (c *wsClient) writeFrame(t *testing.T, op byte, payload []byte) {
	t.Helper()
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	first := byte(0x80) | op
	var header []byte
	switch {
	case len(payload) < 126:
		header = []byte{first, 0x80 | byte(len(payload))}
	case len(payload) <= 0xffff:
		header = make([]byte, 4)
		header[0], header[1] = first, 0x80|126
		binary.BigEndian.PutUint16(header[2:], uint16(len(payload)))
	default:
		header = make([]byte, 10)
		header[0], header[1] = first, 0x80|127
		binary.BigEndian.PutUint64(header[2:], uint64(len(payload)))
	}
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	header = append(header, mask[:]...)
	if err := writeAll(c.conn, header); err != nil {
		t.Fatalf("写帧头: %v", err)
	}
	if err := writeAll(c.conn, masked); err != nil {
		t.Fatalf("写帧正文: %v", err)
	}
}

func (c *wsClient) writeText(t *testing.T, payload string) {
	t.Helper()
	c.writeFrame(t, opText, []byte(payload))
}

// readFrame 读一条服务端帧。
func (c *wsClient) readFrame(t *testing.T, timeout time.Duration) (byte, []byte, error) {
	t.Helper()
	if timeout > 0 {
		_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
		defer c.conn.SetReadDeadline(time.Time{})
	}
	var header [2]byte
	if _, err := io.ReadFull(c.br, header[:]); err != nil {
		return 0, nil, err
	}
	op := header[0] & 0x0f
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	return op, payload, nil
}

// readJSONFrame 读一条文本帧并解成 map（跳过 ping/pong）。
func (c *wsClient) readJSONFrame(t *testing.T, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("等一帧超时（%v）", timeout)
		}
		op, payload, err := c.readFrame(t, remaining)
		if err != nil {
			t.Fatalf("读帧失败：%v", err)
		}
		switch op {
		case opText:
			var frame map[string]any
			if err := json.Unmarshal(payload, &frame); err != nil {
				t.Fatalf("帧不是 JSON 对象：%v 原文=%s", err, payload)
			}
			return frame
		case opPing:
			// 心跳帧：回 pong，继续等业务帧。
			c.writeFrame(t, opPong, payload)
		case opPong:
			continue
		case opClose:
			t.Fatalf("连接被关闭（实际关闭码见 closeCode）")
		}
	}
}

// readCloseCode 读一条关闭帧并返回关闭码。
func (c *wsClient) readCloseCode(t *testing.T, timeout time.Duration) int {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	defer c.conn.SetReadDeadline(time.Time{})
	for {
		op, payload, err := c.readFrame(t, 0)
		if err != nil {
			t.Fatalf("等关闭帧失败（连接可能被直接断开）：%v", err)
		}
		switch op {
		case opClose:
			if len(payload) < 2 {
				return 1005
			}
			return int(binary.BigEndian.Uint16(payload[:2]))
		case opPing:
			c.writeFrame(t, opPong, payload)
		default:
			continue
		}
	}
}

// expectEOF 断言连接被直接断开（terminate 路径没有关闭帧）。
func (c *wsClient) expectEOF(t *testing.T, timeout time.Duration) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	defer c.conn.SetReadDeadline(time.Time{})
	var probe [1]byte
	for {
		if _, err := c.br.Read(probe[:]); err != nil {
			if err == io.EOF {
				return
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				t.Fatalf("%v 内连接没有被断开（心跳判死没生效）", timeout)
			}
			return
		}
	}
}

// frameValueOf 取 mux item 帧的 value 对象。
func frameValueOf(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	value, ok := frame["value"].(map[string]any)
	if !ok {
		t.Fatalf("item 帧的 value 不是对象：%v", frame)
	}
	return value
}

// payloadOf 造一条 open 帧。
func openFrame(streamID, endpoint, argsJSON string) string {
	if argsJSON == "" {
		argsJSON = "{}"
	}
	return `{"type":"open","streamId":` + quoteJSON(streamID) +
		`,"endpoint":` + quoteJSON(endpoint) +
		`,"payload":{"args":` + argsJSON + `}}`
}

// captureHandler 是 legacy 侧的确定性 handler（用于 Router 并存测试）。
type captureHandler struct {
	status int
	body   string
	seen   *bytes.Buffer
}

func (h *captureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.seen != nil {
		fmt.Fprintf(h.seen, "%s %s %s\n", r.Method, r.URL.Path, r.URL.RawQuery)
	}
	status := h.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(h.body))
}

package main

// HOST-L2-M1-WIRE 的接线层验收：证明 officialwire 协议层挂到**本仓真实宿主**上
// 之后，官方 namespace/method 能读真东西（真 v4 会话、真文件字节、真 journal），
// 并且 dotted 路径的字节一字未动。
//
// 这里不打桩：session/page 读的是真实 v4 日志，workspaceFiles/readBytes 读的是
// 真文件系统的字节，session/events 读的是 transport.Journals 里真追加的帧。

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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dshgo/internal/officialwire"
	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// officialWireHarness 是一套"真 home + 真 Transport + 真 officialwire 挂载"。
type officialWireHarness struct {
	home     string
	srv      *transport.Server
	journals *transport.Journals
	mounted  *httptest.Server
	client   *http.Client // 官方 cookie（answer 型 Peer）
}

func newOfficialWireHarness(t *testing.T) *officialWireHarness {
	t.Helper()
	return buildOfficialWireHarness(t, officialwire.InteractionsAnswer)
}

// newOfficialWireHarnessWith 允许测试注入 `$events` 事件源（其余与上面完全一致）。
//
// 为什么用可变参数而不是改签名：一目了然的 `newOfficialWireHarness(t)` 调用点有
// 十几处，改签名会把"这个用例在测什么"埋进参数里。
func newOfficialWireHarnessWith(t *testing.T, tweaks ...func(*officialWireDeps)) *officialWireHarness {
	t.Helper()
	return buildOfficialWireHarness(t, officialwire.InteractionsAnswer, tweaks...)
}

// buildOfficialWireHarness 是 harness 的唯一构造点：interactions 决定 Peer 是
// answer 型还是 none 型（`$events/result` 的准入就是按它判的，EVIDENCE §3.2）。
func buildOfficialWireHarness(t *testing.T, interactions string, tweaks ...func(*officialWireDeps)) *officialWireHarness {
	t.Helper()
	home := t.TempDir()
	journals := transport.NewJournals(nil)
	source := newSessionSource(home, "")
	opts := withOfficialSessionOptions(home, transport.Options{
		Source:   source,
		Journals: journals,
	}, nil)
	srv, err := transport.NewServer(opts)
	if err != nil {
		t.Fatalf("transport.NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	// workspaceFiles.* 需要真实 workspace API（它读真磁盘）。
	attachOfficialWorkspace(srv, home, source, nil)

	deps := officialWireDeps{
		Home:     home,
		Journals: journals,
		Token:    srv.Token,
	}
	for _, tweak := range tweaks {
		tweak(&deps)
	}
	wire, failure := newOfficialWireServer(srv, deps)
	if failure != nil {
		t.Fatalf("newOfficialWireServer: %v", failure)
	}
	mounted := httptest.NewServer(wire.Handler(srv))
	t.Cleanup(mounted.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	connect, err := client.Get(mounted.URL + "/connect?token=" + srv.Token() + "&interactions=" + interactions)
	if err != nil {
		t.Fatalf("官方 /connect: %v", err)
	}
	connect.Body.Close()
	if connect.StatusCode != http.StatusFound {
		t.Fatalf("官方 /connect 应 302，实际 %d", connect.StatusCode)
	}
	return &officialWireHarness{home: home, srv: srv, journals: journals, mounted: mounted, client: client}
}

// officialCookies 取官方 cookie（给裸 WS 握手用）。
func (h *officialWireHarness) officialCookies(t *testing.T) []*http.Cookie {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, h.mounted.URL+"/", nil)
	if err != nil {
		t.Fatalf("构造 cookie 查询: %v", err)
	}
	cookies := h.client.Jar.Cookies(request.URL)
	if len(cookies) == 0 {
		t.Fatalf("官方客户端没有 cookie")
	}
	return cookies
}

// post 发一条官方 unary 请求。
func (h *officialWireHarness) post(t *testing.T, endpoint, body string) (int, []byte) {
	t.Helper()
	response, err := h.client.Post(h.mounted.URL+"/api/"+endpoint, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/%s: %v", endpoint, err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	return response.StatusCode, payload
}

func wireEnvelope(rpcID, method, args string) string {
	encodedID, _ := json.Marshal(rpcID)
	encodedMethod, _ := json.Marshal(method)
	return `{"type":"client-request","rpcId":` + string(encodedID) + `,"method":` + string(encodedMethod) +
		`,"payload":{"args":` + args + `}}`
}

// wireResult 解出 result 对象并断言 200。
func wireResult(t *testing.T, status int, payload []byte) map[string]any {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("官方 unary 必须 200，实际 %d 原文=%s", status, payload)
	}
	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("响应不是 JSON：%v 原文=%s", err, payload)
	}
	if envelope["type"] != "server-response" {
		t.Fatalf("不是 server-response：%s", payload)
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		t.Fatalf("缺 result：%s", payload)
	}
	return result
}

// TestOfficialWireSwitchDefaultsOff 钉住"默认关 + 关闭时原样返回 nil"。
func TestOfficialWireSwitchDefaultsOff(t *testing.T) {
	for _, value := range []string{"", "0", "false", "no", "off"} {
		t.Setenv(officialWireEnv, value)
		if officialWireEnabled() {
			t.Fatalf("%s=%q 不该打开开关", officialWireEnv, value)
		}
		if mount := officialWireMount(officialWireDeps{}); mount != nil {
			t.Fatalf("%s=%q 时必须返回 nil（transport 原样用自己），实际拿到包装函数", officialWireEnv, value)
		}
	}
	for _, value := range []string{"1", "true", "TRUE", "yes"} {
		t.Setenv(officialWireEnv, value)
		if !officialWireEnabled() {
			t.Fatalf("%s=%q 应该打开开关", officialWireEnv, value)
		}
		if officialWireMount(officialWireDeps{}) == nil {
			t.Fatalf("%s=%q 时必须给出包装函数", officialWireEnv, value)
		}
	}
}

// TestOfficialWireMountServesSlashRoute 钉住挂载点真的接上了协议层。
func TestOfficialWireMountServesSlashRoute(t *testing.T) {
	t.Setenv(officialWireEnv, "1")
	home := t.TempDir()
	journals := transport.NewJournals(nil)
	srv, err := transport.NewServer(transport.Options{Source: newSessionSource(home, ""), Journals: journals})
	if err != nil {
		t.Fatalf("transport.NewServer: %v", err)
	}
	defer srv.Close()

	mount := officialWireMount(officialWireDeps{Home: home, Journals: journals})
	if mount == nil {
		t.Fatalf("开关打开时必须给出包装函数")
	}
	handler := mount(srv)
	if handler == nil {
		t.Fatalf("包装函数必须返回 handler")
	}
	mounted := httptest.NewServer(handler)
	defer mounted.Close()

	// 没有 cookie：官方路径 401（说明它确实被官方层截走了，没有落到 L1 的 501）。
	response, err := http.Post(mounted.URL+"/api/session/page", "application/json",
		strings.NewReader(wireEnvelope("r-1", "session/page", `{"address":{"kind":"session","sessionId":"x"}}`)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未认证的官方路径应 401，实际 %d", response.StatusCode)
	}
}

// TestOfficialWireUnknownMethodIs200UnknownMethod 钉住未知方法分支。
func TestOfficialWireUnknownMethodIs200UnknownMethod(t *testing.T) {
	h := newOfficialWireHarness(t)
	status, payload := h.post(t, "session/nope", wireEnvelope("r-unknown", "session/nope", `{}`))
	result := wireResult(t, status, payload)
	if ok, _ := result["ok"].(bool); ok {
		t.Fatalf("未知方法必须 ok:false：%s", payload)
	}
	failure, _ := result["error"].(map[string]any)
	if failure["code"] != "gateway/unknown-method" {
		t.Fatalf("码应为 gateway/unknown-method，实际 %v", failure)
	}
}

// TestOfficialWireSessionPageReadsRealSession 钉住 session/page → L1 session.page。
func TestOfficialWireSessionPageReadsRealSession(t *testing.T) {
	h := newOfficialWireHarness(t)
	fixture := newOfficialTestSession(t, h.home, "wire-page")

	args := fmt.Sprintf(`{"address":{"kind":"session","sessionId":%q},"maxMessages":50}`, fixture.id)
	status, payload := h.post(t, "session/page", wireEnvelope("r-page", "session/page", args))
	result := wireResult(t, status, payload)
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("session/page 应成功：%s", payload)
	}
	value, _ := result["value"].(map[string]any)
	if value["cursor"] != float64(fixture.lastSeq) {
		t.Fatalf("cursor 应是真实日志末端 %d，实际 %v", fixture.lastSeq, value["cursor"])
	}
	records, _ := value["records"].([]any)
	if len(records) == 0 {
		t.Fatalf("records 不该为空（真会话里有 user/assistant 消息）：%v", value)
	}
	if _, has := value["hasMore"]; !has {
		t.Fatalf("结果必须带 hasMore：%v", value)
	}

	// 参数失败走 200 + arguments-invalid（不是 4xx）。
	for _, testCase := range []struct {
		name string
		args string
	}{
		{"多一个字段", fmt.Sprintf(`{"address":{"kind":"session","sessionId":%q},"bogus":1}`, fixture.id)},
		{"缺 address", `{"maxMessages":5}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			status, payload := h.post(t, "session/page", wireEnvelope("r-args", "session/page", testCase.args))
			result := wireResult(t, status, payload)
			failure, _ := result["error"].(map[string]any)
			if failure["code"] != "gateway/arguments-invalid" {
				t.Fatalf("应回 gateway/arguments-invalid，实际 %v", failure)
			}
		})
	}
}

// TestOfficialWireReadBytesReturnsMultipart 钉住 workspaceFiles/readBytes 的真字节 + 附件。
func TestOfficialWireReadBytesReturnsMultipart(t *testing.T) {
	h := newOfficialWireHarness(t)

	root := filepath.Join(h.home, "workspace-readbytes")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// 既有 attachment store 的真实策略：只收图片或 UTF-8 文本。
	// 裸字节**编解码**的二进制安全由 internal/officialwire 的用例证明（含 0x00/0xFF）；
	// 这里证明"字节真的进了 L1 那个 store"，所以用一份合法的 UTF-8 内容。
	content := []byte{0x00, 0x01, 0x02, 0xfe, 0xff, 'd', 's', 'h'}
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	sessionID := "wire-readbytes"
	lg, err := session.CreateSession(h.home, root, sessionID, "standard")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("关闭 session log: %v", err)
	}

	args := fmt.Sprintf(`{"workspaceFileScopeId":%q,"path":"blob.bin","options":{}}`, sessionID)
	response, err := h.client.Post(h.mounted.URL+"/api/workspaceFiles/readBytes", "application/json",
		strings.NewReader(wireEnvelope("r-bytes", "workspaceFiles/readBytes", args)))
	if err != nil {
		t.Fatalf("POST readBytes: %v", err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("readBytes 应 200，实际 %d 原文=%s", response.StatusCode, payload)
	}
	contentType := response.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		t.Fatalf("真字节必须走 multipart，实际 %q 原文=%s", contentType, payload)
	}
	envelope, err := officialwire.DecodeMultipartResponse(contentType, payload)
	if err != nil {
		t.Fatalf("multipart 解码失败：%v", err)
	}
	result, _ := envelope["result"].(map[string]any)
	value, _ := result["value"].(map[string]any)
	data, ok := value["data"].([]byte)
	if !ok {
		t.Fatalf("data 应还原成字节，实际 %T", value["data"])
	}
	if string(data) != string(content) {
		t.Fatalf("字节内容不对：%v", data)
	}
	if _, has := value["encoding"]; has {
		t.Fatalf("官方结果里不该有 L1 的 encoding 字段：%v", value)
	}
	if value["absolutePath"] == "" || value["version"] == "" {
		t.Fatalf("absolutePath/version 必须透传：%v", value)
	}
}

// TestOfficialWireSessionEventsFollowsJournal 钉住"真实流读真 journal"。
func TestOfficialWireSessionEventsFollowsJournal(t *testing.T) {
	h := newOfficialWireHarness(t)
	sessionID := "wire-events"
	for i := 1; i <= 2; i++ {
		if _, err := h.journals.Append(sessionID, "assistant/message",
			json.RawMessage(fmt.Sprintf(`{"text":"frame-%d"}`, i)), int64(1_700_000_000_000+i)); err != nil {
			t.Fatalf("journal.Append: %v", err)
		}
	}

	ws, response := dialWireMux(t, h.mounted.URL, h.officialCookies(t))
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("mux 升级应 101，实际 %d", response.StatusCode)
	}
	defer ws.Close()

	ws.writeText(t, `{"type":"open","streamId":"ev","endpoint":"session/events","payload":{"args":{"sessionId":`+
		quoteWire(sessionID)+`,"sinceSeq":0}}}`)

	for want := 1.0; want <= 2.0; want++ {
		frame := ws.readJSON(t, 3*time.Second)
		if frame["type"] != "item" {
			t.Fatalf("应收到 item 帧：%v", frame)
		}
		value, _ := frame["value"].(map[string]any)
		if value["seq"] != want {
			t.Fatalf("seq 应为 %.0f（就是 L1 journal 的 seq），实际 %v", want, value["seq"])
		}
	}

	// 真跟随：测试这边再追加一帧，流必须把它推下来。
	if _, err := h.journals.Append(sessionID, "assistant/message", json.RawMessage(`{"text":"frame-3"}`), 1_700_000_000_003); err != nil {
		t.Fatalf("journal.Append: %v", err)
	}
	frame := ws.readJSON(t, 3*time.Second)
	value, _ := frame["value"].(map[string]any)
	if value["seq"] != 3.0 {
		t.Fatalf("跟随失败：期望 seq=3，实际 %v", value)
	}

	// cancel：客户端 finally 会补发；之后 socket 仍可用（再开一条流）。
	ws.writeText(t, `{"type":"cancel","streamId":"ev"}`)
	ws.writeText(t, `{"type":"open","streamId":"ev2","endpoint":"session/events","payload":{"args":{"sessionId":`+
		quoteWire(sessionID)+`,"sinceSeq":2}}}`)
	next := ws.readJSON(t, 3*time.Second)
	if next["type"] != "item" {
		t.Fatalf("cancel 之后新流应立刻收到 seq=3 那帧：%v", next)
	}
	ws.writeText(t, `{"type":"cancel","streamId":"ev2"}`)
}

// TestOfficialWireConnectCookieCoversMuxAndUnary 是要求 5 的完整走通：
// 同一个 cookie 既升级 mux、也调 unary。
func TestOfficialWireConnectCookieCoversMuxAndUnary(t *testing.T) {
	h := newOfficialWireHarness(t)
	cookies := h.officialCookies(t)

	ws, response := dialWireMux(t, h.mounted.URL, cookies)
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("同一个 cookie 必须能升级 mux，实际 %d", response.StatusCode)
	}
	defer ws.Close()
	ws.writeText(t, `{"type":"open","streamId":"events","endpoint":"$events","payload":{"args":{}}}`)
	frame := ws.readJSON(t, 3*time.Second)
	value, _ := frame["value"].(map[string]any)
	if value["type"] != "ready" {
		t.Fatalf("`$events` 首帧应是 ready：%v", frame)
	}

	// 同一个 cookie 调 unary：未知方法 → 200 + unknown-method（证明认证通了）。
	status, payload := h.post(t, "session/nope", wireEnvelope("r-unary", "session/nope", `{}`))
	result := wireResult(t, status, payload)
	failure, _ := result["error"].(map[string]any)
	if failure["code"] != "gateway/unknown-method" {
		t.Fatalf("认证通了才会走到未知方法判定：%v", failure)
	}
}

// TestOfficialWireEventsResultIsMethodUnavailable 钉住"存在但未接线"与"未知"分开。
func TestOfficialWireEventsResultIsMethodUnavailable(t *testing.T) {
	h := newOfficialWireHarness(t)
	args := `{"clientId":"c","eventId":"e","outcome":{"kind":"next"}}`
	status, payload := h.post(t, "$events/result", wireEnvelope("r-events", "$events/result", args))
	result := wireResult(t, status, payload)
	failure, _ := result["error"].(map[string]any)
	if failure["code"] != "gateway/method-unavailable" {
		t.Fatalf("应回 gateway/method-unavailable，实际 %v", failure)
	}
}

// TestOfficialWireKeepsDottedPathBytesIdentical 钉住并存隔离（真实 Transport 层）。
func TestOfficialWireKeepsDottedPathBytesIdentical(t *testing.T) {
	h := newOfficialWireHarness(t)

	// launch token 在 L1 是一次性的：先换**一份** cookie，随后两条请求都用它。
	// cookie 按 host 存（不含端口），所以同一个值在挂载点与原生服务上都有效。
	plainServer := httptest.NewServer(h.srv)
	defer plainServer.Close()
	jar, _ := cookiejar.New(nil)
	plainClient := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	connect, err := plainClient.Get(plainServer.URL + "/connect?token=" + h.srv.Token())
	if err != nil {
		t.Fatalf("L1 /connect: %v", err)
	}
	connect.Body.Close()
	if connect.StatusCode != http.StatusSeeOther {
		t.Fatalf("L1 /connect 应 303，实际 %d", connect.StatusCode)
	}
	probe, err := http.NewRequest(http.MethodGet, plainServer.URL+"/", nil)
	if err != nil {
		t.Fatalf("构造 cookie 查询: %v", err)
	}
	cookies := jar.Cookies(probe.URL)
	if len(cookies) == 0 {
		t.Fatalf("L1 /connect 没发 cookie")
	}

	request := func(target string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, target, nil)
		if err != nil {
			t.Fatalf("构造请求: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", target, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}

	direct, directBody := request(h.mounted.URL + "/api/session.list")
	plain, plainBody := request(plainServer.URL + "/api/session.list")

	if string(directBody) != string(plainBody) {
		t.Fatalf("dotted 路径字节被改了：\n挂载=%s\n原生=%s", directBody, plainBody)
	}
	if direct.StatusCode != plain.StatusCode {
		t.Fatalf("状态码被改了：%d vs %d", direct.StatusCode, plain.StatusCode)
	}
}

// TestOfficialWireRawUploadMapsAttachmentStore 钉住上行裸字节落进**既有** attachment store。
func TestOfficialWireRawUploadMapsAttachmentStore(t *testing.T) {
	home := t.TempDir()
	store := newAttachmentStore(home)
	journals := transport.NewJournals(nil)
	source := newSessionSource(home, "")
	srv, err := transport.NewServer(withOfficialSessionOptions(home, transport.Options{
		Source:      source,
		Journals:    journals,
		Attachments: store,
	}, nil))
	if err != nil {
		t.Fatalf("transport.NewServer: %v", err)
	}
	defer srv.Close()
	wire, failure := newOfficialWireServer(srv, officialWireDeps{
		Home:        home,
		Journals:    journals,
		Token:       srv.Token,
		Attachments: store,
	})
	if failure != nil {
		t.Fatalf("newOfficialWireServer: %v", failure)
	}
	mounted := httptest.NewServer(wire.Handler(srv))
	defer mounted.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	connect, err := client.Get(mounted.URL + "/connect?token=" + srv.Token() + "&interactions=none")
	if err != nil {
		t.Fatalf("官方 /connect: %v", err)
	}
	connect.Body.Close()

	// 既有 attachment store 的真实策略：只收图片或 UTF-8 文本。
	// 裸字节**编解码**的二进制安全由 internal/officialwire 的用例证明（含 0x00/0xFF）；
	// 这里证明"字节真的进了 L1 那个 store"，所以用一份合法的 UTF-8 内容。
	content := []byte("hello dsh\n世界\n")
	request, err := http.NewRequest(http.MethodPost,
		mounted.URL+officialwire.UploadPath+"?name=blob.bin", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("构造上传请求: %v", err)
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("POST 上传: %v", err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("上传应 200，实际 %d 原文=%s", response.StatusCode, payload)
	}
	var uploaded struct {
		AttachmentID string `json:"attachmentId"`
		Name         string `json:"name"`
		ByteSize     int64  `json:"byteSize"`
	}
	if err := json.Unmarshal(payload, &uploaded); err != nil {
		t.Fatalf("上传结果不是 JSON：%v 原文=%s", err, payload)
	}
	if uploaded.AttachmentID == "" {
		t.Fatalf("必须返回 attachmentId：%s", payload)
	}
	if uploaded.ByteSize != int64(len(content)) {
		t.Fatalf("byteSize 不对：%d（期望 %d）", uploaded.ByteSize, len(content))
	}

	// 用**同一个** attachment store 读回来：字节必须一字不差。
	stored, err := store.Get(uploaded.AttachmentID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if string(stored.Data) != string(content) {
		t.Fatalf("存下来的字节被改写了：%v", stored.Data)
	}
	if stored.Attachment.SHA256 == "" {
		t.Fatalf("既有 store 必须算出 sha256（证明真的走了 L1 那条路）")
	}

	// 二进制内容会被既有 store 拒（真实业务策略），错误必须翻成官方码而不是 500。
	request, err = http.NewRequest(http.MethodPost, mounted.URL+officialwire.UploadPath+"?name=blob.bin",
		bytes.NewReader([]byte{0x00, 0x01, 0xff}))
	if err != nil {
		t.Fatalf("构造二进制上传: %v", err)
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err = client.Do(request)
	if err != nil {
		t.Fatalf("POST 二进制上传: %v", err)
	}
	defer response.Body.Close()
	payload, _ = io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("业务拒绝也必须 200，实际 %d 原文=%s", response.StatusCode, payload)
	}
	var rejected map[string]any
	if err := json.Unmarshal(payload, &rejected); err != nil {
		t.Fatalf("被拒响应不是 JSON：%v 原文=%s", err, payload)
	}
	rejectedResult, _ := rejected["result"].(map[string]any)
	rejectedError, _ := rejectedResult["error"].(map[string]any)
	if rejectedError["code"] != "gateway/arguments-invalid" {
		t.Fatalf("既有 store 的拒绝应翻成 gateway/arguments-invalid，实际 %v", rejectedError)
	}
}

// ---- 裸 WebSocket 客户端（mux 测试用） ----

type wireWS struct {
	conn net.Conn
	br   *bufio.Reader
}

func quoteWire(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func dialWireMux(t *testing.T, base string, cookies []*http.Cookie) (*wireWS, *http.Response) {
	t.Helper()
	address := strings.TrimPrefix(base, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("连接 %s: %v", address, err)
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatalf("生成 key: %v", err)
	}
	var request strings.Builder
	fmt.Fprintf(&request, "GET /api/remote.mux HTTP/1.1\r\nHost: %s\r\n", address)
	request.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\n")
	fmt.Fprintf(&request, "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n",
		base64.StdEncoding.EncodeToString(keyBytes))
	for _, cookie := range cookies {
		fmt.Fprintf(&request, "Cookie: %s=%s\r\n", cookie.Name, cookie.Value)
	}
	request.WriteString("\r\n")
	if _, err := conn.Write([]byte(request.String())); err != nil {
		conn.Close()
		t.Fatalf("写握手: %v", err)
	}
	br := bufio.NewReader(conn)
	response, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		t.Fatalf("读握手响应: %v", err)
	}
	return &wireWS{conn: conn, br: br}, response
}

func (w *wireWS) Close() {
	if w == nil || w.conn == nil {
		return
	}
	_ = w.conn.Close()
}

func (w *wireWS) writeText(t *testing.T, payload string) {
	t.Helper()
	data := []byte(payload)
	mask := [4]byte{7, 8, 9, 10}
	first := byte(0x80) | 0x1
	var header []byte
	switch {
	case len(data) < 126:
		header = []byte{first, 0x80 | byte(len(data))}
	case len(data) <= 0xffff:
		header = make([]byte, 4)
		header[0], header[1] = first, 0x80|126
		binary.BigEndian.PutUint16(header[2:], uint16(len(data)))
	default:
		header = make([]byte, 10)
		header[0], header[1] = first, 0x80|127
		binary.BigEndian.PutUint64(header[2:], uint64(len(data)))
	}
	masked := make([]byte, len(data))
	for i := range data {
		masked[i] = data[i] ^ mask[i%4]
	}
	header = append(header, mask[:]...)
	if _, err := w.conn.Write(append(header, masked...)); err != nil {
		t.Fatalf("写帧: %v", err)
	}
}

func (w *wireWS) readJSON(t *testing.T, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("等帧超时（%v）", timeout)
		}
		_ = w.conn.SetReadDeadline(time.Now().Add(remaining))
		var header [2]byte
		if _, err := io.ReadFull(w.br, header[:]); err != nil {
			t.Fatalf("读帧头: %v", err)
		}
		op := header[0] & 0x0f
		length := uint64(header[1] & 0x7f)
		switch length {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(w.br, ext[:]); err != nil {
				t.Fatalf("读长度: %v", err)
			}
			length = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(w.br, ext[:]); err != nil {
				t.Fatalf("读长度: %v", err)
			}
			length = binary.BigEndian.Uint64(ext[:])
		}
		payload := make([]byte, int(length))
		if _, err := io.ReadFull(w.br, payload); err != nil {
			t.Fatalf("读正文: %v", err)
		}
		switch op {
		case 0x1:
			_ = w.conn.SetReadDeadline(time.Time{})
			var frame map[string]any
			if err := json.Unmarshal(payload, &frame); err != nil {
				t.Fatalf("帧不是 JSON：%v", err)
			}
			return frame
		case 0x9:
			// ping：回 pong（否则心跳会把连接断掉）。
			w.pong(payload)
		default:
			continue
		}
	}
}

// pong 回一条**带掩码**的 pong。
//
// 必须掩码：RFC 6455 §5.1 要求客户端→服务端的帧一律掩码，而 mux 的读侧对
// 未掩码帧是按协议违规关连接的。原来这条不发掩码，于是任何活过 2s 心跳的
// 用例都会以 "读帧头: EOF" 结束——现象看着像"服务端断了"，其实是客户端
// 发了一条非法帧。（2026-11-07 做 `$events` waterfall 时踩到。）
func (w *wireWS) pong(payload []byte) {
	mask := [4]byte{1, 2, 3, 4}
	header := []byte{0x8A, 0x80 | byte(len(payload))}
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_, _ = w.conn.Write(append(append(header, mask[:]...), masked...))
}

// newOfficialWireTestClient 造一个"照官方客户端行为"的 HTTP 客户端：
// 带 cookie jar、不跟随 302（Location 本身是判据）。
func newOfficialWireTestClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// TestOfficialWireSpentTokenUsesMintedCookie 是 L2 E2E P0-2 的回归测试（生产时序）。
//
// 生产时序：桌面壳启动时必然先 `GET /connect?token=…` 把 launch token 兑掉
// （不然 WebView 没有 cookie）。launch token 在 L1 是**一次性**的，官方 wire 的
// L1 代理如果还拿同一张 token 去换，只会拿到 401，于是每个官方 unary 都变成
// `remote-access/denied`（HTTP 200 + result.ok:false）——客户端能连上，但什么方法都调不通。
//
// 本用例两半都是判据：
//
//	1. 生产挂载（officialWireMount 把 LegacyCookie 接成 srv.MintInternalCookie）：
//	   `POST /api/session/page` → 200 + result.ok:true + 真 journal 的 cursor/records；
//	2. 修复前的旧路径（只给 TokenSource、不给 CookieSource）：同一条请求 →
//	   200 + result.ok:false + `remote-access/denied`。
//
// 第 2 半就是"先红"本体：修复被摘掉时，第 1 半会拿到第 2 半的结果。
func TestOfficialWireSpentTokenUsesMintedCookie(t *testing.T) {
	t.Setenv(officialWireEnv, "1")

	home := t.TempDir()
	journals := transport.NewJournals(nil)
	source := newSessionSource(home, "")
	srv, err := transport.NewServer(withOfficialSessionOptions(home, transport.Options{
		Source:   source,
		Journals: journals,
	}, nil))
	if err != nil {
		t.Fatalf("transport.NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	fixture := newOfficialTestSession(t, home, "wire-spent-token")

	// 1) 桌面壳先兑掉 launch token。这是一次性语义：之后 L1 /connect 必 401。
	legacy := httptest.NewServer(srv)
	t.Cleanup(legacy.Close)
	shell := newOfficialWireTestClient(t)
	shellConnect, err := shell.Get(legacy.URL + "/connect?token=" + srv.Token())
	if err != nil {
		t.Fatalf("shell GET /connect: %v", err)
	}
	shellConnect.Body.Close()
	if shellConnect.StatusCode != http.StatusSeeOther {
		t.Fatalf("shell /connect 应 303，实际 %d（launch token 没被消耗？）", shellConnect.StatusCode)
	}

	args := fmt.Sprintf(`{"address":{"kind":"session","sessionId":%q},"maxMessages":50}`, fixture.id)
	postSessionPage := func(t *testing.T, base string, client *http.Client) (int, []byte) {
		t.Helper()
		response, err := client.Post(base+"/api/session/page", "application/json",
			strings.NewReader(wireEnvelope("r-spent", "session/page", args)))
		if err != nil {
			t.Fatalf("POST /api/session/page: %v", err)
		}
		defer response.Body.Close()
		payload, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("读 /api/session/page 响应: %v", err)
		}
		return response.StatusCode, payload
	}

	// 2) 生产挂载：走 officialWireMount，证明**接线**本身（不只是构造函数）把
	//    LegacyCookie 接成了 srv.MintInternalCookie。
	mount := officialWireMount(officialWireDeps{Home: home, Journals: journals})
	if mount == nil {
		t.Fatalf("开关打开时必须给出包装函数")
	}
	handler := mount(srv)
	if handler == nil {
		t.Fatalf("官方 wire 挂载失败（返回 nil）")
	}
	mounted := httptest.NewServer(handler)
	t.Cleanup(mounted.Close)
	officialClient := newOfficialWireTestClient(t)
	officialConnect, err := officialClient.Get(mounted.URL + "/connect?token=" + srv.Token() + "&interactions=none")
	if err != nil {
		t.Fatalf("官方 /connect: %v", err)
	}
	officialConnect.Body.Close()
	if officialConnect.StatusCode != http.StatusFound {
		t.Fatalf("官方 /connect 应 302（官方域有自己的一次性集合），实际 %d", officialConnect.StatusCode)
	}

	status, payload := postSessionPage(t, mounted.URL, officialClient)
	result := wireResult(t, status, payload)
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("生产时序（shell 已兑过 launch token）下必须成功，实际：%s", payload)
	}
	value, _ := result["value"].(map[string]any)
	if value["cursor"] != float64(fixture.lastSeq) {
		t.Fatalf("cursor 应是真 journal 末端 %d，实际 %v", fixture.lastSeq, value["cursor"])
	}
	if records, _ := value["records"].([]any); len(records) == 0 {
		t.Fatalf("records 不该为空（真 v4 会话里有帧）：%v", value)
	}

	// 3) 旧路径反证：只给 TokenSource、不给 CookieSource。
	oldWire, failure := newOfficialWireServer(srv, officialWireDeps{
		Home:     home,
		Journals: journals,
		Token:    srv.Token,
	})
	if failure != nil {
		t.Fatalf("newOfficialWireServer(旧路径): %v", failure)
	}
	oldMounted := httptest.NewServer(oldWire.Handler(srv))
	t.Cleanup(oldMounted.Close)
	oldClient := newOfficialWireTestClient(t)
	oldConnect, err := oldClient.Get(oldMounted.URL + "/connect?token=" + srv.Token() + "&interactions=none")
	if err != nil {
		t.Fatalf("旧路径官方 /connect: %v", err)
	}
	oldConnect.Body.Close()
	if oldConnect.StatusCode != http.StatusFound {
		t.Fatalf("旧路径官方 /connect 应 302，实际 %d", oldConnect.StatusCode)
	}

	status, payload = postSessionPage(t, oldMounted.URL, oldClient)
	result = wireResult(t, status, payload)
	if ok, _ := result["ok"].(bool); ok {
		t.Fatalf("旧路径（只给 TokenSource、launch token 已兑过）必须失败，实际 ok:true：%s", payload)
	}
	oldFailure, _ := result["error"].(map[string]any)
	if oldFailure["code"] != "remote-access/denied" {
		t.Fatalf("旧路径应得到 remote-access/denied，实际 %v（原文 %s）", oldFailure, payload)
	}
}

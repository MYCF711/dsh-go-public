package officialwire

// 本文件是先红探针的**收官形态**（HOST-L2-M1-WIRE）。
//
// 实现前那一条（原样输出见交付报告，exit 1）是：
//
//	POST /api/session/page   （斜杠两段 + 官方信封）
//	→ 501 {"error":{"code":"not_implemented",…}}        ← 路由不存在、信封不被识别
//
// 实现后它变成两条事实，这里各留一条断言：
//
//	1. **L1 一字未动**：同一个请求打到只有 L1 的宿主，仍然 501 not_implemented。
//	   （并存隔离：官方 wire 没有偷偷改既有分派）
//	2. **挂上官方 wire 之后**：同一个请求得到 200 + server-response + rpcId 回显。
//	   （M1 的可观测目标）

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"dshgo/internal/transport"
)

// startLegacyTransport 起一个**只有既有 L1 路由**的宿主（不含 officialwire）。
func startLegacyTransport(t *testing.T) (*transport.Server, string, *http.Client) {
	t.Helper()
	srv, err := transport.NewServer(transport.Options{
		List: func() ([]transport.SessionSummary, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("transport.NewServer: %v", err)
	}
	base, err := srv.Start()
	if err != nil {
		t.Fatalf("transport.Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Get(base + "/connect?token=" + srv.Token())
	if err != nil {
		t.Fatalf("GET /connect: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("/connect 应 303，实际 %d", response.StatusCode)
	}
	return srv, base, client
}

// slashEnvelope 就是"官方客户端会发的那一条"。
func slashEnvelope() string {
	return envelopeBody("red-1", "session/page",
		`{"address":{"kind":"session","sessionId":"x"}}`)
}

// TestLegacyStillRejectsSlashRoute 钉住 L1 侧行为**一字未动**。
func TestLegacyStillRejectsSlashRoute(t *testing.T) {
	_, base, client := startLegacyTransport(t)

	response, err := client.Post(base+"/api/session/page", "application/json", strings.NewReader(slashEnvelope()))
	if err != nil {
		t.Fatalf("POST /api/session/page: %v", err)
	}
	defer response.Body.Close()
	payload := make([]byte, 8192)
	n, _ := response.Body.Read(payload)
	body := payload[:n]
	t.Logf("L1 原样：status=%d body=%s", response.StatusCode, body)

	if response.StatusCode != http.StatusNotImplemented {
		t.Fatalf("L1 对斜杠路径应保持 501 not_implemented，实际 %d body=%s", response.StatusCode, body)
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("L1 错误体不是 JSON：%v", err)
	}
	failure, ok := envelope["error"].(map[string]any)
	if !ok || failure["code"] != "not_implemented" {
		t.Fatalf("L1 错误码应保持 not_implemented：%s", body)
	}
}

// TestOfficialWireServesSlashRouteAfterMount 钉住 M1 的可观测目标。
func TestOfficialWireServesSlashRouteAfterMount(t *testing.T) {
	legacy, base, legacyClient := startLegacyTransport(t)

	wire, failure := NewServer(ServerConfig{
		Secret:      []byte("0123456789abcdef0123456789abcdef"),
		TokenSource: legacy.Token,
		Home:        "/home/test",
	})
	if failure != nil {
		t.Fatalf("NewServer: %v", failure)
	}
	if err := wire.Handle("session/page", Binding{
		Descriptor: &Descriptor{Required: []string{"address"}, Optional: []string{"maxThroughSeq"}},
		Handle: func(_ context.Context, _ map[string]json.RawMessage) (any, *Error) {
			return map[string]any{"cursor": 1.0, "records": []any{}, "hasMore": false}, nil
		},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	mounted := httptest.NewServer(wire.Handler(legacy))
	defer mounted.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	connect, err := client.Get(mounted.URL + "/connect?token=" + legacy.Token() + "&interactions=none")
	if err != nil {
		t.Fatalf("官方 /connect: %v", err)
	}
	connect.Body.Close()
	if connect.StatusCode != http.StatusFound {
		t.Fatalf("官方 /connect 应 302，实际 %d", connect.StatusCode)
	}
	if location := connect.Header.Get("Location"); location != "./api/remote.mux" {
		t.Fatalf("Location 不对：%q", location)
	}

	response, err := client.Post(mounted.URL+"/api/session/page", "application/json", strings.NewReader(slashEnvelope()))
	if err != nil {
		t.Fatalf("POST /api/session/page: %v", err)
	}
	defer response.Body.Close()
	var envelope map[string]any
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	t.Logf("官方 wire 原样：status=%d body=%v", response.StatusCode, envelope)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("业务响应必须 200，实际 %d", response.StatusCode)
	}
	if envelope["type"] != "server-response" {
		t.Fatalf("不是 server-response：%v", envelope)
	}
	if envelope["rpcId"] != "red-1" {
		t.Fatalf("rpcId 必须逐字回显：%v", envelope["rpcId"])
	}
	result, _ := envelope["result"].(map[string]any)
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("result.ok 应为 true：%v", envelope)
	}

	// 同一个挂载点上，dotted 路径仍然是 L1 的字节。
	legacyResponse, err := legacyClient.Post(base+"/api/session.list", "application/json", nil)
	if err != nil {
		t.Fatalf("L1 POST session.list: %v", err)
	}
	defer legacyResponse.Body.Close()
	legacyBody := readAll(t, legacyResponse)

	// cookie 是按 host 存的（不含端口），所以同一个 L1 cookie 在挂载点上也有效：
	// dotted 路径走的是同一个 legacy handler，凭据也该是同一份。
	dotted, err := legacyClient.Post(mounted.URL+"/api/session.list", "application/json", nil)
	if err != nil {
		t.Fatalf("mounted POST session.list: %v", err)
	}
	defer dotted.Body.Close()
	mountedBody := readAll(t, dotted)

	if string(legacyBody) != string(mountedBody) {
		t.Fatalf("dotted 路径的响应字节被改了：\nL1  =%s\n挂载=%s", legacyBody, mountedBody)
	}
}

func readAll(t *testing.T, response *http.Response) []byte {
	t.Helper()
	var out strings.Builder
	buf := make([]byte, 8192)
	for {
		n, err := response.Body.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return []byte(out.String())
}

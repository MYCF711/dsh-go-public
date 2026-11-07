package officialwire

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// countingHandler 记录被调用次数，并回一段确定性字节。
type countingHandler struct {
	status int
	body   string
	hits   int
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.hits++
	status := h.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Legacy", "yes")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(h.body))
}

// record 跑一次请求并把完整响应录下来。
func record(t *testing.T, handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	request := httptest.NewRequest(method, target, reader)
	request.Host = "127.0.0.1:7777"
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// assertSameResponse 逐字节比较两份响应。
func assertSameResponse(t *testing.T, label string, want, got *httptest.ResponseRecorder) {
	t.Helper()
	if want.Code != got.Code {
		t.Fatalf("%s: 状态码变了 want=%d got=%d", label, want.Code, got.Code)
	}
	wantHeader := want.Result().Header
	gotHeader := got.Result().Header
	if len(wantHeader) != len(gotHeader) {
		t.Fatalf("%s: 响应头数量变了 want=%v got=%v", label, wantHeader, gotHeader)
	}
	for key, wantValues := range wantHeader {
		gotValues, ok := gotHeader[key]
		if !ok {
			t.Fatalf("%s: 少了响应头 %s", label, key)
		}
		if len(wantValues) != len(gotValues) {
			t.Fatalf("%s: 响应头 %s 值数量变了", label, key)
		}
		for i := range wantValues {
			if wantValues[i] != gotValues[i] {
				t.Fatalf("%s: 响应头 %s 变了 want=%q got=%q", label, key, wantValues[i], gotValues[i])
			}
		}
	}
	if !bytes.Equal(want.Body.Bytes(), got.Body.Bytes()) {
		t.Fatalf("%s: 正文变了\nwant=%q\ngot =%q", label, want.Body.String(), got.Body.String())
	}
}

// TestRouterDisabledIsByteIdentical 是"关闭时既有行为字节不变"的证明。
func TestRouterDisabledIsByteIdentical(t *testing.T) {
	legacy := &countingHandler{status: http.StatusOK, body: `{"result":{"sessions":[]}}` + "\n"}
	router := Router{Enabled: false, Legacy: legacy, Wire: nil}

	cases := []struct{ method, target, body string }{
		{http.MethodPost, "/api/session.list", `{}`},
		{http.MethodPost, "/api/session.page", `{"id":"a"}`},
		{http.MethodPost, "/api/session/page", envelopeBody("r-1", "session/page", `{}`)},
		{http.MethodGet, "/connect?token=abc", ""},
		{http.MethodGet, "/connect?token=abc&interactions=none", ""},
		{http.MethodGet, "/ws", ""},
		{http.MethodGet, "/debug/stats", ""},
		{http.MethodGet, "/", ""},
		{http.MethodPut, "/api/session.list", `{}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.method+" "+testCase.target, func(t *testing.T) {
			want := record(t, legacy, testCase.method, testCase.target, testCase.body)
			got := record(t, router, testCase.method, testCase.target, testCase.body)
			assertSameResponse(t, "Router{Enabled:false}", want, got)
		})
	}
}

// TestRouterEnabledLeavesLegacyPathsIdentical 是**并存隔离**的证明：
//
// 开关打开后，凡是"不是官方两段路径"的请求，L1 拿到的字节一字不变。
func TestRouterEnabledLeavesLegacyPathsIdentical(t *testing.T) {
	cfg := testServerConfig()
	wire, failure := NewServer(cfg)
	if failure != nil {
		t.Fatalf("NewServer: %v", failure)
	}
	legacy := &countingHandler{status: http.StatusOK, body: `{"result":{"sessions":[]}}` + "\n"}
	router := wire.Handler(legacy)

	cases := []struct{ method, target, body string }{
		{http.MethodPost, "/api/session.list", `{}`},
		{http.MethodPost, "/api/session.list", `{"anything":true}`},
		{http.MethodPost, "/api/session.page", `{"id":"a"}`},   // dotted：一段
		{http.MethodPost, "/api/attachment.get", `{"id":"x"}`}, // dotted
		{http.MethodPost, "/api/a!b/c", `{}`},                  // 段非法（! 不在官方字符集）
		{http.MethodPost, "/api/session/page/extra", `{}`},     // 三段
		{http.MethodGet, "/connect?token=abc", ""},             // 没有 interactions = L1 引导
		{http.MethodGet, "/ws", ""},
		{http.MethodGet, "/debug/stats", ""},
		{http.MethodGet, "/", ""},
		{http.MethodPut, "/api/session.list", `{}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.method+" "+testCase.target, func(t *testing.T) {
			legacy.hits = 0
			want := record(t, legacy, testCase.method, testCase.target, testCase.body)
			wantHits := legacy.hits
			got := record(t, router, testCase.method, testCase.target, testCase.body)
			assertSameResponse(t, "Router{Enabled:true} legacy 路径", want, got)
			if legacy.hits != wantHits+1 {
				t.Fatalf("%s 应原样落到 legacy（hits=%d → %d）", testCase.target, wantHits, legacy.hits)
			}
		})
	}
}

// TestRouterEnabledInterceptsSlashRoute 钉住两段路径被官方层截走。
func TestRouterEnabledInterceptsSlashRoute(t *testing.T) {
	wire, failure := NewServer(testServerConfig())
	if failure != nil {
		t.Fatalf("NewServer: %v", failure)
	}
	if err := wire.Handle("session/page", Binding{
		Descriptor: &Descriptor{Required: []string{"address"}},
		Handle:     func(_ context.Context, _ map[string]json.RawMessage) (any, *Error) { return map[string]any{"ok": true}, nil },
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	legacy := &countingHandler{status: http.StatusOK, body: "legacy\n"}
	router := wire.Handler(legacy)

	// 未认证 → 401，legacy 一次都不该被调。
	unauthorized := record(t, router, http.MethodPost, "/api/session/page",
		envelopeBody("r-1", "session/page", `{"address":{"kind":"session","sessionId":"s"}}`))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("官方路径未认证应 401，实际 %d", unauthorized.Code)
	}
	if legacy.hits != 0 {
		t.Fatalf("官方路径不该落到 legacy（hits=%d）", legacy.hits)
	}

	// 未注册的两段路径 → 200 + unknown-method（不是 legacy 的 501）。
	unknown := record(t, router, http.MethodPost, "/api/whatever/thing", envelopeBody("r-2", "whatever/thing", `{}`))
	if unknown.Code != http.StatusUnauthorized {
		// 未认证仍然先 401：认证在方法解析之前，这是刻意的顺序。
		t.Fatalf("未认证的未知方法也应 401（认证先于方法解析），实际 %d", unknown.Code)
	}
	if legacy.hits != 0 {
		t.Fatalf("未知两段路径也不该落到 legacy（hits=%d）", legacy.hits)
	}
}

// TestRouterMuxPathNeverFallsThroughToLegacy 钉住 /api/remote.mux 归官方层。
func TestRouterMuxPathNeverFallsThroughToLegacy(t *testing.T) {
	wire, failure := NewServer(testServerConfig())
	if failure != nil {
		t.Fatalf("NewServer: %v", failure)
	}
	legacy := &countingHandler{status: http.StatusOK, body: "legacy\n"}
	router := wire.Handler(legacy)

	response := record(t, router, http.MethodGet, "/api/remote.mux", "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("没有 cookie 的 mux 升级必须 401，实际 %d", response.Code)
	}
	if legacy.hits != 0 {
		t.Fatalf("mux 路径不该落到 legacy")
	}
}

// TestRouterConnectDispatchByInteractions 钉住两条引导路径的分派依据。
func TestRouterConnectDispatchByInteractions(t *testing.T) {
	wire, failure := NewServer(testServerConfig())
	if failure != nil {
		t.Fatalf("NewServer: %v", failure)
	}
	legacy := &countingHandler{status: http.StatusSeeOther, body: ""}
	router := wire.Handler(legacy)

	// 没有 interactions → L1。
	record(t, router, http.MethodGet, "/connect?token=abc", "")
	if legacy.hits != 1 {
		t.Fatalf("/connect 无 interactions 应落到 L1，hits=%d", legacy.hits)
	}

	// 有 interactions → 官方（错 token 走官方 401）。
	official := record(t, router, http.MethodGet, "/connect?token=wrong&interactions=none", "")
	if official.Code != http.StatusUnauthorized {
		t.Fatalf("官方 /connect 错 token 应 401，实际 %d", official.Code)
	}
	if legacy.hits != 1 {
		t.Fatalf("官方 /connect 不该落到 L1，hits=%d", legacy.hits)
	}
}

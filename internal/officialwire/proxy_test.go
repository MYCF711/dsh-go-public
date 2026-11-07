package officialwire

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeLegacy 是一个最小的 L1 宿主：只用固定回答证明"翻译"这一层。
//
// 真正的 L1 语义由 transport 自己的测试保证；这里要证明的是 **官方 wire 没有
// 自己的第二套业务逻辑**——它把 args 原样送进去、把结果/错误翻译回来。
type fakeLegacy struct {
	token string

	mu       sync.Mutex
	paths    []string
	cookies  []string
	connects int
}

func (f *fakeLegacy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	if cookie, err := r.Cookie("dshd_session"); err == nil {
		f.cookies = append(f.cookies, cookie.Value)
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	switch r.URL.Path {
	case "/connect":
		f.mu.Lock()
		f.connects++
		f.mu.Unlock()
		if r.URL.Query().Get("token") != f.token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"unauthorized","hint":"","retryable":false}}`))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "dshd_session", Value: "legacy-cookie", Path: "/"})
		w.WriteHeader(http.StatusSeeOther)
	case "/api/session.page":
		_, _ = w.Write([]byte(`{"result":{"cursor":3,"records":[],"hasMore":false}}`))
	case "/api/workspaceFiles.readBytes":
		_, _ = w.Write([]byte(`{"result":{"offset":0,"data":"aGVsbG8=","encoding":"base64","eof":true,"absolutePath":"/w/a.bin","version":"v1"}}`))
	case "/api/missing.thing":
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"no_such_session","message":"没有这个会话","hint":"","retryable":false}}`))
	case "/api/unwired.thing":
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":{"code":"not_implemented","message":"not implemented","hint":"","retryable":false}}`))
	case "/api/bad.thing":
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"bad_request","message":"参数不对","hint":"","retryable":false}}`))
	case "/api/denied.thing":
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"forbidden","hint":"","retryable":false}}`))
	case "/api/boom.thing":
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"炸了","hint":"","retryable":true}}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found","hint":"","retryable":false}}`))
	}
}

func newFakeLegacy(token string) *fakeLegacy { return &fakeLegacy{token: token} }

// TestLegacyClientExchangesTokenAndReusesCookie 钉住内部服务凭据那条链。
func TestLegacyClientExchangesTokenAndReusesCookie(t *testing.T) {
	legacy := newFakeLegacy("tok")
	client := &LegacyClient{Handler: legacy, TokenSource: func() string { return "tok" }}

	result, failure := client.Call(context.Background(), "session.page", json.RawMessage(`{"address":{"kind":"session","sessionId":"s"}}`))
	if failure != nil {
		t.Fatalf("Call: %v", failure)
	}
	if !strings.Contains(string(result), `"cursor":3`) {
		t.Fatalf("result 没透传：%s", result)
	}
	legacy.mu.Lock()
	connects := legacy.connects
	cookies := append([]string(nil), legacy.cookies...)
	legacy.mu.Unlock()
	if connects != 1 {
		t.Fatalf("launch token 只该换一次，实际 %d 次", connects)
	}
	if len(cookies) == 0 || cookies[len(cookies)-1] != "legacy-cookie" {
		t.Fatalf("调用 L1 时必须带内部 cookie，实际 %v", cookies)
	}

	// 第二次调用复用同一份 cookie，不再换 token。
	if _, failure := client.Call(context.Background(), "session.page", json.RawMessage(`{}`)); failure != nil {
		t.Fatalf("第二次 Call: %v", failure)
	}
	legacy.mu.Lock()
	connects = legacy.connects
	legacy.mu.Unlock()
	if connects != 1 {
		t.Fatalf("第二次调用不该再换 token，实际 %d 次", connects)
	}
}

// TestLegacyClientTranslatesErrors 钉住 L1 四字段错误 → 官方码空间。
func TestLegacyClientTranslatesErrors(t *testing.T) {
	legacy := newFakeLegacy("tok")
	client := &LegacyClient{Handler: legacy, TokenSource: func() string { return "tok" }}

	cases := []struct {
		dotted string
		code   string
	}{
		{"missing.thing", "missing/not-found"},
		{"unwired.thing", CodeMethodUnavailable},
		{"bad.thing", CodeArgumentsInvalid},
		{"denied.thing", CodeAccessDenied},
		{"boom.thing", CodeInternal},
	}
	for _, testCase := range cases {
		t.Run(testCase.dotted, func(t *testing.T) {
			_, failure := client.Call(context.Background(), testCase.dotted, json.RawMessage(`{}`))
			if failure == nil {
				t.Fatalf("必须翻译成官方错误，实际成功了")
			}
			if failure.Code != testCase.code {
				t.Fatalf("码应为 %s，实际 %s（%v）", testCase.code, failure.Code, failure.Details)
			}
			if failure.Details == nil {
				t.Fatalf("翻译后的错误必须带 details")
			}
		})
	}
}

// TestLegacyClientEmptyResultFailsLoud 钉住"空 result 不许当成功"。
func TestLegacyClientEmptyResultFailsLoud(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":null}`))
	})
	client := &LegacyClient{Handler: handler}
	_, failure := client.Call(context.Background(), "session.page", json.RawMessage(`{}`))
	if failure == nil || failure.Code != CodeInternal {
		t.Fatalf("空 result 必须报 internal，实际 %v", failure)
	}
}

// TestLegacyClientMissingHandlerFailsLoud 钉住 nil handler 不 panic。
func TestLegacyClientMissingHandlerFailsLoud(t *testing.T) {
	client := &LegacyClient{}
	_, failure := client.Call(context.Background(), "session.page", json.RawMessage(`{}`))
	if failure == nil || failure.Code != CodeInternal {
		t.Fatalf("没有下游处理器时必须报 internal，实际 %v", failure)
	}
}

// TestLegacyBindingDecodesAnyTree 钉住"binding 交给附件编码器的是 any 树"。
//
// 如果这里返回 json.RawMessage，协议层就走不进嵌套结构，[]byte 永远不会被
// 抽成 multipart part —— 附件功能会"看起来实现了、实际一个字节也发不出去"。
func TestLegacyBindingDecodesAnyTree(t *testing.T) {
	legacy := newFakeLegacy("tok")
	client := &LegacyClient{Handler: legacy, TokenSource: func() string { return "tok" }}

	called := false
	binding := client.Binding("session.page", nil, func(result json.RawMessage) (any, *Error) {
		called = true
		return decodeAny(result)
	})
	value, failure := binding.Handle(context.Background(), map[string]json.RawMessage{})
	if failure != nil {
		t.Fatalf("binding: %v", failure)
	}
	if !called {
		t.Fatalf("transform 没被调用")
	}
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("默认应解成 any 树（map[string]any），实际 %T", value)
	}
	if object["cursor"] != 3.0 {
		t.Fatalf("字段没透传：%v", object)
	}

	// readBytes：把 base64 换成 []byte 之后，编码器必须抽出附件。
	readBytes := client.Binding("workspaceFiles.readBytes", readBytesTestDescriptor(),
		func(result json.RawMessage) (any, *Error) {
			var object map[string]any
			if err := json.Unmarshal(result, &object); err != nil {
				return nil, Errorf(CodeInternal, "解 result: %v", err)
			}
			object["data"] = []byte("hello")
			delete(object, "encoding")
			return object, nil
		})
	bytesValue, failure := readBytes.Handle(context.Background(), map[string]json.RawMessage{})
	if failure != nil {
		t.Fatalf("readBytes binding: %v", failure)
	}
	contentType, _, err := EncodeResult("r-1", bytesValue)
	if err != nil {
		t.Fatalf("EncodeResult: %v", err)
	}
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		t.Fatalf("含 []byte 的结果必须走 multipart，实际 %q", contentType)
	}
}

func readBytesTestDescriptor() *Descriptor {
	return &Descriptor{Required: []string{"path"}}
}

// TestProxyCallPreservesArgs 钉住"args 原样转发"（不改字段名、不丢字段）。
func TestProxyCallPreservesArgs(t *testing.T) {
	var seen []byte
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		seen = append([]byte(nil), buf[:n]...)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"ok":true}}`))
	})
	client := &LegacyClient{Handler: handler}
	args := json.RawMessage(`{"address":{"kind":"session","sessionId":"s"},"maxMessages":5}`)
	if _, failure := client.Call(context.Background(), "session.page", args); failure != nil {
		t.Fatalf("Call: %v", failure)
	}
	if string(seen) != string(args) {
		t.Fatalf("args 被改写了：got=%s want=%s", seen, args)
	}
}

// TestProxySurfacesTransportFailure 钉住"L1 返回非 JSON 时是 internal，不是静默成功"。
func TestProxySurfacesTransportFailure(t *testing.T) {
	handler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("<html>oops</html>"))
	}))
	defer handler.Close()
	client := &LegacyClient{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<html>oops</html>", http.StatusInternalServerError)
	})}
	_, failure := client.Call(context.Background(), "session.page", json.RawMessage(`{}`))
	if failure == nil || failure.Code != CodeInternal {
		t.Fatalf("非 JSON 响应必须报 internal，实际 %v", failure)
	}
}

package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 请求必须署自己的名：不设 UA 时 Go 会发 "Go-http-client/1.1"，
// 网关的客户端列表里就多一个不知名的调用方（2026-11-03 用户截图反馈）。
func TestRequestCarriesUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	if _, err := c.Stream(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(Event) error { return nil }); err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	if got == "" || got == "Go-http-client/1.1" {
		t.Fatalf("UA 没署自己的名：%q", got)
	}
}

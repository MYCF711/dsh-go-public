package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `data: []` 与 `data: null` 必须分开——它们的下一步完全不同。
//
// 合成一个空列表是最糟的处理：界面显示"没有可用模型"，而真实原因可能是
// 打到了别的服务/网关坏了。那时候用户去挂模型是白费功夫。
func TestListModelsDistinguishesNullFromEmpty(t *testing.T) {
	serve := func(body string) *Client {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		c := New(srv.URL+"/v1/", "")
		return c
	}

	// 真·空列表：不是错误，返回空切片
	list, err := serve(`{"data":[]}`).ListModels(context.Background())
	if err != nil {
		t.Fatalf("data:[] 是合法的空列表，不该报错：%v", err)
	}
	if len(list) != 0 {
		t.Fatalf("应当是空列表：%+v", list)
	}

	// data:null → 形状不对，必须报错且说清不是"没有模型"
	_, err = serve(`{"data":null}`).ListModels(context.Background())
	if err == nil {
		t.Fatal("data:null 必须报错")
	}
	if !strings.Contains(err.Error(), "响应形状不对") || strings.Contains(err.Error(), "没有可用模型") {
		t.Fatalf("要说清这不是空列表：%v", err)
	}

	// 根本没有 data 键 → 同上
	if _, err := serve(`{"object":"list"}`).ListModels(context.Background()); err == nil {
		t.Fatal("没有 data 键必须报错")
	}
}

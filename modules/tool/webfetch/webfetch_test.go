package webfetch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 正常取回：正文就是模型看到的东西，Data 里有状态码与字节数。
func TestFetchOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("hello 世界"))
	}))
	defer srv.Close()

	tool := &Tool{}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "hello 世界") {
		t.Fatalf("正文不对: %q", res.Text)
	}
	d := res.Data.(data)
	if d.Status != 200 || d.Bytes == 0 {
		t.Fatalf("Data 不对: %+v", d)
	}
}

// 404：不是"成功但空"，要带状态码 + 一段正文 + 下一步。
func TestFetch404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte("not here"))
	}))
	defer srv.Close()

	tool := &Tool{}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if err == nil {
		t.Fatal("404 必须返回错误")
	}
	if !strings.Contains(res.Summary, "404") || !strings.Contains(res.Text, "not here") {
		t.Fatalf("要说清状态码并带回正文片段: %q / %q", res.Summary, res.Text)
	}
	if !strings.Contains(res.Hint, "404") {
		t.Fatalf("Hint 要解读状态码: %q", res.Hint)
	}
}

// 403 的说法要不一样：多半是反爬/要登录，不是地址错。
func TestFetch403Hint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer srv.Close()

	tool := &Tool{}
	res, _ := tool.Exec(context.Background(), json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if !strings.Contains(res.Hint, "登录") {
		t.Fatalf("403 的 Hint 要指向登录/反爬: %q", res.Hint)
	}
}

// 超大正文：截断，并且在 Hint 里说清"别指望一次取全"。
func TestFetchTruncates(t *testing.T) {
	big := strings.Repeat("x", maxBytes+100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(big))
	}))
	defer srv.Close()

	tool := &Tool{}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Text) > maxBytes+2 {
		t.Fatalf("正文没截断: %d 字节", len(res.Text))
	}
	if d := res.Data.(data); !d.Truncated {
		t.Fatal("Data 要标记截断")
	}
	if !strings.Contains(res.Hint, "截断") {
		t.Fatalf("Hint 要说清截断: %q", res.Hint)
	}
}

// 协议与空值：file:// 要拒（别把本地文件当网页读）。
func TestFetchBadInput(t *testing.T) {
	tool := &Tool{}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"url":"file:///etc/passwd"}`))
	if err == nil || !strings.Contains(res.Hint, "http") {
		t.Fatalf("非 http(s) 要拒: %v %q", err, res.Hint)
	}
	if _, err := tool.Exec(context.Background(), json.RawMessage(`{"url":"  "}`)); err == nil {
		t.Fatal("空 url 要拒")
	}
	if _, err := tool.Exec(context.Background(), json.RawMessage(`{"url":"https://"}`)); err == nil {
		t.Fatal("缺主机名要拒")
	}
}

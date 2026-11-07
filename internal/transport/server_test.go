package transport

import (
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

func startServer(t *testing.T) (*Server, string, *http.Client) {
	t.Helper()
	s, err := NewServer(Options{List: func() ([]SessionSummary, error) {
		return []SessionSummary{{ID: "a", Title: "t", CWD: "/w", UpdatedAt: "2026-01-01"}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	base, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	jar, _ := cookiejar.New(nil)
	return s, base, &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse // 看 303 本身，不要自动跟
	}}
}

// 端口 0：地址必须由 OS 分配，不许写死。两次起服务端口必须不同。
func TestStartUsesEphemeralPort(t *testing.T) {
	_, a, _ := startServer(t)
	_, b, _ := startServer(t)
	if a == b {
		t.Fatalf("两次起服务拿到同一个地址 %s —— 端口没有被 OS 分配", a)
	}
	if !strings.HasPrefix(a, "http://127.0.0.1:") {
		t.Fatalf("地址形状不对: %s", a)
	}
}

// 没有 cookie 就打 /api —— 必须 401，不许"本机回环就放行"。
func TestAPIRequiresCookie(t *testing.T) {
	_, base, c := startServer(t)
	resp, err := c.Post(base+"/api/session.list", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("没有 cookie 要 401，拿到 %d", resp.StatusCode)
	}
}

// token 换 cookie，换完就能用；token 错则 401。
func TestConnectExchangesTokenForCookie(t *testing.T) {
	s, base, c := startServer(t)

	if resp, err := c.Get(base + "/connect?token=wrong"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("错 token 要 401，拿到 %d", resp.StatusCode)
		}
	}

	resp, err := c.Get(base + "/connect?token=" + s.Token())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("对 token 要 303，拿到 %d", resp.StatusCode)
	}
	if len(resp.Cookies()) == 0 {
		t.Fatal("303 必须带 Set-Cookie")
	}

	r2, err := c.Post(base+"/api/session.list", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("换了 cookie 就该能用，拿到 %d", r2.StatusCode)
	}
	b, _ := io.ReadAll(r2.Body)
	if !strings.Contains(string(b), `"sessions"`) {
		t.Fatalf("正文不对: %s", b)
	}
}

// launch token 是一次性的：换过再用必须 401。
func TestTokenIsSingleUse(t *testing.T) {
	s, base, _ := startServer(t)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	tok := s.Token()

	resp, err := c.Get(base + "/connect?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	resp2, err := c.Get(base + "/connect?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token 用第二次要 401，拿到 %d", resp2.StatusCode)
	}
}

// /ws 和 /api 一样要 cookie：没有 cookie 不许升级。
func TestWSRequiresCookie(t *testing.T) {
	_, base, c := startServer(t)
	req, _ := http.NewRequest(http.MethodGet, base+"/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("没 cookie 升级要 401，拿到 %d", resp.StatusCode)
	}
}

// sinceSeq 必须原样传给宿主侧的实现——传 0 会让每次重连都从头重放。
// 这条测试不依赖真实语料：用回调把入参记下来就行。
func TestHistoryPassesSinceSeqThrough(t *testing.T) {
	var got struct {
		id       string
		sinceSeq int
		limit    int
	}
	s, err := NewServer(Options{History: func(id string, sinceSeq, limit int) (Page, error) {
		got.id, got.sinceSeq, got.limit = id, sinceSeq, limit
		return Page{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	base, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	if r, err := c.Get(base + "/connect?token=" + s.Token()); err != nil {
		t.Fatal(err)
	} else {
		r.Body.Close()
	}
	body := strings.NewReader(`{"sessionId":"abc","sinceSeq":42,"limit":7}`)
	resp, err := c.Post(base+"/api/session.history", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.id != "abc" || got.sinceSeq != 42 || got.limit != 7 {
		t.Fatalf("入参没原样透传：%+v", got)
	}
}

// prompt 的两种结局要分开报：被接受 → 200；上一轮没结束 → 409。
// 界面靠状态码决定"等一下再说"还是"参数错了"，混成一个就再也分不出来。
func TestPromptStatusCodes(t *testing.T) {
	s, err := NewServer(Options{Prompt: func(sid, text string) (PromptResult, error) {
		if text == "busy" {
			return PromptResult{}, errBusyForTest
		}
		return PromptResult{Accepted: true, TurnID: "t-1"}, nil
	}, Cancel: func(sid string) error { return nil },
		Create: func(mode string) (string, error) { return "session-new", nil }})
	if err != nil {
		t.Fatal(err)
	}
	base, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	if r, err := c.Get(base + "/connect?token=" + s.Token()); err != nil {
		t.Fatal(err)
	} else {
		r.Body.Close()
	}
	post := func(method, body string) (int, string) {
		resp, err := c.Post(base+"/api/"+method, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := post("session.prompt", `{"sessionId":"a","text":"hi"}`); code != 200 {
		t.Fatalf("正常 prompt 要 200，拿到 %d：%s", code, body)
	}
	if code, _ := post("session.prompt", `{"sessionId":"a","text":"busy"}`); code != 409 {
		t.Fatalf("上一轮没结束要 409，拿到 %d", code)
	}
	if code, _ := post("session.cancel", `{"sessionId":"a"}`); code != 200 {
		t.Fatalf("cancel 要 200，拿到 %d", code)
	}
	code, body := post("session.create", `{"mode":"default"}`)
	if code != 200 || !strings.Contains(body, "session-new") {
		t.Fatalf("create 要 200 且带 id，拿到 %d：%s", code, body)
	}
}

var errBusyForTest = errors.New("上一轮还没结束")

package officialwire

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCookieNameIsAuthorityBoundUnpadded 钉住 cookie 名的字节形状（EVIDENCE §4.2）。
//
// 必须是**无填充** base64url：cookie-name 是 RFC6265 token，带 `=` 的名字
// 客户端根本存不下——那会让 /connect 看起来成功、后续请求却全是 401。
func TestCookieNameIsAuthorityBoundUnpadded(t *testing.T) {
	name := CookieName("127.0.0.1:52341")
	sum := sha256.Sum256([]byte("127.0.0.1:52341"))
	want := "dsh-auth-" + base64.RawURLEncoding.EncodeToString(sum[:])
	if name != want {
		t.Fatalf("cookie 名不对：got=%q want=%q", name, want)
	}
	if strings.ContainsAny(name, "=+/") {
		t.Fatalf("cookie 名里有非法字符：%q", name)
	}
	if CookieName("127.0.0.1:1") == name {
		t.Fatalf("不同 authority 必须得到不同的 cookie 名")
	}
}

// TestConnectIssuesV2Cookie 钉住引导响应（EVIDENCE §4.1、§4.2）。
func TestConnectIssuesV2Cookie(t *testing.T) {
	_, ts, client := startWire(t, testServerConfig(), nil)
	response := doConnect(t, ts, client, InteractionsAnswer)

	if response.StatusCode != http.StatusFound {
		t.Fatalf("应 302，实际 %d", response.StatusCode)
	}
	if got := response.Header.Get("Location"); got != "./api/remote.mux" {
		t.Fatalf("Location 必须是 ./api/remote.mux，实际 %q", got)
	}
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应只发一份 cookie，实际 %d", len(cookies))
	}
	cookie := cookies[0]
	if !strings.HasPrefix(cookie.Name, "dsh-auth-") {
		t.Fatalf("cookie 名应带 dsh-auth- 前缀：%q", cookie.Name)
	}
	if !strings.HasPrefix(cookie.Value, "v2.") {
		t.Fatalf("cookie 值应是 v2 payload：%q", cookie.Value)
	}
	if parts := strings.Split(cookie.Value, "."); len(parts) != 3 {
		t.Fatalf("cookie 值应是 v2.<body>.<sig> 三段：%q", cookie.Value)
	}
	if !cookie.HttpOnly {
		t.Fatalf("cookie 必须 HttpOnly")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie 必须 SameSite=Strict，实际 %v", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Fatalf("cookie Path 必须是 /：%q", cookie.Path)
	}
	if cookie.MaxAge <= 0 {
		t.Fatalf("cookie 必须有 Max-Age：%d", cookie.MaxAge)
	}
	if cookie.Expires.IsZero() {
		t.Fatalf("cookie 必须带 Expires")
	}
	if cookie.Secure {
		t.Fatalf("明文 HTTP 下不许加 Secure（浏览器会直接丢弃这份 cookie）")
	}
}

// TestConnectInteractionsRules 钉住 interactions 必须出现恰好一次且取值合法。
func TestConnectInteractionsRules(t *testing.T) {
	_, ts, client := startWire(t, testServerConfig(), nil)

	cases := []struct {
		name string
		url  string
	}{
		{"interactions 出现两次", ts.URL + "/connect?token=" + testToken + "&interactions=none&interactions=answer"},
		{"interactions 非法值", ts.URL + "/connect?token=" + testToken + "&interactions=maybe"},
		{"interactions 空值", ts.URL + "/connect?token=" + testToken + "&interactions="},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response, err := client.Get(testCase.url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("应 400，实际 %d", response.StatusCode)
			}
		})
	}

	// 合法的两个取值都必须过。
	for _, interactions := range []string{InteractionsNone, InteractionsAnswer} {
		t.Run("合法 "+interactions, func(t *testing.T) {
			_, freshTS, freshClient := startWire(t, testServerConfig(), nil)
			response := doConnect(t, freshTS, freshClient, interactions)
			if response.StatusCode != http.StatusFound {
				t.Fatalf("interactions=%s 应 302，实际 %d", interactions, response.StatusCode)
			}
		})
	}
}

// TestConnectTokenRules 钉住 launch token 的 401 与一次性语义。
func TestConnectTokenRules(t *testing.T) {
	_, ts, client := startWire(t, testServerConfig(), nil)

	bad, err := client.Get(ts.URL + "/connect?token=wrong&interactions=none")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错 token 应 401，实际 %d", bad.StatusCode)
	}

	first := doConnect(t, ts, client, InteractionsNone)
	if first.StatusCode != http.StatusFound {
		t.Fatalf("第一次换 cookie 应 302，实际 %d", first.StatusCode)
	}
	second, err := client.Get(ts.URL + "/connect?token=" + testToken + "&interactions=none")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	second.Body.Close()
	if second.StatusCode != http.StatusUnauthorized {
		t.Fatalf("同一个 launch token 第二次必须 401，实际 %d", second.StatusCode)
	}
}

// TestCookieVerifyRejectsBadPayloads 钉住 HMAC / authority / 过期三条校验。
func TestCookieVerifyRejectsBadPayloads(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	secret := make([]byte, 32)
	auth, failure := NewAuth(AuthConfig{
		Secret: secret,
		MaxAge: time.Minute,
		Now:    func() time.Time { return clock },
	})
	if failure != nil {
		t.Fatalf("NewAuth: %v", failure)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000/connect", nil)
	cookie, failure := auth.Issue(request, "peer-1")
	if failure != nil {
		t.Fatalf("Issue: %v", failure)
	}

	verify := func(value string, host string) *Error {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/x/y", nil)
		req.Host = host
		req.AddCookie(&http.Cookie{Name: CookieName(Authority(req)), Value: value})
		_, err := auth.Verify(req)
		return err
	}

	if err := verify(cookie.Value, "127.0.0.1:9000"); err != nil {
		t.Fatalf("刚签发的 cookie 必须通过：%v", err)
	}

	// 篡改 body。
	parts := strings.Split(cookie.Value, ".")
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte(`{"version":2,"authority":"127.0.0.1:9000","issuedAt":0,"expiresAt":1,"id":"peer-1"}`))
	if err := verify(strings.Join(parts, "."), "127.0.0.1:9000"); err == nil {
		t.Fatalf("篡改 body 必须被拒（否则任何人都能自己造 peer id）")
	}

	// authority 不同：cookie 名对不上，天然取不到。
	if err := verify(cookie.Value, "127.0.0.1:9001"); err == nil {
		t.Fatalf("换 authority 必须被拒")
	}

	// 过期。
	clock = clock.Add(2 * time.Minute)
	if err := verify(cookie.Value, "127.0.0.1:9000"); err == nil {
		t.Fatalf("过期 cookie 必须被拒")
	}
}

// TestFenceRejectsBadRequests 钉住 Host / Origin / sec-fetch-site 三档围栏。
func TestFenceRejectsBadRequests(t *testing.T) {
	auth, failure := NewAuth(AuthConfig{
		Secret:       make([]byte, 32),
		TrustedHosts: []string{"trusted.example"},
		BindHost:     "127.0.0.1:7777",
		Now:          func() time.Time { return time.Unix(1_700_000_000, 0) },
	})
	if failure != nil {
		t.Fatalf("NewAuth: %v", failure)
	}

	cases := []struct {
		name    string
		host    string
		headers map[string]string
		wantErr bool
	}{
		{"回环 IP", "127.0.0.1:9000", nil, false},
		{"localhost", "localhost:9000", nil, false},
		{"bind 字面量", "127.0.0.1:7777", nil, false},
		{"trustedHosts", "trusted.example", nil, false},
		{"外部 Host", "evil.example", nil, true},
		{"跨站标记", "127.0.0.1:9000", map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{"Origin 不同 authority", "127.0.0.1:9000", map[string]string{"Origin": "http://evil.example"}, true},
		{"Origin 相同 authority", "127.0.0.1:9000", map[string]string{"Origin": "http://127.0.0.1:9000"}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://"+testCase.host+"/connect", nil)
			request.Host = testCase.host
			for key, value := range testCase.headers {
				request.Header.Set(key, value)
			}
			err := auth.Fence(request)
			if testCase.wantErr && err == nil {
				t.Fatalf("%s 必须被围栏拒掉", testCase.name)
			}
			if !testCase.wantErr && err != nil {
				t.Fatalf("%s 不该被拒：%v", testCase.name, err)
			}
		})
	}
}

// TestForgedCookieCannotCallAPI 钉住"自己拼一份 cookie 调 unary"必须 401。
func TestForgedCookieCannotCallAPI(t *testing.T) {
	cfg := testServerConfig()
	_, ts, _ := startWire(t, cfg, unaryBindings())

	request, err := http.NewRequest(http.MethodPost, ts.URL+"/api/session/page",
		strings.NewReader(envelopeBody("r-forged", "session/page", `{}`)))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: CookieName(Authority(request)), Value: "v2.eyJ4IjoxfQ.forged"})

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("伪造 cookie 必须 401，实际 %d", response.StatusCode)
	}
}

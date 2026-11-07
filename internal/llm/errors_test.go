package llm

import "testing"

// 给客户端看的错误要短、可执行、不含内部细节。
// 原来它长这样：
//
//	请求模型失败(http://127.0.0.1:1/v1/chat/completions): Post "http://...": dial tcp ...
func TestGatewayErrorHidesInternals(t *testing.T) {
	e := &GatewayError{Kind: GatewayConnect, Base: "http://127.0.0.1:1/v1/"}
	msg := e.Error()
	if contains(msg, "/chat/completions") {
		t.Fatalf("不该带接口路径: %q", msg)
	}
	if contains(msg, "dial tcp") || contains(msg, "connectex") {
		t.Fatalf("不该带 Go 的原始网络错误: %q", msg)
	}
	if contains(msg, "v1/：") {
		t.Fatalf("尾斜杠要去掉: %q", msg)
	}
	if !contains(msg, "连不上") || !contains(msg, "--base") {
		t.Fatalf("要说清连不上并给出下一步: %q", msg)
	}
}

func TestGatewayErrorByStatus(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{401, "认证"},
		{403, "认证"},
		{404, "/v1"},
		{429, "限流"},
		{500, "网关出错"},
	}
	for _, c := range cases {
		msg := (&GatewayError{Kind: GatewayStatus, Code: c.code}).Error()
		if !contains(msg, c.want) {
			t.Errorf("%d 的消息应含 %q，实际 %q", c.code, c.want, msg)
		}
	}
}

// 其它 4xx 把网关正文带出来，但要截断——正文可能有几 KB。
func TestGatewayErrorTruncatesBody(t *testing.T) {
	long := ""
	for i := 0; i < 500; i++ {
		long += "x"
	}
	msg := (&GatewayError{Kind: GatewayStatus, Code: 400, Body: long}).Error()
	if len([]rune(msg)) > 300 {
		t.Fatalf("正文要截断，实际 %d 字符: %q", len([]rune(msg)), msg)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

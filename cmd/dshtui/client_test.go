package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// summarize 只为一件事服务：把一帧压成**人能看懂的一行**。
// 它故意不猜：不认识的结构就原样给 JSON，而不是编一句"收到一条消息"。
func TestSummarizePicksReadableField(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"text":"你好","type":"text"}`, "你好"},
		{`{"message":"说明"}`, "说明"},
		{`{"content":[{"text":"正文"}]}`, `{"content":[{"text":"正文"}]}`}, // 嵌套原样返回，不瞎猜
	}
	for _, c := range cases {
		got := summarize(json.RawMessage(c.in))
		if got != c.want {
			t.Fatalf("%s → %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 长正文必须截断：一帧几千字会把终端刷爆。
func TestSummarizeTruncates(t *testing.T) {
	long := strings.Repeat("字", 500)
	got := summarize(json.RawMessage(`{"text":"` + long + `"}`))
	if len([]rune(got)) > 61 {
		t.Fatalf("没截断，长度 %d", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatal("截断要有省略号，否则读者不知道后面还有")
	}
}

// newClient 对没 token 的地址要给得出话，而不是干等。
func TestNewClientRejectsDeadHost(t *testing.T) {
	if _, err := newClient("http://127.0.0.1:1", "x"); err == nil {
		t.Fatal("连不上时应当报错")
	}
}

// 重连退避：必须单调增长并封顶，否则断线时会把 CPU 打满。
func TestBackoffGrowsAndCaps(t *testing.T) {
	prev := time.Duration(0)
	for i := 0; i < 12; i++ {
		d := backoff(i)
		if d > 5*time.Second {
			t.Fatalf("第 %d 次退避 %s，超过 5s 上限", i, d)
		}
		if i > 0 && d < prev {
			t.Fatalf("退避变小了：%s → %s", prev, d)
		}
		prev = d
	}
	if backoff(0) < 100*time.Millisecond {
		t.Fatal("首次重连别太急")
	}
}

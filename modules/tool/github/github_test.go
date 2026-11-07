package github

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func run(t *testing.T, raw string) (string, string, error) {
	t.Helper()
	var tool Tool
	res, err := tool.Exec(context.Background(), json.RawMessage(raw))
	return res.Hint, res.Summary, err
}

// 写操作没 confirm：必须在发请求之前拒掉，而且说清怎么改。
func TestWriteRequiresConfirm(t *testing.T) {
	hint, _, err := run(t, `{"path":"repos/o/r/issues","method":"POST","body":"{\"title\":\"x\"}"}`)
	if err == nil {
		t.Fatal("POST 没有 confirm 必须被拒")
	}
	if !strings.Contains(hint, "confirm") {
		t.Fatalf("Hint 要说清缺 confirm: %q", hint)
	}
}

// 前导斜杠是被**归一**掉而不是拒掉（`/repos/o/r` 和 `repos/o/r` 等价）——
// 这比报错友好，但要有测试钉住，否则哪天改成报错就是破坏性变更。
func TestLeadingSlashNormalized(t *testing.T) {
	var tool Tool
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"path":"repos/octocat/Hello-World"}`))
	if err != nil && strings.Contains(res.Hint, "path") {
		t.Fatalf("正常 path 不该在参数校验阶段被拒: %v / %q", err, res.Hint)
	}
}

// 这些都在出网之前就该拦下——拦不住的会变成一次真实请求，慢且脏。
func TestInputGuards(t *testing.T) {
	cases := []struct{ raw, want string }{
		{`{"path":""}`, "path"},
		{`{"path":"repos/o/r","method":"BREW"}`, "method 只支持"},
		{`{"path":"repos/o/r","method":"GET","body":"{}"}`, "请求体"},
		{`{"path":"repos/o/r","method":"POST","body":"not json","confirm":true}`, "JSON"},
	}
	for _, c := range cases {
		hint, _, err := run(t, c.raw)
		if err == nil {
			t.Fatalf("%s 应当被拒", c.raw)
		}
		if !strings.Contains(hint, c.want) {
			t.Fatalf("%s 的 Hint 要提「%s」，实际 %q", c.raw, c.want, hint)
		}
	}
}

// 令牌绝不能出现在 Hint/Summary 里。
func TestTokenNeverLeaks(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_SECRET_SHOULD_NOT_APPEAR")
	hint, summary, _ := run(t, `{"path":"repos/o/r","method":"POST","body":"{}","confirm":true}`)
	for _, s := range []string{hint, summary} {
		if strings.Contains(s, "SECRET_SHOULD_NOT_APPEAR") {
			t.Fatalf("令牌泄漏到输出里: %q", s)
		}
	}
}

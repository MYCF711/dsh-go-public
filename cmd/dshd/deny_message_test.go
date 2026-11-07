package main

import "testing"

// "处理策略拒绝"和"处理用户拒绝"要说不一样的话。
// 对模型来说前者换问法没用（会白跑一轮），后者可以重新征求同意。
func TestDeniesSilently(t *testing.T) {
	cases := []struct {
		policy      string
		interactive bool
		want        bool
	}{
		{"deny", true, true},
		{"deny", false, true},
		{"auto", false, true},   // 非终端 = 没人可问
		{"auto", true, false},   // 终端里会问
		{"ask", false, false},   // ask 永远是真问
		{"allow", false, false}, // 放行，谈不上拒绝
	}
	for _, c := range cases {
		if got := deniesSilently(c.policy, c.interactive); got != c.want {
			t.Fatalf("deniesSilently(%q, %v) = %v，期望 %v", c.policy, c.interactive, got, c.want)
		}
	}
}

package main

import "testing"

func TestApprovalDecision(t *testing.T) {
	cases := []struct {
		name       string
		mode       string
		remembered bool
		allow      bool
		needAsk    bool
	}{
		{"模式 deny：连问都不问", "deny", false, false, false},
		{"模式 deny 压过记忆", "deny", true, false, false},
		{"模式 allow：不问直接放行", "allow", false, true, false},
		{"没策略 + 没记忆 → 问", "", false, false, true},
		{"没策略 + 有记忆 → 放行", "", true, true, false},
		{"ask 策略 + 没记忆 → 问", "ask", false, false, true},
		{"ask 策略 + 有记忆 → 放行", "ask", true, true, false},
	}
	for _, c := range cases {
		allow, needAsk := approvalDecision(c.mode, c.remembered)
		if allow != c.allow || needAsk != c.needAsk {
			t.Errorf("%s: got allow=%v ask=%v，期望 allow=%v ask=%v", c.name, allow, needAsk, c.allow, c.needAsk)
		}
	}
}

package main

import "testing"

// ACP 客户端只显示 title 的话，它必须自带命令——只写 "bash" 等于盲签。
func TestApprovalTitleLeadsWithCommand(t *testing.T) {
	got := approvalTitle("bash", `{"command":"rm -rf /tmp/build","description":"看看目录"}`)
	if got == "bash" || len(got) <= 6 {
		t.Fatalf("title 不能只是工具名: %q", got)
	}
	if !contains(got, "rm -rf /tmp/build") {
		t.Fatalf("title 要带命令: %q", got)
	}
	if contains(got, "看看目录") {
		t.Fatalf("title 不该混进模型的描述: %q", got)
	}
}

// 多行命令压成一行：title 只有一行的高度。
//
// 换行用 rune 拼出来，避免源码里出现转义歧义。
func TestApprovalTitleSingleLine(t *testing.T) {
	esc := string(rune(92)) + "n"
	args := "{" + string(rune(34)) + "command" + string(rune(34)) + ":" + string(rune(34)) +
		"echo a" + esc + "echo b" + esc + "rm -rf /" + string(rune(34)) + "}"
	got := approvalTitle("bash", args)
	for _, r := range got {
		if r == 10 || r == 13 {
			t.Fatalf("title 必须是单行: %q", got)
		}
	}
	if !contains(got, "rm -rf /") {
		t.Fatalf("压行之后命令内容要都在: %q", got)
	}
}

func TestApprovalKind(t *testing.T) {
	cases := map[string]string{"bash": "execute", "write": "edit", "edit": "edit", "grep": "other"}
	for tool, want := range cases {
		if got := approvalKind(tool); got != want {
			t.Errorf("%s 的 kind = %q，期望 %q", tool, got, want)
		}
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

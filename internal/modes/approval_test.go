package modes

import "testing"

// read-only 模式**必须**把审批倾向定成 deny。
//
// 这一条要是被改成 allow/空，read-only 就只剩"工具表里没列写类工具"这一层防线；
// 而工具表是会变的（用户自建模式、copy_of 继承）。审批倾向是第二道闸，不能悄悄消失。
func TestReadOnlyModeDeniesApprovals(t *testing.T) {
	var found *Mode
	for _, m := range Builtins() {
		if m.ID == "read-only" {
			mm := m
			found = &mm
		}
	}
	if found == nil {
		t.Fatal("内置模式里没有 read-only")
	}
	if found.Approval != "deny" {
		t.Fatalf("read-only 的审批倾向是 %q，必须是 deny", found.Approval)
	}
}

// 默认模式不能自己把审批定死成 allow——那等于绕过宿主的 --policy。
func TestDefaultModeDoesNotForceAllow(t *testing.T) {
	for _, m := range Builtins() {
		if m.ID == "default" && m.Approval == "allow" {
			t.Fatal("default 模式不该把审批定成 allow")
		}
	}
}

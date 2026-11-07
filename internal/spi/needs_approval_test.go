package spi

import "testing"

// NeedsApproval 是"要不要先问人"的**唯一**判定入口（审批、提示词分组、模式裁剪都走它）。
// 它少认一个字段，那一类工具就再也不问人了，而所有测试照样绿——
// 所以四个字段逐个钉，一个都不许漏。
func TestNeedsApproval(t *testing.T) {
	cases := []struct {
		name string
		caps Caps
		want bool
	}{
		{"全只读", Caps{}, false},
		{"会写盘", Caps{Writes: true}, true},
		{"会起进程", Caps{Executes: true}, true},
		{"会联网", Caps{Network: true}, true},
		{"可能不可恢复", Caps{Destructive: true}, true},
	}
	for _, c := range cases {
		if got := c.caps.NeedsApproval(); got != c.want {
			t.Fatalf("%s: NeedsApproval() = %v，期望 %v（%+v）", c.name, got, c.want, c.caps)
		}
	}
}

// 组合起来也必须是 true：任意一项危险就得问。
func TestNeedsApprovalAnyDangerousField(t *testing.T) {
	all := Caps{Writes: true, Executes: true, Network: true, Destructive: true}
	if !all.NeedsApproval() {
		t.Fatal("四个都置位时必须需要审批")
	}
}

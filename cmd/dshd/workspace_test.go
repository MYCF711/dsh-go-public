package main

import (
	"strings"
	"testing"
)

// 一个 dshd 进程只服务一个工作区。
//
// 工具的根是进程级的：两个会话要两个工作区时，后开的会顶掉先开的，
// 结果是 A 会话突然在 B 里干活——这种串台比"直接报错"难查一百倍。
func TestAdoptWorkspaceRefusesSecondOne(t *testing.T) {
	r := &acpRunner{}
	if err := r.adoptWorkspace("/tmp/a"); err != nil {
		t.Fatalf("第一次应当接受: %v", err)
	}
	if err := r.adoptWorkspace("/tmp/a"); err != nil {
		t.Fatalf("同一个工作区再来一次应当放行: %v", err)
	}
	err := r.adoptWorkspace("/tmp/b")
	if err == nil {
		t.Fatal("换工作区必须被拒绝")
	}
	if !strings.Contains(err.Error(), "另起一个 dshd") {
		t.Fatalf("错误里要说清怎么办: %v", err)
	}
}

// 大小写与尾斜杠的差别不算换工作区（Windows 上太常见）。
func TestSamePath(t *testing.T) {
	cases := [][3]any{
		{"D:/x/y", "D:/x/y/", true},
		{"D:/X/Y", "d:/x/y", true},
		{"D:/x/y", "D:/x/z", false},
	}
	for _, c := range cases {
		got := samePath(c[0].(string), c[1].(string))
		if got != c[2].(bool) {
			t.Fatalf("samePath(%q,%q)=%v，期望 %v", c[0], c[1], got, c[2])
		}
	}
}

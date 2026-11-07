package main

import (
	"strings"
	"testing"
)

// 终端层是唯一没有测试覆盖的一层：它坏了的症状是"界面花掉"，单测看不见。
// 这些断言把 ANSI 字节钉住——不是"看起来对"，是逐字节对。
func TestScreenEnterLeaveBytes(t *testing.T) {
	var b strings.Builder
	s := newScreenTo(&b, 20, 3)

	// 测试环境里 enableVT 可能为 false（不是真终端），Enter 会直接 os.Exit。
	// 所以这里只验 Leave 的幂等与字节，Enter 的字节由 Render 覆盖（同一份写出口）。
	s.inAlt = true
	s.Leave()
	first := b.String()
	if !strings.Contains(first, "\x1b[?25h") || !strings.Contains(first, "\x1b[?1049l") {
		t.Fatalf("Leave 要恢复光标并退出 alt-screen：%q", first)
	}
	if !s.inAlt == false {
		t.Fatal("Leave 之后 inAlt 必须为 false")
	}

	// 幂等：异常退出路径也会调 Leave，第二次不许再吐一遍转义序列。
	b.Reset()
	s.Leave()
	if b.Len() != 0 {
		t.Fatalf("Leave 必须幂等，第二次不该再写：%q", b.String())
	}
}

// Render 必须画满整屏：每一行都清行首、短行补空、超宽行裁到显示宽度。// 少画一行的症状是"上一帧的尾巴留在下面"，而那看起来像消息重复。
func TestScreenRenderFillsEveryRow(t *testing.T) {
	var b strings.Builder
	s := newScreenTo(&b, 10, 4)
	s.Render([]string{"短", strings.Repeat("x", 30), "", "第四行"})
	out := b.String()

	if !strings.HasPrefix(out, "\x1b[H") {
		t.Fatalf("Render 要从左上角开始：%q", out[:min(len(out), 20)])
	}
	// 4 行 = 4 次清行；每行都用光标定位（不再靠 \r\n 推进，差分要能跳到任意一行）
	if got := strings.Count(out, "\x1b[2K"); got != 4 {
		t.Fatalf("4 行屏幕要 4 次清行，得到 %d：%q", got, out)
	}
	if got := strings.Count(out, ";1H") + strings.Count(out, "\x1b[H"); got != 4 {
		t.Fatalf("4 行都要被定位到，得到 %d：%q", got, out)
	}
	// 超宽行必须被裁到 10 列，否则会折行、把整屏推下去
	for _, seg := range strings.Split(out, "\x1b[2K") {
		plain := seg
		if i := strings.Index(plain, "\x1b["); i >= 0 {
			plain = plain[:i]
		}
		if n := len([]rune(plain)); n > 10 {
			t.Fatalf("有一行没裁到 10 列（%d）：%q", n, plain)
		}
	}
}

// 差分重画：内容没变的行不该重画。
// 这条不是"省字节"，是"流式输出时别闪"——整屏重画在慢终端上看得出来。
func TestScreenSkipsUnchangedRows(t *testing.T) {
	var b strings.Builder
	s := newScreenTo(&b, 20, 3)
	s.Render([]string{"A", "B", "C"})
	first := b.String()
	b.Reset()
	s.Render([]string{"A", "B", "C"})
	second := b.String()
	if second == first {
		t.Fatal("内容完全没变时不该整屏重画（差分失效）")
	}
	if len(second) >= len(first) {
		t.Fatalf("没变的帧应该更短：第一帧 %d 字节，第二帧 %d 字节", len(first), len(second))
	}
}

// 进 alt-screen 时必须把窗口标题换成产品名：任务管理器/终端标题栏上
// 认得出"这是 DSH TUI GO"，而不是一个空的或不认识的名字。
func TestEnterSetsProductTitle(t *testing.T) {
	var b strings.Builder
	s := newScreenTo(&b, 20, 3)
	s.enterAlt()
	out := b.String()
	if !strings.Contains(out, oscTitle(productName)) {
		t.Fatalf("进入时要发标题序列 %q：%q", oscTitle(productName), out)
	}
	if !strings.HasPrefix(out, "\x1b[?1049h") {
		t.Fatalf("先进 alt-screen 再改标题：%q", out)
	}
	if !s.inAlt {
		t.Fatal("enterAlt 之后 inAlt 必须为 true（否则 Leave 不会恢复）")
	}
}

// 退出必须把原来的标题还回去——顺序上先退出 alt-screen，再改标题。
func TestLeaveRestoresTitle(t *testing.T) {
	var b strings.Builder
	s := newScreenTo(&b, 20, 3)
	s.enterAlt() // 真终端标题可能读得到也可能读不到，下面直接给定，不依赖环境
	b.Reset()
	s.prevTitle = "C:\\Windows\\System32\\cmd.exe"
	s.Leave()
	out := b.String()
	if !strings.Contains(out, "\x1b[?1049l") {
		t.Fatalf("Leave 要退出 alt-screen：%q", out)
	}
	if !strings.Contains(out, oscTitle("C:\\Windows\\System32\\cmd.exe")) {
		t.Fatalf("Leave 要把原标题还回去：%q", out)
	}
	if strings.Index(out, "\x1b[?1049l") > strings.Index(out, "\x1b]0;") {
		t.Fatalf("先退出 alt-screen，再恢复标题：%q", out)
	}
}

// 读不到原标題时不要发**空的** OSC：那会把人家终端的标题清成空白，
// 比"标题没恢复"更糟。这里用空 prevTitle 直接验那条分支。
func TestLeaveSkipsEmptyTitle(t *testing.T) {
	var b strings.Builder
	s := newScreenTo(&b, 20, 3)
	s.inAlt = true
	s.prevTitle = ""
	s.Leave()
	if got := b.String(); strings.Contains(got, "\x1b]0;") {
		t.Fatalf("空标题不该发 OSC：%q", got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

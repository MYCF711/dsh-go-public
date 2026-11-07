package main

// 显示列宽的契约。这是全屏渲染最容易错、也最难从屏幕上看出原因的一层：
// 宽了会被终端自己折行（整屏往下推一格），窄了会残留上一帧的尾巴。

import (
	"strings"
	"testing"
)

func TestCellWidth(t *testing.T) {
	cases := []struct {
		r    rune
		want int
		why  string
	}{
		{'A', 1, "ASCII"},
		{' ', 1, "空格"},
		{'█', 1, "块元素是 Neutral（不是全宽）"},
		{'▀', 1, "半块"},
		{'·', 1, "间隔号：Ambiguous，按 1 列"},
		{'…', 1, "省略号：Ambiguous，按 1 列"},
		{'❯', 1, "输入指针：等宽字体里是 1 列"},
		{'⚓', 1, "思考标记：同上"},
		{'探', 2, "中日韩统一表意文字"},
		{'あ', 2, "平假名"},
		{'！', 2, "全角标点"},
		{'Ａ', 2, "全角字母"},
		{'\u0301', 0, "组合用附加符号不占列"},
		{0, 0, "NUL"},
	}
	for _, c := range cases {
		if got := cellWidth(c.r); got != c.want {
			t.Fatalf("cellWidth(%q)=%d，应为 %d（%s）", c.r, got, c.want, c.why)
		}
	}
}

func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"", 0},
		{"DEEPSEEK", 8},
		{"探索未至之境！", 14}, // 6 个汉字 ×2 + 全角感叹号 ×2
		{"工具 0", 6},     // 工具=4 + 空格 + 0
		{"❯ hi", 4},
	}
	for _, c := range cases {
		if got := displayWidth(c.s); got != c.want {
			t.Fatalf("displayWidth(%q)=%d，应为 %d", c.s, got, c.want)
		}
	}
	// ANSI 转义序列不占列。
	if got := displayWidth("\x1b[38;2;1;2;3m中文\x1b[0m"); got != 4 {
		t.Fatalf("带转义的中文宽度应为 4，实际 %d", got)
	}
}

// clipWidth 的契约：结果**恰好** w 列（不足补空格，超了裁并补 `…`）。
func TestClipWidthIsExactlyNWide(t *testing.T) {
	for _, w := range []int{1, 2, 5, 10, 20} {
		for _, s := range []string{"", "abc", "中文字", "探索未至之境！", "a中b文c"} {
			got := clipWidth(s, w)
			if n := displayWidth(got); n != w {
				t.Fatalf("clipWidth(%q,%d) 宽 %d，应为 %d：%q", s, w, n, w, got)
			}
		}
	}
	// 超宽时必须出现省略号，且不许把宽字符切成半个。
	got := clipWidth("中文中文中文", 5)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("裁断处应有省略号：%q", got)
	}
	if n := displayWidth(got); n != 5 {
		t.Fatalf("裁断后应恰好 5 列，实际 %d：%q", n, got)
	}
	// 恰好放得下时**不许**加省略号。
	if got := clipWidth("abcde", 5); got != "abcde" {
		t.Fatalf("放得下时不该动它：%q", got)
	}
}

// 裁带颜色的行：转义序列整段照抄、不占列，裁完必须复位
// （不然最后一段的颜色会漏到右边的空白上）。
func TestClipWidthKeepsAnsiBalanced(t *testing.T) {
	styled := "\x1b[38;2;125;161;222m" + "中文abc" + "\x1b[0m"
	got := clipWidth(styled, 6)
	if n := displayWidth(got); n != 6 {
		t.Fatalf("裁完应恰好 6 列，实际 %d：%q", n, got)
	}
	if !strings.HasPrefix(got, "\x1b[38;2;125;161;222m") {
		t.Fatalf("开头的颜色不该被裁掉：%q", got)
	}
	if !strings.HasSuffix(got, "\x1b[0m") {
		t.Fatalf("裁过的上色行必须以复位结尾：%q", got)
	}

	// 放得下的带色行原样返回（只补空格）。
	if got := clipWidth(styled, 40); displayWidth(got) != 40 {
		t.Fatalf("补宽后应恰好 40 列：%q", got)
	}
}

// padRight 只补不裁。
func TestPadRightNeverTruncates(t *testing.T) {
	if got := padRight("中文", 6); displayWidth(got) != 6 {
		t.Fatalf("padRight 应补到 6 列：%q", got)
	}
	if got := padRight("中文字", 4); got != "中文字" {
		t.Fatalf("padRight 不该裁：%q", got)
	}
}

// fitWidth 是渲染层的入口，以前按 rune 数算（中文行会溢出）——这条钉住新语义。
func TestFitWidthCountsDisplayColumns(t *testing.T) {
	got := fitWidth("探索未至之境！", 14)
	if displayWidth(got) != 14 {
		t.Fatalf("14 列的中文应原样放下：%q", got)
	}
	got = fitWidth("探索未至之境！", 10)
	if displayWidth(got) != 10 {
		t.Fatalf("裁到 10 列，实际 %d：%q", displayWidth(got), got)
	}
	if n := len([]rune(got)); n > 10 {
		t.Fatalf("裁过之后 rune 数 %d 也不该超过列数 10", n)
	}
}

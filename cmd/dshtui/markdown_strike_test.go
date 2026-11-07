package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// 与 viva markdown.Strike 对齐：恰好两个波浪号才进入删除线；普通波浪、
// 长运行和未闭合分隔符都只是原文。正常路径强制启用 SGR 9，能力降级另测。
func enableStrikeForTest(t *testing.T) {
	t.Helper()
	t.Setenv("DSH_TUI_STRIKETHROUGH", "1")
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
}

func TestMarkdownStrikeDoubleTildeStyleAndText(t *testing.T) {
	enableStrikeForTest(t)
	got := inlineFormat("之前 ~~删除~~ 之后")
	want := "之前 \x0e删除\x0f 之后"
	if got != want {
		t.Fatalf("双波浪号没有变成内部 strike 样式：\n got=%q\nwant=%q", got, want)
	}
	if plain := plainMessageLine(got); plain != "之前 删除 之后" {
		t.Fatalf("纯文本应去掉配对分隔符且不吞正文，实际 %q", plain)
	}
	ansi := paint(got, true)
	if !strings.Contains(ansi, "\x1b[9m删除") {
		t.Fatalf("真屏缺少 SGR 9 删除线：%q", ansi)
	}
	if strings.ContainsAny(ansi, "\x0e\x0f") {
		t.Fatalf("内部 strike 标记漏进真屏：%q", ansi)
	}
}

func TestMarkdownStrikeNestedBoldCodeAndRawLink(t *testing.T) {
	enableStrikeForTest(t)
	for _, tc := range []struct {
		name string
		src  string
		want string
	}{
		{"strike in bold", "**粗 ~~删除~~**", stBold + "粗 \x0e删除\x0f" + stBoldEnd},
		{"bold in strike", "~~**粗**~~", "\x0e" + stBold + "粗" + stBoldEnd + "\x0f"},
		{"code tildes are text", "~~`a~~b`~~", "\x0e" + stAccent + "a~~b" + stAccentEnd + "\x0f"},
		{"strike in link label", "[~~标签~~](https://example.test/a~~b)", "[\x0e标签\x0f](https://example.test/a~~b)"},
		{"link inside strike keeps destination", "~~[标签](https://example.test/a~~b)~~", "\x0e[标签](https://example.test/a~~b)\x0f"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := inlineFormat(tc.src); got != tc.want {
				t.Fatalf("嵌套渲染错误：\n src=%q\n got=%q\nwant=%q", tc.src, got, tc.want)
			}
		})
	}
}

func TestMarkdownStrikeSingleUnclosedAndWindowsPathsStayLiteral(t *testing.T) {
	enableStrikeForTest(t)
	for _, src := range []string{
		"~单波浪~",
		"普通 a~b 与大约 ~ 50% 的波浪",
		"~~~三个波浪~~~",
		"~~~~",
		"~~",
		"~~未闭合",
		"未闭合~~",
		"~~ 两侧空白 ~~",
		`C:\Users\ADMINI~1\AppData\Local\Temp`,
		`C:\~\tools`,
		`C:\work\~~draft~~\file`,
		`~/work/~cache`,
	} {
		t.Run(src, func(t *testing.T) {
			got := inlineFormat(src)
			if plainMessageLine(got) != src {
				t.Fatalf("普通/未闭合波浪或 Windows 路径被改写：\n src=%q\n got=%q", src, plainMessageLine(got))
			}
			if strings.ContainsAny(got, "\x0e\x0f") {
				t.Fatalf("不应进入 strike 样式：%q", got)
			}
		})
	}
}

func TestMarkdownStrikeCJKEmojiWidthAndWrap(t *testing.T) {
	enableStrikeForTest(t)
	const (
		width = 8 // renderMarkdownLines 的最小排版宽度；必须折成两行。
		want  = "中文🙂é中文🙂"
	)
	lines := renderMarkdownLines("~~"+want+"~~", width)
	if len(lines) < 2 {
		t.Fatalf("测试必须真的跨行，实际只有 %d 行", len(lines))
	}
	var plain strings.Builder
	for i, ln := range lines {
		if got := dispWidth(ln.text); got > width {
			t.Fatalf("第 %d 行宽 %d，超过 %d：%q", i, got, width, plainMessageLine(ln.text))
		}
		if !strings.Contains(ln.text, "\x0e") || !strings.Contains(ln.text, "\x0f") {
			t.Fatalf("第 %d 行没有重开并收掉 strike：%q", i, ln.text)
		}
		plain.WriteString(plainMessageLine(ln.text))
		if ansi := paint(ln.text, true); strings.ContainsAny(ansi, "\x0e\x0f") {
			t.Fatalf("第 %d 行泄漏内部标记：%q", i, ansi)
		}
	}
	if got := plain.String(); got != want {
		t.Fatalf("CJK/emoji/组合符折行后正文改变：%q，应为 %q", got, want)
	}
	truncated := truncDisp("\x0e"+want+"\x0f", width)
	if dispWidth(truncated) > width || !strings.Contains(truncated, "\x0f") || !strings.HasSuffix(plainMessageLine(truncated), "…") {
		t.Fatalf("截断必须收掉 strike、保留省略号且不超宽：%q", truncated)
	}
}

func TestMarkdownStrikeUnsupportedTerminalUsesVisibleDimFallback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		force   string
		term    string
		noColor string
	}{
		{"explicit opt out", "0", "xterm-256color", ""},
		{"dumb terminal", "", "dumb", ""},
		{"text only NO_COLOR", "", "xterm-256color", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DSH_TUI_STRIKETHROUGH", tc.force)
			t.Setenv("TERM", tc.term)
			t.Setenv("NO_COLOR", tc.noColor)
			got := inlineFormat("~~删除~~")
			if plain := plainMessageLine(got); plain != "~~删除~~" {
				t.Fatalf("无 SGR 9 时必须保留可见分隔符与正文，实际 %q", plain)
			}
			ansi := paint(got, true)
			if !strings.Contains(ansi, "\x1b[2m") || !strings.Contains(ansi, "~~删除~~") {
				t.Fatalf("降级应是 dim + 可见 ~~删除~~，实际 %q", ansi)
			}
			if strings.Contains(ansi, "\x1b[9m") {
				t.Fatalf("声明无删除线能力时仍输出 SGR 9：%q", ansi)
			}
		})
	}
}

func TestMarkdownStrikeDumpTranscriptDeterministicAtBothSizes(t *testing.T) {
	enableStrikeForTest(t)
	path := filepath.Join("testdata", "markdown_strike_frames.json")
	for _, tc := range []struct {
		name  string
		size  string
		w, h  int
		color bool
	}{
		{"120x36/plain", "120x36", 120, 36, false},
		{"120x36/color", "120x36", 120, 36, true},
		{"80x24/plain", "80x24", 80, 24, false},
		{"80x24/color", "80x24", 80, 24, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"--dump-transcript", path, "--size", tc.size, "--expand", "--show-tool-results"}
			if tc.color {
				args = append(args, "--color")
			}
			var outA, outB, errA, errB strings.Builder
			if code := run(args, &outA, &errA); code != 0 {
				t.Fatalf("第一次转储退出码 %d：%s", code, errA.String())
			}
			if code := run(args, &outB, &errB); code != 0 {
				t.Fatalf("第二次转储退出码 %d：%s", code, errB.String())
			}
			if outA.String() != outB.String() || errA.String() != errB.String() {
				t.Fatal("同一帧文件、尺寸与能力下 dump/transcript 不确定")
			}
			if tc.color && !strings.Contains(outA.String(), "\x1b[9m") {
				t.Fatalf("彩色 transcript 没有 SGR 9：%q", outA.String())
			}
			if !tc.color && strings.Contains(outA.String(), "\x1b") {
				t.Fatalf("纯文本 transcript 泄漏 ESC：%q", outA.String())
			}
			plain := plainMessageLine(outA.String())
			for _, want := range []string{"删除线", "删除", "嵌套", "列表删除", "~~围栏原文~~", "保留思考块", "工具正文", "成功"} {
				if !strings.Contains(plain, want) {
					t.Fatalf("%s transcript 缺少 %q：\n%s", tc.size, want, plain)
				}
			}
			for _, bad := range []string{"~~删除~~", "~~嵌套~~", "~~列表删除~~"} {
				if strings.Contains(plain, bad) {
					t.Fatalf("%s 配对删除线分隔符漏进纯文本：%q", tc.size, bad)
				}
			}
			lines := strings.Split(strings.TrimSuffix(outA.String(), "\n"), "\n")
			if len(lines) != tc.h {
				t.Fatalf("%s 应有 %d 行，实际 %d", tc.size, tc.h, len(lines))
			}
			for i, ln := range lines {
				if got := displayWidth(ln); got > tc.w {
					t.Fatalf("%s 第 %d 行宽 %d，超过 %d：%q", tc.size, i, got, tc.w, ln)
				}
				if strings.ContainsAny(ln, "\x0e\x0f") {
					t.Fatalf("%s 第 %d 行泄漏内部 strike 标记：%q", tc.size, i, ln)
				}
			}
		})
	}
}

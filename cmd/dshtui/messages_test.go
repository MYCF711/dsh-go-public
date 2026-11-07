package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func plainMDLines(lines []mdLine) []string {
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = stripANSI(stripStyleMarks(ln.text))
	}
	return out
}

// 上游 formatToken：段落软换行仍是换行；strong 去掉标记，codespan 只给正文上色；
// 无序列表 marker 是 '-'；顶层围栏走 CodeBlockFrame，而不是伪造一个 copy 控件。
func TestMarkdownParagraphInlineListAndCodeFrame(t *testing.T) {
	src := "# 标题\n\n第一段 **加粗** 与 `code`\n第二行\n\n- 甲\n- 乙\n1. 一\n2. 二\n\n```go\nfmt.Println(\"你好\")\n```"
	lines := renderMarkdownLines(src, 60)
	plain := strings.Join(plainMDLines(lines), "\n")
	for _, want := range []string{
		"标题", "第一段 加粗 与 code\n第二行", "- 甲", "- 乙", "1. 一", "2. 二", "┌─ go", "│ fmt.Println(\"你好\")",
	} {
		if !strings.Contains(plain, want) {
			t.Fatalf("Markdown 缺 %q：\n%s", want, plain)
		}
	}
	for _, bad := range []string{"# 标题", "**", "`code`", "copy", "• 甲"} {
		if strings.Contains(plain, bad) {
			t.Fatalf("Markdown 不该留下/伪造 %q：\n%s", bad, plain)
		}
	}
}

// 折叠计数必须按折行后的屏幕行，而不是源码行。否则一条长行能绕过整个折叠阈值。
func TestCodeFrameFoldUsesRenderedRows(t *testing.T) {
	var body []string
	for i := 0; i < codeFoldRows+2; i++ {
		body = append(body, fmt.Sprintf("line-%02d", i))
	}
	lines := renderCodeFrameLines("go", strings.Join(body, "\n"), 40)
	if got := plainMDLines(lines)[0]; got != "┌─ go" {
		t.Fatalf("窄头部应只有角和语言，实际 %q", got)
	}
	rank := 0
	hint := 0
	for _, ln := range lines {
		if ln.meta.capLimit == codeFoldRows {
			if ln.meta.capRank != rank {
				t.Fatalf("第 %d 个显示行 capRank=%d", rank, ln.meta.capRank)
			}
			rank++
		}
		if ln.meta.capHint > 0 {
			hint = ln.meta.capHint
		}
	}
	if rank != codeFoldRows+2 || hint != 2 {
		t.Fatalf("代码应有 %d 个正文行并藏 2 行，实际 rows=%d hint=%d", codeFoldRows+2, rank, hint)
	}

	plain := plainFenceLines("go", strings.Repeat("x", 30), 10)
	rank = 0
	for _, ln := range plain {
		if ln.meta.capLimit == 0 {
			continue
		}
		if ln.meta.capRank != rank {
			t.Fatalf("窄围栏第 %d 个显示行 capRank=%d（长行被当成一个源码行）", rank, ln.meta.capRank)
		}
		rank++
	}
	if rank != 4 {
		t.Fatalf("30 个字符按 8 列应折成 4 行，实际 %d", rank)
	}
}

func TestCodeHeaderCannotOverflowOrInventCopy(t *testing.T) {
	for _, width := range []int{12, 24, 56, 80} {
		got := codeHeader(strings.Repeat("语言", 20), width)
		if n := dispWidth(got); n > width {
			t.Fatalf("语言框宽 %d，超过 %d：%q", n, width, got)
		}
		if strings.Contains(stripStyleMarks(got), "copy") {
			t.Fatalf("没有复制行为就不该画 copy：%q", got)
		}
	}
}

// 跨三行的 bold 必须在每行重开并收掉；只修第一行会让后两行掉色或把颜色漏到别的角色。
func TestWrapDispReopensAndClosesStyleOnEveryRow(t *testing.T) {
	lines := wrapDisp(stBold+"abcdefghij"+stBoldEnd, 3)
	if len(lines) != 4 {
		t.Fatalf("10 个字母按 3 列应有 4 行，实际 %d", len(lines))
	}
	for i, ln := range lines {
		if dispWidth(ln) > 3 {
			t.Fatalf("第 %d 行超宽：%q", i, ln)
		}
		if !strings.Contains(ln, stBold) || !strings.Contains(ln, stBoldEnd) {
			t.Fatalf("第 %d 行没有完整重开/收掉 bold：%q", i, ln)
		}
	}
}

func TestTruncDispClosesStyleAndCountsANSI(t *testing.T) {
	got := truncDisp("\x1b[31m"+stBold+"中abc"+stBoldEnd+"\x1b[0m", 4)
	if dispWidth(got) != 4 {
		t.Fatalf("截断应恰好 4 列，实际 %d：%q", dispWidth(got), got)
	}
	if !strings.Contains(got, stBoldEnd) {
		t.Fatalf("截断把 bold 的收尾丢了：%q", got)
	}
	if strings.Contains(stripANSI(got), "\x1b") {
		t.Fatalf("ANSI 被切成半截：%q", got)
	}
}

// 工具卡只消费真实字段：name/arguments、message.toolCallId/content/isError、显式 durationMs。
// 两个帧的墙钟差不是工具执行耗时，不许拿来冒充。
func TestToolCardRealShapeStatusResultAndDuration(t *testing.T) {
	a := newTestApp(100, 30)
	a.busy = true
	a.handleFrame(ServerFrame{Type: "tool/call", Time: 1000, Data: json.RawMessage(
		`{"callId":"c1","name":"bash","arguments":"{\"cmd\":\"printf 中文\"}"}`)})
	header := stripStyleMarks(a.lines[a.cards[0].hdr])
	if !strings.Contains(header, "运行中") || !strings.Contains(header, "printf 中文") {
		t.Fatalf("运行中的工具头缺真实状态/参数：%q", header)
	}

	a.handleFrame(ServerFrame{Type: "tool/result", Time: 9000, Data: json.RawMessage(
		`{"durationMs":1500,"message":{"toolCallId":"c1","isError":false,"content":[{"type":"text","text":"第一\n第二\n第三\n第四\n第五"}]}}`)})
	header = stripStyleMarks(a.lines[a.cards[0].hdr])
	if !strings.Contains(header, "成功") || !strings.Contains(header, "1.5s") {
		t.Fatalf("结果后工具头缺真实成功/耗时：%q", header)
	}
	if strings.Contains(header, "8.0s") {
		t.Fatalf("帧间隔被冒充成工具耗时：%q", header)
	}

	a.showToolResults = true
	visible := strings.Join(a.visibleLines(), "\n")
	for _, want := range []string{"第一", "第二", "第三", "+2 行"} {
		if !strings.Contains(visible, want) {
			t.Fatalf("展开工具结果但长内容仍折叠时缺 %q：\n%s", want, visible)
		}
	}
	if strings.Contains(visible, "第四") || strings.Contains(visible, "第五") {
		t.Fatalf("第 4/5 行应先折叠：\n%s", visible)
	}
	a.expandAll = true
	visible = strings.Join(a.visibleLines(), "\n")
	if !strings.Contains(visible, "第四") || !strings.Contains(visible, "第五") || strings.Contains(visible, "+2 行") {
		t.Fatalf("Ctrl+O 后应显示第 4/5 行并撤掉折叠提示：\n%s", visible)
	}
}

// 上游 capLines：只多一行时直接展示，避免为了藏一行再多画一行提示。
func TestToolBodyShowsOneExtraLineWithoutFold(t *testing.T) {
	a := newTestApp(100, 20)
	a.showToolResults = true
	a.handleFrame(ServerFrame{Type: "tool/result", Data: json.RawMessage(
		`{"summary":"一\n二\n三\n四"}`)})
	visible := strings.Join(a.visibleLines(), "\n")
	if !strings.Contains(visible, "四") || strings.Contains(visible, "+1 行") {
		t.Fatalf("只多一行应直接显示：\n%s", visible)
	}
}

func TestToolBodyFoldCountsWrappedRows(t *testing.T) {
	c := &toolCard{status: toolOK, body: []mdLine{{text: strings.Repeat("中", 40)}}}
	lines := renderToolBody(c, 12) // 净宽 9，每个汉字 2 列 → 10 个显示行。
	rank := 0
	hint := 0
	for _, ln := range lines {
		if ln.meta.capLimit == toolBodyRows {
			if ln.meta.capRank != rank {
				t.Fatalf("第 %d 个折行 capRank=%d", rank, ln.meta.capRank)
			}
			rank++
		}
		if ln.meta.capHint > 0 {
			hint = ln.meta.capHint
		}
	}
	if rank != 10 || hint != 7 {
		t.Fatalf("40 个汉字应折成 10 行、藏 7 行，实际 rows=%d hint=%d", rank, hint)
	}
}

func TestToolHeaderKeepsStatusWithLongArguments(t *testing.T) {
	c := &toolCard{name: "bash", title: strings.Repeat("中文", 40), status: toolRunning}
	got := renderToolHeader(c, 40)
	if dispWidth(got) > 40 || !strings.Contains(stripStyleMarks(got), "运行中") {
		t.Fatalf("长参数不能把真实状态挤出头部：%q", stripStyleMarks(got))
	}
}

func TestToolCardDoesNotInventDurationFromFrameTimes(t *testing.T) {
	a := newTestApp(100, 20)
	a.busy = true
	a.handleFrame(ServerFrame{Type: "tool/call", Time: 1000, Data: json.RawMessage(`{"callId":"c","name":"read"}`)})
	a.handleFrame(ServerFrame{Type: "tool/result", Time: 9000, Data: json.RawMessage(
		`{"message":{"toolCallId":"c","content":[{"type":"text","text":"ok"}]}}`)})
	header := stripStyleMarks(a.lines[a.cards[0].hdr])
	if strings.Contains(header, "8.0s") || strings.Contains(header, "· 0.") {
		t.Fatalf("没有 durationMs 就不该编耗时：%q", header)
	}
}

func TestToolErrorNeverGetsFalseFoldHint(t *testing.T) {
	a := newTestApp(100, 24)
	a.handleFrame(ServerFrame{Type: "tool/call", Data: json.RawMessage(`{"callId":"e","name":"bash"}`)})
	a.handleFrame(ServerFrame{Type: "tool/result", Data: json.RawMessage(
		`{"message":{"toolCallId":"e","isError":true,"content":[{"type":"text","text":"一\n二\n三\n四\n五"}]}}`)})
	visible := strings.Join(a.visibleLines(), "\n")
	for _, want := range []string{"一", "二", "三", "四", "五"} {
		if !strings.Contains(visible, want) {
			t.Fatalf("错误正文 %q 被折叠：\n%s", want, visible)
		}
	}
	if strings.Contains(visible, "+2 行") {
		t.Fatalf("错误已全部显示，不能再谎称还藏了 2 行：\n%s", visible)
	}
}

func TestReasoningOnlySettlesWithoutInventedDuration(t *testing.T) {
	a := newTestApp(100, 20)
	a.busy = true
	a.handleFrame(ServerFrame{Type: "assistant/message", Time: 1000, Data: json.RawMessage(
		`{"reasoning":"只想了一句\n还有一句"}`)})
	folded := strings.Join(a.view(), "\n")
	if !strings.Contains(folded, "⚓ 思考") || !strings.Contains(folded, "Ctrl+O 展开") {
		t.Fatalf("reasoning-only 完整消息应立即落成折叠行：\n%s", folded)
	}
	if strings.Contains(folded, "思考 ·") {
		t.Fatalf("单帧没有时间跨度/显式 durationMs，不该编耗时：\n%s", folded)
	}
	if strings.Contains(folded, "只想了一句") {
		t.Fatalf("默认应折叠思考正文：\n%s", folded)
	}
	a.handleRune(15)
	expanded := strings.Join(a.view(), "\n")
	if !strings.Contains(expanded, "只想了一句") || !strings.Contains(expanded, "还有一句") {
		t.Fatalf("Ctrl+O 应展开完整思考：\n%s", expanded)
	}
}

// 转储读的是调用方给的 Transport 帧文件，不在程序里编一段“真实会话”。
func TestRunDumpTranscriptSizesAndWidth(t *testing.T) {
	path := filepath.Join("testdata", "message_frames.json")
	for _, tc := range []struct {
		size string
		w, h int
	}{{"120x36", 120, 36}, {"80x24", 80, 24}} {
		var out, errb strings.Builder
		code := run([]string{"--dump-transcript", path, "--size", tc.size}, &out, &errb)
		if code != 0 {
			t.Fatalf("%s 转储退出码 %d：%s", tc.size, code, errb.String())
		}
		if strings.Contains(out.String(), "\x1b") {
			t.Fatalf("无颜色转储含 ANSI/ESC：%q", out.String())
		}
		lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
		if len(lines) != tc.h {
			t.Fatalf("%s 应有 %d 行，实际 %d", tc.size, tc.h, len(lines))
		}
		for i, ln := range lines {
			if n := displayWidth(ln); n > tc.w {
				t.Fatalf("%s 第 %d 行宽 %d，超过 %d：%q", tc.size, i, n, tc.w, ln)
			}
		}
		if tc.size == "120x36" {
			for _, want := range []string{"#", "┌─ go", "bash", "成功", "⚓ 思考"} {
				if want == "#" {
					continue // 标题渲染后不该留下 #；这里只让检查表表达完整目标。
				}
				if !strings.Contains(out.String(), want) {
					t.Fatalf("120x36 转储缺 %q：\n%s", want, out.String())
				}
			}
		}
	}
}

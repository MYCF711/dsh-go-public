package main

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func editorType(a *app, text string) {
	for _, r := range text {
		a.handleRune(r)
	}
}

func editorRow(a *app) styledLine {
	return a.inputRow(currentTheme())
}

func editorReverseSpans(row styledLine) []string {
	var out []string
	for _, s := range row {
		if s.reverse {
			out = append(out, s.text)
		}
	}
	return out
}

func editorSpecials(a *app, raw string) []string {
	runes := make(chan rune, 128)
	specials := make(chan string, 128)
	readKeysFrom(strings.NewReader(raw), runes, specials)
	close(runes)
	close(specials)
	for r := range runes {
		a.handleRune(r)
	}
	var names []string
	for k := range specials {
		names = append(names, k)
		a.handleSpecial(k)
	}
	return names
}

func TestInputInsertDeleteAtCaret(t *testing.T) {
	a := newTestApp(80, 24)
	editorType(a, "ac")
	a.handleSpecial("left")
	editorType(a, "b")
	if got := string(a.input); got != "abc" {
		t.Fatalf("左移后插入 = %q，want abc", got)
	}
	a.handleRune(127)
	if got := string(a.input); got != "ac" {
		t.Fatalf("行中 Backspace = %q，want ac", got)
	}
	a.handleSpecial("delete")
	if got := string(a.input); got != "a" {
		t.Fatalf("行中 Delete = %q，want a", got)
	}
	a.handleSpecial("end")
	editorType(a, "z")
	a.handleSpecial("home")
	editorType(a, "q")
	if got := string(a.input); got != "qaz" {
		t.Fatalf("Home/End 后插入 = %q，want qaz", got)
	}
}

func TestInputHomeEndAndWordJumps(t *testing.T) {
	a := newTestApp(80, 24)
	a.input = []rune("one two  三😀 four")
	a.handleSpecial("ctrl+left")
	editorType(a, "X")
	if got := string(a.input); got != "one two  三😀 Xfour" {
		t.Fatalf("Ctrl+Left 没有到上一词首：%q", got)
	}
	a.handleSpecial("home")
	a.handleSpecial("ctrl+right")
	editorType(a, "Y")
	if got := string(a.input); got != "one Ytwo  三😀 Xfour" {
		t.Fatalf("Ctrl+Right 没有到下一词首：%q", got)
	}
	a.handleSpecial("end")
	editorType(a, "!")
	if got := string(a.input); got != "one Ytwo  三😀 Xfour!" {
		t.Fatalf("End 没有回到草稿末尾：%q", got)
	}
}

func TestInputGraphemeEditing(t *testing.T) {
	const family = "👨‍👩‍👧‍👦"
	a := newTestApp(80, 24)
	editorType(a, "Ae\u0301"+family+"B")
	a.handleSpecial("left")
	a.handleRune(127)
	if got := string(a.input); got != "Ae\u0301B" {
		t.Fatalf("Backspace 删除了 emoji 的一部分而不是整个 grapheme：%q", got)
	}
	a.handleRune(127)
	if got := string(a.input); got != "AB" {
		t.Fatalf("Backspace 没有一次删除 e+组合符：%q", got)
	}
	a.handleSpecial("delete")
	if got := string(a.input); got != "A" {
		t.Fatalf("Delete 在行中删除错误：%q", got)
	}

	b := newTestApp(80, 24)
	b.input = []rune("X" + family + "Y")
	b.handleSpecial("home")
	b.handleSpecial("right")
	b.handleSpecial("delete")
	if got := string(b.input); got != "XY" {
		t.Fatalf("Delete 没有一次删除完整 emoji ZWJ 序列：%q", got)
	}
}

func TestInputHorizontalViewportKeepsCaretVisible(t *testing.T) {
	a := newTestApp(12, 24)
	a.input = []rune(strings.Repeat("0123456789", 4))
	if got, want := editorRow(a).plain(), "❯ 123456789 "; got != want {
		t.Fatalf("末尾水平视口 = %q，want %q", got, want)
	}
	if got := editorReverseSpans(editorRow(a)); !reflect.DeepEqual(got, []string{" "}) {
		t.Fatalf("末尾应显示反显空格光标，实际 %q", got)
	}

	a.handleSpecial("home")
	if got, want := editorRow(a).plain(), "❯ 0123456789"; got != want {
		t.Fatalf("Home 水平视口 = %q，want %q", got, want)
	}
	if got := editorReverseSpans(editorRow(a)); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("Home 光标应反显首字符，实际 %q", got)
	}

	a.handleSpecial("end")
	for i := 0; i < 25; i++ {
		a.handleSpecial("left")
	}
	if got, want := editorRow(a).plain(), "❯ 5678901234"; got != want {
		t.Fatalf("连续左移后的水平视口 = %q，want %q", got, want)
	}
	if got := editorReverseSpans(editorRow(a)); !reflect.DeepEqual(got, []string{"5"}) {
		t.Fatalf("左滚后光标字符不可见：%q", got)
	}
	a.handleSpecial("right")
	if got := editorReverseSpans(editorRow(a)); !reflect.DeepEqual(got, []string{"6"}) {
		t.Fatalf("右移后光标字符不可见：%q", got)
	}
}

func TestInputViewportCJKAndEmojiColumns(t *testing.T) {
	a := newTestApp(10, 24)
	a.input = []rune("一二三四五六😀七八")
	a.handleSpecial("home")
	if got, want := editorRow(a).plain(), "❯ 一二三四"; got != want {
		t.Fatalf("CJK 首端视口 = %q，want %q", got, want)
	}
	if got := editorReverseSpans(editorRow(a)); !reflect.DeepEqual(got, []string{"一"}) {
		t.Fatalf("CJK 首字符应完整反显，实际 %q", got)
	}
	a.handleSpecial("end")
	if got, want := editorRow(a).plain(), "❯ 😀七八 "; got != want {
		t.Fatalf("CJK/emoji 末尾视口 = %q，want %q", got, want)
	}
	if got := dispWidth(editorRow(a).plain()); got > 10 {
		t.Fatalf("输入行按显示列超宽：%d > 10", got)
	}
}

func TestInputViewportKeepsEmojiClusterWhole(t *testing.T) {
	const family = "👨‍👩‍👧‍👦"
	a := newTestApp(12, 24)
	a.input = []rune("开始" + family + "结束")
	a.handleSpecial("home")
	a.handleSpecial("right")
	a.handleSpecial("right")
	row := editorRow(a)
	if got := editorReverseSpans(row); !reflect.DeepEqual(got, []string{family}) {
		t.Fatalf("光标落在 ZWJ emoji 内部或没有完整反显：%q", got)
	}
	if !strings.Contains(row.plain(), family) || strings.Contains(row.plain(), "…") {
		t.Fatalf("emoji 被水平视口切坏：%q", row.plain())
	}

	var out bytes.Buffer
	a.scr = newScreenTo(&out, 12, 4)
	lines := []string{"", "", "", row.render(true)}
	a.scr.Render(lines)
	if !strings.Contains(out.String(), family) || strings.Contains(out.String(), "…") {
		t.Fatalf("Screen.Render 按 rune 而不是 grapheme 列宽切坏 emoji：%q", out.String())
	}
}

func TestInputCtrlHomeEndStillScrollTranscript(t *testing.T) {
	a := newTestApp(80, 12)
	fillLines(a, 60)
	a.input = []rune("draft")
	a.handleSpecial("pgup")
	a.handleSpecial("ctrl+home")
	if a.scroll != a.maxScroll() {
		t.Fatalf("Ctrl+Home 应到正文顶部：scroll=%d max=%d", a.scroll, a.maxScroll())
	}
	scroll := a.scroll
	a.handleSpecial("home")
	editorType(a, "X")
	if got := string(a.input); got != "Xdraft" {
		t.Fatalf("Home 应只移动输入光标：%q", got)
	}
	if a.scroll != scroll {
		t.Fatalf("Home 不应滚动正文：%d -> %d", scroll, a.scroll)
	}
	a.handleSpecial("ctrl+end")
	if a.scroll != 0 {
		t.Fatalf("Ctrl+End 应回正文底部：%d", a.scroll)
	}
	a.handleSpecial("end")
	editorType(a, "Y")
	if got := string(a.input); got != "XdraftY" {
		t.Fatalf("End 应只移动输入光标：%q", got)
	}
	if a.scroll != 0 {
		t.Fatalf("End 不应滚动正文：%d", a.scroll)
	}
}

func TestFunctionKeysDecodeWithoutFabricatedActions(t *testing.T) {
	a := newTestApp(80, 24)
	a.input = []rune("draft")
	a.handleSpecial("left")
	fillLines(a, 40)
	a.handleSpecial("pgup")
	beforeInput := string(a.input)
	beforeScroll := a.scroll
	beforeView := a.view()
	names := editorSpecials(a, "\x1b[15~\x1b[17~\x1b[18~\x1b[24~")
	if got := strings.Join(names, ","); got != "f5,f6,f7,f12" {
		t.Fatalf("功能键解码 = %q，want f5,f6,f7,f12", got)
	}
	if string(a.input) != beforeInput || a.scroll != beforeScroll || !reflect.DeepEqual(a.view(), beforeView) {
		t.Fatalf("上游未绑定动作的 F5/F6/F7/F12 伪造了界面效果")
	}
	for _, name := range []string{"f5", "f6", "f7", "f12"} {
		if !knownKey(name) {
			t.Errorf("script 不认识 %s", name)
		}
	}
}

func TestPromptFailureRetryRestoresCaretAndAcceptedResets(t *testing.T) {
	a := newTestApp(80, 24)
	calls := 0
	a.sendPrompt = func(string, string) (promptAck, error) {
		calls++
		if calls == 1 {
			return promptAck{}, errors.New("boom")
		}
		return promptAck{Accepted: true, TurnID: "turn-editor"}, nil
	}
	a.input = []rune("abcdef")
	a.handleSpecial("left")
	a.handleSpecial("left")
	a.submit()
	settlePrompt(t, a)
	editorType(a, "X")
	if got := string(a.input); got != "abcdXef" {
		t.Fatalf("失败后草稿/光标被重置：%q", got)
	}
	a.submit()
	settlePrompt(t, a)
	editorType(a, "Z")
	if got := string(a.input); got != "Z" {
		t.Fatalf("accepted 后草稿或光标没有复位：%q", got)
	}
}

func TestPromptPendingSpecialKeysReadOnly(t *testing.T) {
	a := newTestApp(80, 24)
	a.input = []rune("abcdef")
	a.handleSpecial("left")
	a.handleSpecial("left")
	a.pendingSend = &pendingPrompt{id: 1, sid: a.sid, text: "abcdef"}
	for _, k := range []string{"home", "end", "left", "right", "ctrl+left", "ctrl+right", "delete"} {
		a.handleSpecial(k)
	}
	editorType(a, "X")
	a.handleRune(127)
	a.handlePromptResult(promptOutcome{id: 1, sid: a.sid, text: "abcdef", err: errors.New("boom")})
	editorType(a, "X")
	if got := string(a.input); got != "abcdXef" {
		t.Fatalf("pending 特殊键改动了草稿或光标：%q", got)
	}
}

func TestSearchUsesSameCaretEditor(t *testing.T) {
	a := newTestApp(80, 24)
	a.push("abc 在正文中")
	a.enterSearch()
	editorType(a, "ac")
	a.handleSpecial("left")
	editorType(a, "b")
	if got := string(a.input); got != "abc" || a.searchQuery != "abc" {
		t.Fatalf("搜索行中插入 = %q query=%q", got, a.searchQuery)
	}
	a.handleRune(127)
	if got := string(a.input); got != "ac" {
		t.Fatalf("搜索行中 Backspace = %q", got)
	}
	a.handleSpecial("delete")
	if got := string(a.input); got != "a" || a.searchQuery != "a" {
		t.Fatalf("搜索行中 Delete = %q query=%q", got, a.searchQuery)
	}
}

func TestLateOSCAndEditorKeysShareDecoder(t *testing.T) {
	a := newTestApp(80, 24)
	names := editorSpecials(a, "ab\x1b]11;rgb:1/2/3\x07\x1b[D")
	if !reflect.DeepEqual(names, []string{"left"}) {
		t.Fatalf("迟到 OSC 后按键解码 = %v", names)
	}
	editorType(a, "X")
	if got := string(a.input); got != "aXb" {
		t.Fatalf("迟到 OSC 泄漏进草稿或左移失效：%q", got)
	}
}

func TestReadKeysMapsEditorSequences(t *testing.T) {
	a := newTestApp(80, 24)
	names := editorSpecials(a, "\x1b[D\x1bOC\x1b[1;5D\x1b[1;3C\x1b[H\x1bOF\x1b[3~\x1b[[E\x1b[1;5H\x1b[4;5~")
	want := []string{"left", "right", "ctrl+left", "alt+right", "home", "end", "delete", "f5", "ctrl+home", "ctrl+end"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("编辑器序列解码 = %v，want %v", names, want)
	}
	if got := string(a.input); got != "" {
		t.Fatalf("编辑器转义序列漏进草稿：%q", got)
	}
}

func TestEditorScriptKeysRegistered(t *testing.T) {
	for _, name := range []string{
		"left", "right", "ctrl+left", "ctrl+right", "home", "end",
		"ctrl+home", "ctrl+end", "delete", "backspace", "f5", "f6", "f7", "f12",
	} {
		if !knownKey(name) {
			t.Errorf("script 没有接真实按键 %s", name)
		}
	}
	plan, err := readPlan(writePlan(t, `{"steps":[
		{"kind":"key","key":"left"},{"kind":"key","key":"ctrl+right"},
		{"kind":"key","key":"backspace"},{"kind":"key","key":"delete"},
		{"kind":"key","key":"f5"},{"kind":"key","key":"f6"},
		{"kind":"key","key":"f7"},{"kind":"key","key":"f12"},
		{"kind":"snapshot","label":"editor-keys"}]}`))
	if err != nil {
		t.Fatalf("真实 script 计划拒绝编辑器按键：%v", err)
	}
	if len(plan.Steps) != 9 {
		t.Fatalf("script 编辑器步数 = %d，want 9", len(plan.Steps))
	}
}

func TestUnknownModifiedKeysStayInert(t *testing.T) {
	for _, seq := range []string{"1;2D", "1;2C", "3;5~", "99~"} {
		if got := keyName([]byte(seq)); got != "" {
			t.Errorf("未实现序列 %q 被伪造成 %q", seq, got)
		}
	}
}

func TestEditorDumpDeterministicAtTerminalSizes(t *testing.T) {
	for _, size := range [][2]int{{120, 36}, {80, 24}, {12, 6}, {4, 2}, {2, 1}, {1, 1}} {
		a := newTestApp(size[0], size[1])
		a.input = []rune(strings.Repeat("甲😀乙丙", 12))
		a.handleSpecial("home")
		for i := 0; i < 7; i++ {
			a.handleSpecial("right")
		}
		first := a.view()
		second := a.view()
		if !reflect.DeepEqual(first, second) {
			t.Errorf("%dx%d dump 不确定", size[0], size[1])
			continue
		}
		if len(first) != size[1] {
			t.Errorf("%dx%d dump 行数 = %d", size[0], size[1], len(first))
			continue
		}
		for i, line := range first {
			if got := dispWidth(line); got > size[0] {
				t.Errorf("%dx%d 第 %d 行宽 %d：%q", size[0], size[1], i, got, line)
			}
		}
		if got := editorReverseSpans(editorRow(a)); len(got) == 0 {
			t.Errorf("%dx%d 没有可见输入光标", size[0], size[1])
		}
	}
}

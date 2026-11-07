package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestOSC11QueryAndSetBytes(t *testing.T) {
	if got, want := osc11Query, "\x1b]11;?\x07"; got != want {
		t.Fatalf("OSC 11 查询字节=%q，应为 %q", got, want)
	}
	if got, want := da1Query, "\x1b[c"; got != want {
		t.Fatalf("DA1 哨兵字节=%q，应为 %q", got, want)
	}
	got, err := osc11Set("rgb:ff/00/Aa")
	if err != nil {
		t.Fatalf("合法设置色被拒：%v", err)
	}
	if want := "\x1b]11;rgb:ff/00/Aa\x07"; got != want {
		t.Fatalf("OSC 11 设置字节=%q，应为 %q", got, want)
	}
	for _, spec := range []string{"", "?", "rgb:0/0", "rgb:gg/00/00", "#fff", " rgb:00/00/00"} {
		if got, err := osc11Set(spec); err == nil || got != "" {
			t.Fatalf("非法设置 %q 应报错且不得产生控制码，got=%q err=%v", spec, got, err)
		}
	}
}

func TestParseOSCColorScalesChannels(t *testing.T) {
	for _, tc := range []struct {
		spec    string
		r, g, b uint8
	}{
		{"rgb:f/0/A", 255, 0, 170},
		{"rgb:FF/80/0a", 255, 128, 10},
		{"RgB:fF/a0/00", 255, 160, 0},
		{"rgb:1/23/45", 17, 35, 69},
		{"rgb:ffff/0000/8080", 255, 0, 128},
		{"#AaBbCc", 170, 187, 204},
	} {
		got, ok := parseOSCColor(tc.spec)
		if !ok {
			t.Fatalf("%q 应解析成功", tc.spec)
		}
		if got.r != tc.r || got.g != tc.g || got.b != tc.b || !got.set {
			t.Fatalf("%q 解析为 %+v，应为 rgb(%d,%d,%d) 且 set", tc.spec, got, tc.r, tc.g, tc.b)
		}
	}
	for _, spec := range []string{
		"", "?", " rgb:0/0/0", "rgb:0/0", "rgb:/0/0", "rgb:00000/0/0",
		"rgb:gg/0/0", "rgb:0/0/0\n", "rgba:0/0/0", "#fff", "#GG0000",
	} {
		if got, ok := parseOSCColor(spec); ok {
			t.Fatalf("非法颜色 %q 不应解析为 %+v", spec, got)
		}
	}
}

func TestParseOSC11ResponseTerminatorsAndValidation(t *testing.T) {
	for _, raw := range []string{
		"\x1b]11;rgb:ff/80/00\x07",
		"\x1b]11;rgb:FF/80/00\x1b\\",
	} {
		got, ok := parseOSC11Response(raw)
		if !ok || got.r != 255 || got.g != 128 || got.b != 0 {
			t.Fatalf("标准响应 %q 解析失败：%+v ok=%v", raw, got, ok)
		}
	}
	for _, raw := range []string{
		"11;rgb:ff/80/00\x07",            // 缺 OSC introducer
		"\x1b]10;rgb:ff/80/00\x07",       // 前景不是背景
		"\x1b]11;?\x07",                  // 查询不是颜色响应
		"\x1b]11;rgb:ff/80/00",           // 未终止
		"\x1b]11;rgb:ff/80/00\x1b",       // 半个 ST
		"\x1b]11;rgb:ff/80/00\x07junk",   // 终止后还有垃圾
		"\x1b]11;rgb:zz/80/00\x07",       // 非法 hex
		"\x1b]11;rgb:ff/80/00\x1b\\junk", // ST 后还有垃圾
	} {
		if got, ok := parseOSC11Response(raw); ok {
			t.Fatalf("非法响应 %q 不应解析为 %+v", raw, got)
		}
	}
}

func TestThemeForBackgroundThreshold(t *testing.T) {
	for _, tc := range []struct {
		c    color
		want string
	}{
		{color{r: 0, g: 0, b: 0}, "dark"},
		{color{r: 255, g: 255, b: 255}, "light"},
		{color{r: 140, g: 140, b: 140}, "dark"}, // 上游是严格 > 140，不是 >=。
		{color{r: 141, g: 141, b: 141}, "light"},
		{color{r: 0, g: 238, b: 0}, "dark"},  // Rec.601 = 139.766
		{color{r: 0, g: 239, b: 0}, "light"}, // Rec.601 = 140.353
	} {
		if got := themeForBackground(tc.c); got != tc.want {
			t.Fatalf("背景 %+v 应选 %s，实际 %s", tc.c, tc.want, got)
		}
	}
}

func TestProbeOSC11NonTTYEmitsNothing(t *testing.T) {
	src := &readerSource{in: strings.NewReader("\x1b]11;rgb:ff/ff/ff\x07")}
	var out strings.Builder
	got := probeOSC11(src, &out, false, 20*time.Millisecond)
	if got.status != oscProbeNonTTY {
		t.Fatalf("非 TTY 状态=%q，应为 %q", got.status, oscProbeNonTTY)
	}
	if out.Len() != 0 || len(got.replay) != 0 {
		t.Fatalf("非 TTY 不得发控制码或读输入：out=%q replay=%q", out.String(), got.replay)
	}
}

func TestProbeOSC11ResponseAndOrdinaryInputReplay(t *testing.T) {
	src := &readerSource{in: strings.NewReader("x\x1b]11;rgb:ff/ff/ff\x1b\\y")}
	var out strings.Builder
	got := probeOSC11(src, &out, true, time.Second)
	if got.status != oscProbeOK || got.color.r != 255 || got.color.g != 255 || got.color.b != 255 {
		t.Fatalf("查询结果=%+v", got)
	}
	if want := osc11Query + da1Query; out.String() != want {
		t.Fatalf("启动查询=%q，应为 OSC11+DA1 %q", out.String(), want)
	}
	runes := make(chan rune, 16)
	specials := make(chan string, 4)
	decodeKeys(&prefixSource{prefix: got.replay, src: src}, runes, specials)
	if typed := drainRunes(runes); typed != "xy" {
		t.Fatalf("响应前后的普通按键必须保留，decoded=%q replay=%q", typed, got.replay)
	}
}

func TestProbeOSC11DA1MeansUnsupported(t *testing.T) {
	src := &readerSource{in: strings.NewReader("a\x1b[?1;2cb")}
	var out strings.Builder
	got := probeOSC11(src, &out, true, time.Second)
	if got.status != oscProbeUnsupported {
		t.Fatalf("DA1 先于 OSC11 到达应明确判为不支持，got=%+v", got)
	}
	runes := make(chan rune, 16)
	specials := make(chan string, 4)
	decodeKeys(&prefixSource{prefix: got.replay, src: src}, runes, specials)
	if typed := drainRunes(runes); typed != "ab" {
		t.Fatalf("DA1 哨兵不得进入普通输入，其他按键应保留：decoded=%q replay=%q", typed, got.replay)
	}
}

func TestProbeOSC11InvalidResponseFallsBack(t *testing.T) {
	src := &readerSource{in: strings.NewReader("\x1b]11;rgb:zz/00/00\x07")}
	var out strings.Builder
	got := probeOSC11(src, &out, true, time.Second)
	if got.status != oscProbeInvalid {
		t.Fatalf("非法响应状态=%q，应为 %q", got.status, oscProbeInvalid)
	}
	if out.String() != osc11Query+da1Query {
		t.Fatalf("非法响应不等于漏发查询：%q", out.String())
	}
}

type alwaysTimeoutSource struct{}

func (alwaysTimeoutSource) next(bool) (byte, bool, bool) { return 0, true, false }

func TestProbeOSC11TimeoutIsBounded(t *testing.T) {
	var out strings.Builder
	start := time.Now()
	got := probeOSC11(alwaysTimeoutSource{}, &out, true, 15*time.Millisecond)
	elapsed := time.Since(start)
	if got.status != oscProbeTimeout {
		t.Fatalf("超时状态=%q，应为 %q", got.status, oscProbeTimeout)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("15ms 查询却卡住 %v", elapsed)
	}
	if out.String() != osc11Query+da1Query {
		t.Fatalf("超时前应且只应发一轮查询：%q", out.String())
	}
}

func TestStartupThemeExplicitChoiceSkipsProbe(t *testing.T) {
	src := &readerSource{in: strings.NewReader("\x1b]11;rgb:00/00/00\x07")}
	var out strings.Builder
	name, got := startupThemeName(true, "light", src, &out, true, time.Second)
	if name != "light" || got.status != oscProbeExplicit {
		t.Fatalf("显式主题被覆盖：name=%q result=%+v", name, got)
	}
	if out.Len() != 0 {
		t.Fatalf("显式主题不应发 OSC/DA1：%q", out.String())
	}

	out.Reset()
	src = &readerSource{in: strings.NewReader("\x1b]11;rgb:ff/ff/ff\x07")}
	name, got = startupThemeName(false, "dark", src, &out, true, time.Second)
	if name != "light" || got.status != oscProbeOK {
		t.Fatalf("自动主题没有采用浅底：name=%q result=%+v", name, got)
	}
}

func TestStartupThemeFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name        string
		src         byteSource
		interactive bool
		wantStatus  oscProbeStatus
	}{
		{"非法响应", &readerSource{in: strings.NewReader("\x1b]11;rgb:no/00/00\x07")}, true, oscProbeInvalid},
		{"超时", alwaysTimeoutSource{}, true, oscProbeTimeout},
		{"非TTY", &readerSource{in: strings.NewReader("\x1b]11;rgb:00/00/00\x07")}, false, oscProbeNonTTY},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			name, got := startupThemeName(false, "light", tc.src, &out, tc.interactive, 15*time.Millisecond)
			if name != "light" || got.status != tc.wantStatus {
				t.Fatalf("失败时必须保留原回退主题：name=%q status=%q，应为 light/%q", name, got.status, tc.wantStatus)
			}
		})
	}
}

func TestResolveThemeNameTracksExplicitChoice(t *testing.T) {
	oldForced := themeForced
	t.Cleanup(func() { themeForced = oldForced })
	t.Setenv("DSH_TUI_THEME", "")
	t.Setenv("COLORFGBG", "0;15")
	if got := resolveThemeName("dark"); got != "dark" || !themeForced {
		t.Fatalf("--theme 应标为显式选择：got=%q forced=%v", got, themeForced)
	}
	t.Setenv("DSH_TUI_THEME", "light")
	if got := resolveThemeName(""); got != "light" || !themeForced {
		t.Fatalf("DSH_TUI_THEME 应标为显式选择：got=%q forced=%v", got, themeForced)
	}
	t.Setenv("DSH_TUI_THEME", "not-a-theme")
	if got := resolveThemeName(""); got != "light" || themeForced {
		t.Fatalf("非法环境变量不应禁止 OSC；COLORFGBG 只是回退：got=%q forced=%v", got, themeForced)
	}
}

func TestLateOSCResponseNeverBecomesOrdinaryInput(t *testing.T) {
	// 查询窗口结束时只收到半个响应；余下部分在按键解码阶段才到。
	src := &readerSource{in: strings.NewReader("a\x1b]11;rgb:ff/ff/")}
	var out strings.Builder
	probe := probeOSC11(src, &out, true, time.Second)
	if probe.status != oscProbeEOF {
		t.Fatalf("半截响应 EOF 状态=%q", probe.status)
	}
	replayed := &prefixSource{prefix: probe.replay, src: &readerSource{in: strings.NewReader("ff\x07ok")}}
	runes := make(chan rune, 128)
	specials := make(chan string, 16)
	decodeKeys(replayed, runes, specials)
	if got := drainRunes(runes); got != "aok" {
		t.Fatalf("迟到 OSC11 响应漏成普通输入：%q", got)
	}
	if got := drainStrings(specials); len(got) != 0 {
		t.Fatalf("OSC11 响应不应变成 Esc 等特殊键：%q", got)
	}
}

func TestReadKeysSwallowsBELAndSTOSCResponses(t *testing.T) {
	runes := make(chan rune, 128)
	specials := make(chan string, 16)
	readKeysFrom(strings.NewReader("a\x1b]11;rgb:00/00/00\x07b\x1b]11;rgb:ff/ff/ff\x1b\\c"), runes, specials)
	if got := drainRunes(runes); got != "abc" {
		t.Fatalf("BEL/ST OSC 响应漏成普通输入：%q", got)
	}
	if got := drainStrings(specials); len(got) != 0 {
		t.Fatalf("OSC 响应不应产生特殊键：%q", got)
	}
}

func TestIsTerminalFileRejectsPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminalFile(r) || isTerminalFile(w) {
		t.Fatal("os.Pipe 不是 TTY，不得启用 OSC 查询")
	}
}

func drainRunes(ch <-chan rune) string {
	var b strings.Builder
	for len(ch) > 0 {
		b.WriteRune(<-ch)
	}
	return b.String()
}

func drainStrings(ch <-chan string) []string {
	var out []string
	for len(ch) > 0 {
		out = append(out, <-ch)
	}
	return out
}

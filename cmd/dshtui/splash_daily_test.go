package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// 本文件钉住 W59 八款与 W65 第九款 shadow 组成的官方九款**开屏文字字体**。
// 鲸鱼帧、动画和睡眠时钟另有测试，这里不拿字体当鲸鱼，也不凭截图猜字宽或阈值。

var dailyFontExpectations = []struct {
	id                   string
	glyphWidth           int
	topKerning           int
	bottomKerning        int
	bottomIndent         int
	titleWidth           int
	sha256               string
	firstDRow, thirdERow string
}{
	{"bold", 6, 1, 2, 0, 56, "2e0e8905cd93603936e7697813ee7a2fbd4e4d835e405107419167456faf08e5", "██▀▀▄▄", "██▀▀▀·"},
	{"square", 6, 1, 2, 0, 56, "1667b8c206622575fc9d5a1bee64dfebfa0fc1e72d9983fb54a88bab1e655d9c", "██████", "█████·"},
	{"bevel", 6, 1, 2, 0, 56, "35b24b8c960271ad68e805b77ed6761a7da7beb18a00ae23164d9f8dec9e57f0", "██████", "█████·"},
	{"wide", 8, 1, 2, 2, 72, "2f1c5a4482bed22029bdf8ccaef15130a025a88b0ae2757dc62004ef9963b2d5", "███▀▀▄▄▄", "███▀▀▀▀·"},
	{"dot", 6, 1, 2, 0, 56, "9edb3639d356ac250f8260580ed3b0c57483cbd3a16618d2403bd9ec3312601d", "▓▓▒▒▒▒", "▓▓▒▒▒·"},
	{"stencil", 6, 1, 2, 0, 56, "9a332e06c916a6ad018a3b667f7f16067e4a1f907d9d911923bcefcef1e01bc2", "██▀▀▄▄", "█ ▀▀▀·"},
	{"classic", 5, 2, 3, 0, 56, "ae8fb06eba45a1ba1d1a170243562c71efa837e89058c2987f397824b8145cc1", "█▀▀▀▄", "█▀▀▀·"},
	{"slab", 5, 2, 3, 0, 56, "b03b86f21999918d4334009e6e4aa1eb30e07bf4d1b10e64d97e4078d685b085", "████·", "████·"},
	{"shadow", 10, 1, 1, 0, 88, "ee2223a2221631fcce18bf5d9ab62f9c2d72f522af4278bff8999b1b9d40b5bb", "·██████╗··", "·██╔══╝···"},
}

// splashAssetDigest 是**测试侧**的稳定序列化：包含 id、字身宽、全部 18 个字形
// 和 fallback。期望值由本机上游 splashFonts.ts 独立计算，不是从生成文件抄回。
func splashAssetDigest(asset splashFontAsset) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%d\n", asset.id, len([]rune(asset.fallback[0])))
	keys := make([]string, 0, len(asset.glyphs))
	for ch := range asset.glyphs {
		keys = append(keys, string(ch))
	}
	sort.Strings(keys)
	for _, key := range keys {
		rows := asset.glyphs[[]rune(key)[0]]
		fmt.Fprintf(h, "%s:\n%s\n", key, strings.Join(rows[:], "\n"))
	}
	fmt.Fprintf(h, "fallback:\n%s\n", strings.Join(asset.fallback[:], "\n"))
	return hex.EncodeToString(h.Sum(nil))
}

func TestDailyFontAssetsMatchUpstream(t *testing.T) {
	if splashFontSourceSHA256 != "eaaaca75d1e6edf98bc004ed28da7cb0e0420b9fcdd9392087bd0b60c28707bd" {
		t.Fatalf("生成器所读 splashFonts.ts 摘要变了：%s", splashFontSourceSHA256)
	}
	if len(splashFaces) != 9 || len(splashFontAssets) != 9 {
		t.Fatalf("轮换池必须恰好 9 款，faces=%d assets=%d", len(splashFaces), len(splashFontAssets))
	}
	for i, want := range dailyFontExpectations {
		f, asset := splashFaces[i], splashFontAssets[i]
		if f.id != want.id || asset.id != want.id {
			t.Errorf("第 %d 款应为 %s，得到 face=%s asset=%s", i, want.id, f.id, asset.id)
			continue
		}
		if got := splashAssetDigest(asset); got != want.sha256 {
			t.Errorf("%s 字形内容 sha256=%s，应为 %s", want.id, got, want.sha256)
		}
		if len(f.glyphs) != 18 {
			t.Errorf("%s 字形数=%d，应为 18", want.id, len(f.glyphs))
		}
		if f.glyphWidth != want.glyphWidth || f.topKerning != want.topKerning ||
			f.bottomKerning != want.bottomKerning || f.bottomIndent != want.bottomIndent {
			t.Errorf("%s 度量=(%d,%d,%d,%d)，应为 (%d,%d,%d,%d)", want.id,
				f.glyphWidth, f.topKerning, f.bottomKerning, f.bottomIndent,
				want.glyphWidth, want.topKerning, want.bottomKerning, want.bottomIndent)
		}
		wantUniform := 0
		if want.id == "shadow" {
			wantUniform = 1
		}
		if f.uniformKerning != wantUniform {
			t.Errorf("%s uniformKerning=%d，应为 %d", want.id, f.uniformKerning, wantUniform)
		}
		if got := splashTitleWidth(f); got != want.titleWidth {
			t.Errorf("%s 标题宽=%d，应为 %d", want.id, got, want.titleWidth)
		}
		both := want.titleWidth + splashColumnGap + whaleBoxWidth
		layoutCases := []struct {
			columns           int
			whale, big, plain bool
		}{
			{both, true, true, false},
			{both - 1, false, true, false},
			{want.titleWidth, false, true, false},
			{want.titleWidth - 1, true, false, false},
			{39, false, false, true},
		}
		for _, tc := range layoutCases {
			got := resolveSplashLayout(tc.columns, true, f)
			if got.showWhale != tc.whale || got.showBigTitle != tc.big || got.showPlainTitle != tc.plain {
				t.Errorf("%s/%d 列布局={whale:%v big:%v plain:%v}，应为 {%v %v %v}", want.id,
					tc.columns, got.showWhale, got.showBigTitle, got.showPlainTitle, tc.whale, tc.big, tc.plain)
			}
		}
		topPainted := paintedWidth(f.glyphWidth, f.topWord, f.topKerning)
		bottomPainted := paintedWidth(f.glyphWidth, f.bottomWord, f.bottomKerning) + f.bottomIndent
		if want.id == "shadow" {
			// 官方 uniformKerning=1 明确放弃等宽求解：88 vs 77，下排由渲染层半差居中。
			if topPainted != 88 || bottomPainted != 77 {
				t.Errorf("shadow 必须保持 uniform 字距宽度 top=88 bottom=77，实际 %d/%d", topPainted, bottomPainted)
			}
		} else if topPainted != bottomPainted {
			t.Errorf("%s 两排 paintedWidth 未按官方契约对齐", want.id)
		}
		if got := f.glyphs['D'][0]; got != want.firstDRow {
			t.Errorf("%s D 顶行=%q，应为 %q", want.id, got, want.firstDRow)
		}
		if got := f.glyphs['E'][2]; got != want.thirdERow {
			t.Errorf("%s E 第三行=%q，应为 %q", want.id, got, want.thirdERow)
		}
		for ch, rows := range f.glyphs {
			for y, row := range rows {
				if got := len([]rune(row)); got != f.glyphWidth {
					t.Errorf("%s/%c 第 %d 行宽=%d，应为 %d", want.id, ch, y, got, f.glyphWidth)
				}
			}
		}
		for y, row := range f.fallback {
			if got := len([]rune(row)); got != f.glyphWidth {
				t.Errorf("%s fallback 第 %d 行宽=%d，应为 %d", want.id, y, got, f.glyphWidth)
			}
		}
	}
}

func TestDailyFontSelectionMatchesOfficialDates(t *testing.T) {
	// 显式日期路径不受 CI pin 影响，与上游 pickSplashFont(date) 一致。
	t.Setenv("DSH_TUI_SPLASH_FONT", "slab")
	loc := time.FixedZone("test-local", 8*60*60)
	cases := []struct {
		day  int
		date time.Time
		want string
	}{
		{-10, time.Date(1969, 12, 22, 12, 0, 0, 0, loc), "shadow"}, // 负余数归一化
		{-9, time.Date(1969, 12, 23, 12, 0, 0, 0, loc), "bold"},
		{-1, time.Date(1969, 12, 31, 12, 0, 0, 0, loc), "shadow"},
		{0, time.Date(1970, 1, 1, 0, 0, 0, 0, loc), "bold"},
		{1, time.Date(1970, 1, 2, 0, 0, 0, 0, loc), "square"},
		{2, time.Date(1970, 1, 3, 0, 0, 0, 0, loc), "bevel"},
		{3, time.Date(1970, 1, 4, 0, 0, 0, 0, loc), "wide"},
		{4, time.Date(1970, 1, 5, 0, 0, 0, 0, loc), "dot"},
		{5, time.Date(1970, 1, 6, 0, 0, 0, 0, loc), "stencil"},
		{6, time.Date(1970, 1, 7, 0, 0, 0, 0, loc), "classic"},
		{7, time.Date(1970, 1, 8, 23, 59, 59, 0, loc), "slab"},
		{8, time.Date(1970, 1, 9, 0, 0, 0, 0, loc), "shadow"},
		{9, time.Date(1970, 1, 10, 0, 0, 0, 0, loc), "bold"},
		{10, time.Date(1970, 1, 11, 0, 0, 0, 0, loc), "square"},
		{20761, time.Date(2026, 11, 4, 8, 0, 0, 0, loc), "slab"},
		{20762, time.Date(2026, 11, 5, 8, 0, 0, 0, loc), "shadow"},
		{20763, time.Date(2026, 11, 6, 8, 0, 0, 0, loc), "bold"},
		{20764, time.Date(2026, 11, 7, 8, 0, 0, 0, loc), "square"},
	}
	for _, tc := range cases {
		if got := splashFaceForDate(tc.date).id; got != tc.want {
			t.Errorf("day=%d %s 应选 %s，实际 %s", tc.day, tc.date.Format(time.RFC3339), tc.want, got)
		}
	}
}

func TestDailyFontUsesLocalDateAcrossMidnightAndZones(t *testing.T) {
	instant := time.Date(2026, 11, 5, 16, 30, 0, 0, time.UTC)
	shanghai := time.FixedZone("UTC+8", 8*60*60)
	losAngeles := time.FixedZone("UTC-8", -8*60*60)
	if got := splashFaceForDate(instant.In(shanghai)).id; got != "bold" {
		t.Fatalf("同一 UTC 时刻在上海已是 11-06，应选 bold，实际 %s", got)
	}
	if got := splashFaceForDate(instant.In(losAngeles)).id; got != "shadow" {
		t.Fatalf("同一 UTC 时刻在 UTC-8 仍是 11-05，应选 shadow，实际 %s", got)
	}

	t.Setenv("DSH_TUI_SPLASH_FONT", "")
	now := time.Date(2026, 11, 5, 23, 59, 59, 0, shanghai)
	a := newTestApp(120, 36)
	a.now = func() time.Time { return now }
	if got := a.splashInfo().face.id; got != "shadow" {
		t.Fatalf("跨日前应选 shadow，实际 %s", got)
	}
	before := a.splashInfo().face
	if again := a.splashInfo().face; !reflect.DeepEqual(again, before) {
		t.Fatal("同一本地日期的重复渲染选了不同字体")
	}
	beforeScreen := strings.Join(a.view(), "\n")
	now = now.Add(2 * time.Second)
	if got := a.splashInfo().face.id; got != "bold" {
		t.Fatalf("跨过本地午夜后应立刻改选 bold，实际 %s", got)
	}
	if afterScreen := strings.Join(a.view(), "\n"); afterScreen == beforeScreen {
		t.Fatal("跨过本地午夜后，实际屏幕仍是昨天的字体")
	}
}

func TestDailyFontPinDoesNotHideDailyOrUnknownResources(t *testing.T) {
	shadowDate := time.Date(2026, 11, 5, 12, 0, 0, 0, time.Local)
	boldDate := time.Date(2026, 11, 6, 12, 0, 0, 0, time.Local)
	t.Setenv("DSH_TUI_SPLASH_FONT", "bold")
	if got := currentSplashFace(shadowDate).id; got != "bold" {
		t.Fatalf("合法 pin 应钉住 bold，实际 %s", got)
	}
	if got := splashFaceForDate(shadowDate).id; got != "shadow" {
		t.Fatalf("显式日期不应读 bold pin，实际 %s", got)
	}
	t.Setenv("DSH_TUI_SPLASH_FONT", "shadow")
	if got := currentSplashFace(boldDate).id; got != "shadow" {
		t.Fatalf("合法 shadow pin 应覆盖 daily=bold，实际 %s", got)
	}
	if got := splashFaceForDate(boldDate).id; got != "bold" {
		t.Fatalf("显式日期不应读 shadow pin，实际 %s", got)
	}
	t.Setenv("DSH_TUI_SPLASH_FONT", "not-a-font")
	if got := currentSplashFace(shadowDate).id; got != "shadow" {
		t.Fatalf("非法 pin 应按官方规则回 daily=shadow，不是 bold；实际 %s", got)
	}
	if f, err := splashFontByID("not-a-font"); err == nil || f.id != "" {
		t.Fatalf("未知字体 id 必须显式报错，face=%+v err=%v", f, err)
	}
}

func cloneDailyFontAssets() []splashFontAsset {
	out := make([]splashFontAsset, len(splashFontAssets))
	copy(out, splashFontAssets)
	for i := range out {
		out[i].glyphs = make(map[rune][5]string, len(splashFontAssets[i].glyphs))
		for ch, rows := range splashFontAssets[i].glyphs {
			out[i].glyphs[ch] = rows
		}
	}
	return out
}

func TestDailyFontResourcesFailLoud(t *testing.T) {
	if len(splashFontAssets) != 9 {
		t.Fatalf("九款资源未就绪，无法执行 shadow 变异：assets=%d", len(splashFontAssets))
	}
	missing := cloneDailyFontAssets()
	missing = append(missing[:2], missing[3:]...)
	if _, err := buildSplashFonts(missing); err == nil || !strings.Contains(err.Error(), "9") {
		t.Fatalf("缺一款字体必须报数量错误，err=%v", err)
	}

	nilGlyphs := cloneDailyFontAssets()
	nilGlyphs[2].glyphs = nil
	if _, err := buildSplashFonts(nilGlyphs); err == nil ||
		!strings.Contains(err.Error(), "bevel") || !strings.Contains(err.Error(), "字形") {
		t.Fatalf("bevel 缺整张字形表必须指名报错，err=%v", err)
	}

	missingD := cloneDailyFontAssets()
	delete(missingD[0].glyphs, 'D')
	if _, err := buildSplashFonts(missingD); err == nil ||
		!strings.Contains(err.Error(), "bold") || !strings.Contains(err.Error(), "D") {
		t.Fatalf("bold 缺 D 必须指名报错，err=%v", err)
	}

	badWidth := cloneDailyFontAssets()
	row := badWidth[3].glyphs['D']
	row[0] = "x"
	badWidth[3].glyphs['D'] = row
	if _, err := buildSplashFonts(badWidth); err == nil || !strings.Contains(err.Error(), "wide") {
		t.Fatalf("wide 字身宽损坏必须报错，err=%v", err)
	}

	nilShadow := cloneDailyFontAssets()
	nilShadow[8].glyphs = nil
	if _, err := buildSplashFonts(nilShadow); err == nil ||
		!strings.Contains(err.Error(), "shadow") || !strings.Contains(err.Error(), "字形表") {
		t.Fatalf("shadow 缺整张字形表必须指名报错，err=%v", err)
	}

	badShadowUniform := cloneDailyFontAssets()
	badShadowUniform[8].uniformKerning = 0
	if _, err := buildSplashFonts(badShadowUniform); err == nil ||
		!strings.Contains(err.Error(), "shadow") || !strings.Contains(err.Error(), "uniformKerning") {
		t.Fatalf("shadow 缺 uniformKerning=1 必须指名报错，err=%v", err)
	}

	missingShadowD := cloneDailyFontAssets()
	delete(missingShadowD[8].glyphs, 'D')
	if _, err := buildSplashFonts(missingShadowD); err == nil ||
		!strings.Contains(err.Error(), "shadow") || !strings.Contains(err.Error(), "D") {
		t.Fatalf("shadow 缺 D 必须指名报错，err=%v", err)
	}

	badShadowWidth := cloneDailyFontAssets()
	shadowRow := badShadowWidth[8].glyphs['D']
	shadowRow[0] = "x"
	badShadowWidth[8].glyphs['D'] = shadowRow
	if _, err := buildSplashFonts(badShadowWidth); err == nil || !strings.Contains(err.Error(), "shadow") {
		t.Fatalf("shadow 字形行宽损坏必须指名报错，err=%v", err)
	}

	badShadowFallback := cloneDailyFontAssets()
	badShadowFallback[8].fallback[2] = "x"
	if _, err := buildSplashFonts(badShadowFallback); err == nil ||
		!strings.Contains(err.Error(), "shadow") || !strings.Contains(err.Error(), "fallback") {
		t.Fatalf("shadow fallback 行宽损坏必须指名报错，err=%v", err)
	}

	defer func() {
		got := recover()
		if got == nil || !strings.Contains(fmt.Sprint(got), "开屏字体资源") {
			t.Fatalf("启动校验必须 fail-loud panic，recover=%v", got)
		}
	}()
	_ = mustBuildSplashFonts(nilGlyphs)
}

func TestDailyFontDumpsAreDeterministicPerDate(t *testing.T) {
	for _, want := range dailyFontExpectations {
		t.Run(want.id, func(t *testing.T) {
			t.Setenv("DSH_TUI_SPLASH_FONT", want.id)
			var first, second, errb strings.Builder
			for _, size := range []string{"120x36", "80x24"} {
				first.Reset()
				second.Reset()
				errb.Reset()
				args := []string{"--dump-screen", "--size", size}
				if code := run(args, &first, &errb); code != 0 {
					t.Fatalf("%s 第一次 dump 退出码=%d stderr=%q", size, code, errb.String())
				}
				if code := run(args, &second, &errb); code != 0 {
					t.Fatalf("%s 第二次 dump 退出码=%d stderr=%q", size, code, errb.String())
				}
				if first.String() != second.String() {
					t.Fatalf("%s 同一日期、同一 pin 的 dump 字节不同", size)
				}
				sum := sha256.Sum256([]byte(first.String()))
				t.Logf("dump %s sha256=%x", size, sum)
			}

			first.Reset()
			second.Reset()
			errb.Reset()
			transcriptArgs := []string{"--dump-transcript", "testdata/markdown_strike_frames.json",
				"--size", "80x24", "--expand", "--show-tool-results"}
			if code := run(transcriptArgs, &first, &errb); code != 0 {
				t.Fatalf("第一次 transcript 退出码=%d stderr=%q", code, errb.String())
			}
			if code := run(transcriptArgs, &second, &errb); code != 0 {
				t.Fatalf("第二次 transcript 退出码=%d stderr=%q", code, errb.String())
			}
			if first.String() != second.String() {
				t.Fatal("同一字体下 transcript 字节不同")
			}
			transcriptSum := sha256.Sum256([]byte(first.String()))
			t.Logf("transcript sha256=%x", transcriptSum)
		})
	}
}

func TestDailyFontAllSizesDoNotOverflow(t *testing.T) {
	sizes := [][2]int{{120, 36}, {80, 24}, {40, 10}, {30, 8}, {100, 4}, {20, 3}, {8, 2}, {1, 1}}
	for _, want := range dailyFontExpectations {
		t.Run(want.id, func(t *testing.T) {
			t.Setenv("DSH_TUI_SPLASH_FONT", want.id)
			for _, size := range sizes {
				var out, errb strings.Builder
				arg := fmt.Sprintf("%dx%d", size[0], size[1])
				if code := run([]string{"--dump-screen", "--size", arg}, &out, &errb); code != 0 {
					t.Fatalf("%s 退出码=%d stderr=%q", arg, code, errb.String())
				}
				lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
				if len(lines) != size[1] {
					t.Fatalf("%s 行数=%d，应为 %d", arg, len(lines), size[1])
				}
				for i, line := range lines {
					if got := displayWidth(line); got > size[0] {
						t.Fatalf("%s 第 %d 行宽=%d，超过 %d", arg, i, got, size[0])
					}
				}
			}
		})
	}
}

func TestDailyFontWhaleVisibilityUsesWideTitleWidth(t *testing.T) {
	a, _ := newAnimatedTestApp(100, 36, "classic")
	// 保留 W59 的 wide 回归：标题 72 列，100 列只留大字，114 列才与鲸鱼并排。
	a.now = func() time.Time { return time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC) }
	t.Setenv("DSH_TUI_SPLASH_FONT", "wide")
	settleAppWhale(a)
	if got := a.splashInfo().face.id; got != "wide" {
		t.Fatalf("前置 pin 应选 wide，实际 %s", got)
	}
	if a.whaleVisible() {
		t.Fatal("wide/100 列不应显示或推进鲸鱼；bold 阈值会造成误判")
	}
	a.scr.width = 114
	if !a.whaleVisible() {
		t.Fatal("wide/114 列恰好并排，鲸鱼动画应可见")
	}
}

func TestShadowUniformKerningCentersBottomWithoutEqualizing(t *testing.T) {
	f, err := splashFontByID("shadow")
	if err != nil {
		t.Fatalf("官方第九款 shadow 必须可按 id 取用：%v", err)
	}
	if f.topKerning != 1 || f.bottomKerning != 1 || f.bottomIndent != 0 {
		t.Fatalf("shadow uniformKerning=1 应得到 (1,1,0)，实际 (%d,%d,%d)",
			f.topKerning, f.bottomKerning, f.bottomIndent)
	}
	if got := splashTitleWidth(f); got != 88 {
		t.Fatalf("shadow 标题宽必须按真实 10 列字身与字距 1 算为 88，实际 %d", got)
	}
	lay := resolveSplashLayout(88, true, f)
	if lay.showWhale || !lay.showBigTitle || lay.showPlainTitle {
		t.Fatalf("shadow/88 列应只留大字，实际 %+v", lay)
	}
	lines := splashHeader(splashInfo{columns: 88, face: f}, chromeMinimal)
	if len(lines) != 12 {
		t.Fatalf("minimal shadow 应为 margin+两排 5 行+间隔=12 行，实际 %d", len(lines))
	}
	wantTop := renderBigText(f, f.topWord, 1, 0, color{}, color{})
	wantBottom := renderBigText(f, f.bottomWord, 1, 0, color{}, color{})
	for i := 0; i < bigTextRows; i++ {
		if got, want := lines[1+i].plain(), wantTop[i].plain(); got != want {
			t.Errorf("shadow 上排第 %d 行不应额外缩进\ngot  %q\nwant %q", i, got, want)
		}
		// paintedTop=88、paintedBottom=77，官方 Math.round(11/2)=6。
		if got, want := lines[7+i].plain(), "      "+wantBottom[i].plain(); got != want {
			t.Errorf("shadow 下排第 %d 行必须按半差缩进 6 列\ngot  %q\nwant %q", i, got, want)
		}
	}
}

func TestShadowWhaleVisibilityUsesActualTitleWidth(t *testing.T) {
	f, err := splashFontByID("shadow")
	if err != nil {
		t.Fatalf("shadow 资源缺失：%v", err)
	}
	for _, tc := range []struct {
		columns int
		whale   bool
		big     bool
		plain   bool
	}{
		{100, false, true, false},
		{114, false, true, false},
		{129, false, true, false},
		{130, true, true, false},
		{88, false, true, false},
		{87, true, false, false},
	} {
		got := resolveSplashLayout(tc.columns, true, f)
		if got.showWhale != tc.whale || got.showBigTitle != tc.big || got.showPlainTitle != tc.plain {
			t.Errorf("shadow/%d 列布局=%+v，应为 whale=%v big=%v plain=%v", tc.columns, got, tc.whale, tc.big, tc.plain)
		}
	}

	a, _ := newAnimatedTestApp(100, 36, "classic")
	a.now = func() time.Time { return time.Date(1970, 1, 9, 12, 0, 0, 0, time.UTC) } // day=8
	t.Setenv("DSH_TUI_SPLASH_FONT", "")
	settleAppWhale(a)
	if got := a.splashInfo().face.id; got != "shadow" {
		t.Fatalf("day=8 应选 shadow，实际 %s", got)
	}
	for _, width := range []int{100, 114, 129} {
		a.scr.width = width
		if a.whaleVisible() {
			t.Errorf("shadow/%d 列不足 130，不应显示或推进鲸鱼", width)
		}
	}
	a.scr.width = 130
	if !a.whaleVisible() {
		t.Fatal("shadow/130 列恰好并排，鲸鱼动画应可见")
	}
}

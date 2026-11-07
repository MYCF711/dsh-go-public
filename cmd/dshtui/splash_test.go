package main

// 开屏（首屏）的结构契约。
//
// 这些断言钉的都是**上游源码里写死的数**：13 行鲸鱼、40 列鲸鱼盒、2 列 gap、
// 5 行点阵、两排字宽相等、阶梯的四个档位阈值。手抄或重排一次就会红，
// 而"红"的时候人知道该去看 docs/tui-visual.md 的对照表——
// 这比在截图上看"鲸鱼好像小了一点"可靠得多。

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"dshgo/cmd/dshtui/assets"
)

// 点阵表的结构契约：每个字形**恰好 5 行**，且 5 行**等宽**。
//
// 为什么值得一测：手抄或生成错一格，观感上只是"这个字母有点怪"，
// 而版面会整体错位——那是最难定位的一类错。
func TestGlyphsAreFiveEqualWidthRows(t *testing.T) {
	if len(splashFaces[0].glyphs) < 18 {
		t.Fatalf("字形表只有 %d 个，生成多半断了", len(splashFaces[0].glyphs))
	}
	for ch, rows := range splashFaces[0].glyphs {
		if len(rows) != 5 {
			t.Fatalf("字形 %c 是 %d 行，应为 5 行", ch, len(rows))
		}
		w := len([]rune(rows[0]))
		for i, r := range rows {
			if got := len([]rune(r)); got != w {
				t.Fatalf("字形 %c 第 %d 行宽 %d，第 0 行宽 %d——不等宽会让版面错位", ch, i, got, w)
			}
		}
	}
}

// 两款标题用到的字必须都在表里，否则会静默走 fallback 块——
// 屏幕上出现一个方框，没人知道是"缺字"还是设计如此。
func TestWordmarkCharsExist(t *testing.T) {
	for _, ch := range splashTopWord + splashBottomWord {
		if _, ok := splashFaces[0].glyphs[ch]; !ok {
			t.Fatalf("字形表缺 %c，开屏会渲染成 fallback 块", ch)
		}
	}
}

// 字体度量：字身宽度取自字形表（bold 基准款是 6 列），
// 字距由上游求解器解出，且解出来的两排**画出来的列数相等**。
//
// 三个数（1 / 2 / 0）不是抄来的：它是 `painted(top)=painted(bottom)` 与
// 「下排墨迹居中」两条约束在 glyphWidth=6、8 字 vs 7 字下的最小解。
// 上游 verify 脚本同样钉这三个数。
func TestTaglineKerningSolvesToUpstreamValues(t *testing.T) {
	f := splashFaces[0]
	if f.glyphWidth != 6 {
		t.Fatalf("bold 款字身宽应为 6 列，实际 %d", f.glyphWidth)
	}
	if f.topKerning != 1 || f.bottomKerning != 2 || f.bottomIndent != 0 {
		t.Fatalf("DEEPSEEK/HARNESS 的字距解应为 (1,2,0)，实际 (%d,%d,%d)",
			f.topKerning, f.bottomKerning, f.bottomIndent)
	}
	top := paintedWidth(f.glyphWidth, f.topWord, f.topKerning)
	bottom := paintedWidth(f.glyphWidth, f.bottomWord, f.bottomKerning) + f.bottomIndent
	if top != bottom {
		t.Fatalf("两排画出来的列数不等：上 %d，下 %d——版面会一边长一边短", top, bottom)
	}
	if top != splashTitleWidth(f) {
		t.Fatalf("标题块宽 %d 与 paintedWidth %d 不一致", splashTitleWidth(f), top)
	}
}

// 阶梯四档：阈值是 upstream 的 98（= 56 标题 + 2 gap + 40 鲸鱼）、56（标题）、40（鲸鱼盒）。
func TestSplashLayoutLadder(t *testing.T) {
	f := splashFaces[0]
	title := splashTitleWidth(f) // 56
	both := title + splashColumnGap + whaleBoxWidth
	if title != 56 || both != 98 {
		t.Fatalf("阈值变了：标题 %d（应 56）、并排 %d（应 98）", title, both)
	}
	cases := []struct {
		columns                      int
		wantWhale, wantBig, wantText bool
		why                          string
	}{
		{120, true, true, false, "宽终端：鲸鱼 + 大字"},
		{98, true, true, false, "正好并排得下"},
		{97, false, true, false, "差一列：先撤鲸鱼，大字不能截断"},
		{80, false, true, false, "常见窄档：只留大字"},
		{56, false, true, false, "标题正好放得下"},
		{55, true, false, false, "标题放不下、鲸鱼放得下：鲸鱼独占一档"},
		{40, true, false, false, "正好放得下鲸鱼盒"},
		{39, false, false, true, "两者都放不下：一行纯文字"},
	}
	for _, c := range cases {
		got := resolveSplashLayout(c.columns, true, f)
		if got.showWhale != c.wantWhale || got.showBigTitle != c.wantBig || got.showPlainTitle != c.wantText {
			t.Fatalf("%d 列（%s）：得到 {鲸鱼:%v 大字:%v 纯文字:%v}，应为 {%v %v %v}",
				c.columns, c.why, got.showWhale, got.showBigTitle, got.showPlainTitle,
				c.wantWhale, c.wantBig, c.wantText)
		}
	}
	// 关掉鲸鱼（上游 `dsh-tui.whale` 设置）时，宽终端也只留大字。
	if got := resolveSplashLayout(200, false, f); got.showWhale {
		t.Fatal("whale=false 时不该画鲸鱼")
	}
}

// 鲸鱼 art：13 行、40 列。行数是 25 个像素行按半块编码（每格 2 行）折出来的，
// 不是随手定的常数——精灵改了行数这里必须跟着红。
func TestWhaleArtIs13By40(t *testing.T) {
	rows := whaleArtLines()
	if len(rows) != whaleArtRows || whaleArtRows != 13 {
		t.Fatalf("鲸鱼应为 13 行，实际 %d（whaleArtRows=%d）", len(rows), whaleArtRows)
	}
	for i, r := range rows {
		if w := r.width(); w > whaleBoxWidth {
			t.Fatalf("第 %d 行宽 %d 列，超过鲸鱼盒 %d 列", i, w, whaleBoxWidth)
		}
		if strings.TrimSpace(r.plain()) == "" {
			continue // 上下留白的行是正常的（精灵本身有 2 行透明）
		}
		for _, ch := range r.plain() {
			if ch != '▀' && ch != '▄' && ch != ' ' {
				t.Fatalf("第 %d 行出现了半块之外的字符 %q——半块编码只产出 ▀/▄/空格", i, ch)
			}
		}
	}
}

// 精灵原文必须仍是上游 `whaleFrames.ts` 里那 25 行（逐字节）。
//
// 这条钉的不是"文件没被 git 改过"，而是"内容仍然等于从上游抽出来的那段"：
// 上游换了立绘、或有人顺手调了一格，它会红。
func TestWhaleSpriteMatchesUpstreamDigest(t *testing.T) {
	sum := sha256.Sum256([]byte(assets.WhaleStandard))
	if got := hex.EncodeToString(sum[:]); got != assets.WhaleStandardSHA256 {
		t.Fatalf("鲸鱼精灵的 sha256 变了：%s（记录值 %s）\n要么上游换了立绘（更新 docs/tui-visual.md），要么有人改了像素",
			got, assets.WhaleStandardSHA256)
	}
	rows := assets.WhaleRows()
	if len(rows) != assets.WhaleHeight {
		t.Fatalf("精灵应有 %d 行，实际 %d", assets.WhaleHeight, len(rows))
	}
	for i, r := range rows {
		if len([]rune(r)) != assets.WhaleWidth {
			t.Fatalf("精灵第 %d 行 %d 列，应为 %d", i, len([]rune(r)), assets.WhaleWidth)
		}
	}
}

// 表里没有的字形走 fallback，**不许跳过**：跳过会让后面所有字符左移，
// 错位比缺字更难看出问题在哪。
func TestUnknownRuneUsesFallbackNotSkip(t *testing.T) {
	rows := renderBigText(splashFaces[0], "D?E", 1, 0, color{}, color{})
	if len(rows) != bigTextRows {
		t.Fatalf("应渲染 %d 行，实际 %d", bigTextRows, len(rows))
	}
	want := (splashFaces[0].glyphWidth+1)*3 - 1 // 3 个字、末尾字距被裁掉
	if got := rows[0].width(); got != want {
		t.Fatalf("未知字符应当占一个 fallback 宽度：整行宽 %d，应为 %d", got, want)
	}
}

// 上色：带颜色的渲染必须真的带真彩序列，且**每行收尾复位**——
// 忘了复位会让之后所有输出都染色。
func TestSplashColorEmitsPerLineReset(t *testing.T) {
	info := splashInfo{columns: 120, model: "m", effort: "high", cwd: "/w", tip: "t", face: splashFaces[0]}
	lines := splashFirstScreen(info, 30)
	colored := 0
	for _, ln := range lines {
		s := ln.render(true)
		if !strings.Contains(s, "\x1b[38;2;") {
			continue
		}
		colored++
		if !strings.HasSuffix(s, "\x1b[0m") {
			t.Fatalf("上色行没有收尾复位：%q", s)
		}
	}
	if colored < 10 {
		t.Fatalf("上色行数太少（%d）：配色没有真的落到大字/鲸鱼上", colored)
	}
	// 无颜色渲染不许带任何转义序列。
	for _, ln := range lines {
		if s := ln.render(false); strings.Contains(s, "\x1b") {
			t.Fatalf("无颜色渲染里出现了转义序列：%q", s)
		}
	}
}

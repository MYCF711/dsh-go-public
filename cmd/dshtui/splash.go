package main

// 首屏 header：13 行像素鲸鱼 + DEEPSEEK/HARNESS 点阵大字。
//
// 这里是**按上游源码逐项移植**，不是照截图手绘。对照表（都在 docs/tui-visual.md）：
//
//	src/components/splashLayout.ts   4 档窄终端阶梯（阈值算术逐行照抄）
//	src/components/splashFonts.ts    bold 基准款字形（已在 splash_gen.go）+ 字距求解器
//	src/components/bigfont.ts        paintedWidth / bigTextWidth / 画法
//	src/components/Whale.tsx         半块编码（每格两个像素压进 ▀/▄），40×25 → 40×13
//	src/components/whaleFrames.ts    standard 帧的 25 行（assets/whale_standard.txt）
//	src/components/LogoV2.tsx        组合顺序：词标/版本 → 两排大字 → 模型/目录/提示 → 欢迎语
//	src/branding.ts                  两排词与"纯文字档"的文案
//
// 为什么值得这么麻烦：手抄 5 行点阵错一格只会让某个字母"看着有点怪"；
// 而鲸鱼的阈值错一列，窄终端上就是整块消失或者把大字挤出屏幕——
// 这两种错都很难从截图上看出来，但都能从**算法**上断言。

import (
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"dshgo/cmd/dshtui/assets"
)

const (
	// whaleBoxWidth 是鲸鱼 art 的固定盒宽：甩尾帧比标准姿势向右多出 4 列，
	// 钉死宽度才不让文字列跟着左右跳（splashLayout.ts: WHALE_BOX_WIDTH）。
	// 本端只用静止帧，但这个数同时是**阶梯阈值**的一部分，照抄。
	whaleBoxWidth = assets.WhaleWidth
	// splashColumnGap 是鲸鱼与文字列之间的间隔列数（COLUMN_GAP）。
	splashColumnGap = 2
	// whaleArtRows 是半块编码后的行数：25 个像素行 → 每格 2 行 → 13 行。
	whaleArtRows = (assets.WhaleHeight + 1) / 2
	// whaleCenter 是立绘包围盒的中心列（精灵第 3..34 列），欢迎语按它居中。
	whaleCenter = 18.5
	// bigTextRows 是一排大字的行数（5 行点阵）。
	bigTextRows = 5
	// bigWordGap 是词组里空格画的列数（bigfont.ts: WORD_GAP）。
	bigWordGap = 2
	// maxKerning 是字距上限：撞上限还解不出来就走兜底，而不是把字距一直放大。
	maxKerning = 8
)

// 两排大字与"纯文字档"的文案（branding.ts: BRAND_SPLASH_WORDS.deepseek）。
const (
	splashTopWord    = "DEEPSEEK"
	splashBottomWord = "HARNESS"
	splashPlainWord  = "DeepSeek Harness"
)

// welcomeTagline 是欢迎语（i18n.ts: logo-tagline 的 zh 侧）。
const welcomeTagline = "探索未至之境！"

// splashTipPrefix / splashTipMore 是提示行的前后缀（i18n.ts: logo-tip-prefix / logo-tip-more）。
const (
	splashTipPrefix = "提示："
	splashTipMore   = " · /help 更多技巧"
)

// splashTips 是"提示行"的候选文案。
//
// 上游这一步是从一张几十条的技法表里随机挑一条（`/tips` 面板）。
// 本端没有 `/tips`，所以文案换成**这个界面真的支持**的按键/命令：
// 照抄上游的句子会教人按不存在的键，那比不显示提示更糟。
// 首次出现的提示固定取第 0 条（快照可复现），之后按 app 的 tipIdx 轮换。
var splashTips = []string{
	"Ctrl+T 折叠/展开工具结果 · Ctrl+F 搜索会话",
	"PgUp/PgDn 翻一屏 · Ctrl+End 回到底部继续跟随",
	"/resume 恢复历史会话 · /export 导出 Markdown",
	"/compact 压缩上下文 · /clear 清屏",
	"审批时按 y 允许这一次 · a 以后都允许 · n 拒绝",
}

// splashFontAsset 是生成器产出的一款原始字体资源。
type splashFontAsset struct {
	id             string
	uniformKerning int // 0=普通等宽求解；shadow 官方声明为 1。
	glyphs         map[rune][5]string
	fallback       [5]string
}

// splashFont 是一款开屏大字字体：稳定 id + 字身宽度 + 字形表 + 两排词的字距。
type splashFont struct {
	id             string
	glyphWidth     int
	uniformKerning int
	glyphs         map[rune][5]string
	fallback       [5]string
	topWord        string
	bottomWord     string
	topKerning     int
	bottomKerning  int
	bottomIndent   int
}

var splashFontOrder = [9]string{"bold", "square", "bevel", "wide", "dot", "stencil", "classic", "slab", "shadow"}

// buildSplashFonts 校验完整轮换池并求每款字距。缺款、缺表、缺标题字或行宽损坏
// 都指名报错；绝不能用 bold 顶替，否则用户会以为当天字体/设置已生效。
func buildSplashFonts(assets []splashFontAsset) ([]splashFont, error) {
	if len(assets) != len(splashFontOrder) {
		return nil, fmt.Errorf("开屏字体应有 %d 款，实际 %d 款", len(splashFontOrder), len(assets))
	}
	faces := make([]splashFont, 0, len(assets))
	for i, asset := range assets {
		wantID := splashFontOrder[i]
		if asset.id != wantID {
			return nil, fmt.Errorf("开屏字体第 %d 款应为 %s，实际 %q", i, wantID, asset.id)
		}
		if asset.uniformKerning < 0 || asset.uniformKerning > maxKerning {
			return nil, fmt.Errorf("开屏字体 %s uniformKerning=%d 超出 0..%d", asset.id, asset.uniformKerning, maxKerning)
		}
		if asset.id == "shadow" && asset.uniformKerning != 1 {
			return nil, fmt.Errorf("开屏字体 shadow uniformKerning=%d，应为官方 1", asset.uniformKerning)
		}
		if asset.id != "shadow" && asset.uniformKerning != 0 {
			return nil, fmt.Errorf("开屏字体 %s 不应声明 uniformKerning=%d", asset.id, asset.uniformKerning)
		}
		if asset.glyphs == nil {
			return nil, fmt.Errorf("开屏字体 %s 缺字形表", asset.id)
		}
		gw := len([]rune(asset.fallback[0]))
		if gw == 0 {
			return nil, fmt.Errorf("开屏字体 %s fallback 为空", asset.id)
		}
		for y, row := range asset.fallback {
			if got := len([]rune(row)); got != gw {
				return nil, fmt.Errorf("开屏字体 %s fallback 第 %d 行宽=%d，应为 %d", asset.id, y, got, gw)
			}
		}
		for ch, rows := range asset.glyphs {
			for y, row := range rows {
				if got := len([]rune(row)); got != gw {
					return nil, fmt.Errorf("开屏字体 %s 字形 %c 第 %d 行宽=%d，应为 %d", asset.id, ch, y, got, gw)
				}
			}
		}
		for _, ch := range splashTopWord + splashBottomWord {
			if _, ok := asset.glyphs[ch]; !ok {
				return nil, fmt.Errorf("开屏字体 %s 缺标题字形 %c", asset.id, ch)
			}
		}
		if len(asset.glyphs) != 18 {
			return nil, fmt.Errorf("开屏字体 %s 字形数=%d，应为 18", asset.id, len(asset.glyphs))
		}
		face := newSplashFont(asset.glyphs, asset.fallback, splashTopWord, splashBottomWord, asset.uniformKerning)
		face.id = asset.id
		faces = append(faces, face)
	}
	return faces, nil
}

func mustBuildSplashFonts(assets []splashFontAsset) []splashFont {
	faces, err := buildSplashFonts(assets)
	if err != nil {
		panic(fmt.Sprintf("开屏字体资源错误: %v", err))
	}
	return faces
}

// newSplashFont 用字形表造一款字体。普通款按上游求解器解字距；声明
// uniformKerning 的 shadow 两行同用固定字距、bottomIndent 恒 0，居中交给渲染层。
// 可变参数保留旧资源/算法测试的直接调用形状；生产构建总是显式传入资源声明。
func newSplashFont(glyphs map[rune][5]string, fallback [5]string, top, bottom string, fixed ...int) splashFont {
	gw := len([]rune(fallback[0]))
	uniformKerning := 0
	if len(fixed) > 0 {
		uniformKerning = fixed[0]
	}
	tk, bk, ind := 0, 0, 0
	if uniformKerning > 0 {
		tk, bk, ind = uniformKerning, uniformKerning, 0
	} else {
		tk, bk, ind = solveTagline(gw, top, bottom)
	}
	return splashFont{
		glyphWidth:     gw,
		uniformKerning: uniformKerning,
		glyphs:         glyphs,
		fallback:       fallback,
		topWord:        top,
		bottomWord:     bottom,
		topKerning:     tk,
		bottomKerning:  bk,
		bottomIndent:   ind,
	}
}

var splashFaces = mustBuildSplashFonts(splashFontAssets)

// splashFace 是 animation_app.go 既有布局/命中测试读取的当前款。splashInfo 每次
// 按当时本地日期刷新它；纯资源/算法测试直接使用 splashFaces，不依赖这个兼容缝。
var splashFace = splashFaces[0]

// splashFaceForDate 是上游 pickSplashFont(date) 的逐行移植：取**本地**年月日，
// 构造 UTC 当日零点得到稳定日序号，再对官方九款取模并归一化负余数。显式日期不读 env。
func splashFaceForDate(now time.Time) splashFont {
	year, month, day := now.Date()
	dayNumber := time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Unix() / 86400
	n := int64(len(splashFaces))
	index := ((dayNumber % n) + n) % n
	return splashFaces[index]
}

// currentSplashFace 保留上游无参路径的 CI 确定性缝：合法 DSH_TUI_SPLASH_FONT
// 钉住一款；非法值忽略并继续 daily，而不是静默变成 bold。
func currentSplashFace(now time.Time) splashFont {
	if pinned := os.Getenv("DSH_TUI_SPLASH_FONT"); pinned != "" {
		if face, err := splashFontByID(pinned); err == nil {
			return face
		}
	}
	return splashFaceForDate(now)
}

func splashFontByID(id string) (splashFont, error) {
	for _, face := range splashFaces {
		if face.id == id {
			return face, nil
		}
	}
	return splashFont{}, fmt.Errorf("开屏字体资源缺少 id %q", id)
}

// paintedWidth 是 text 在该字身宽度、该字距下**画出来**的列数（含末尾那格字距留白）。
func paintedWidth(glyphWidth int, text string, kerning int) int {
	w := 0
	for _, ch := range text {
		if ch == ' ' {
			w += bigWordGap
			continue
		}
		w += glyphWidth + kerning
	}
	return w
}

// bigTextWidth 是 text 的显示宽度：含字距、**不含**最后一个字形之后的留白。
func bigTextWidth(glyphWidth int, text string, kerning int) int {
	chars := []rune(text)
	if len(chars) == 0 {
		return 0
	}
	trailing := kerning
	if chars[len(chars)-1] == ' ' {
		trailing = bigWordGap
	}
	return paintedWidth(glyphWidth, text, kerning) - trailing
}

// solveTagline 解一对词的标题字距，让两行**画出来的列数相等**，
// 且下排墨迹在上排墨迹下居中（左右留白差 ≤ 1 列）。
//
// 契约（painted 含末尾字距留白，ink 不含）：
//
//	painted(top, tk) = painted(bottom, bk) + indent   （indent ≥ 0，两行等宽）
//	|ink(top, tk) − ink(bottom, bk) − 2·indent| ≤ 1   （下排墨迹居中）
//
// 取解顺序与上游一致：先要求字距 ≥1（字身相接会糊成一片），按 `tk+bk`
// 从小到大取第一个解——字距最紧、画面最不松散；无解时才退到允许 0。
//
// 本端唯一的上游未写死的加固：`bottomKerning` 不许超过 maxKerning。
// 上游那层循环不挡这一条，只是在当前词表下永远走不到（先返回了）。
func solveTagline(glyphWidth int, top, bottom string) (topKerning, bottomKerning, bottomIndent int) {
	for _, minKerning := range []int{1, 0} {
		for sum := minKerning * 2; sum <= maxKerning*2; sum++ {
			hi := sum - minKerning
			if hi > maxKerning {
				hi = maxKerning
			}
			for tk := minKerning; tk <= hi; tk++ {
				bk := sum - tk
				if bk > maxKerning {
					continue
				}
				indent := paintedWidth(glyphWidth, top, tk) - paintedWidth(glyphWidth, bottom, bk)
				if indent < 0 {
					continue
				}
				err := absInt(bigTextWidth(glyphWidth, top, tk) - bigTextWidth(glyphWidth, bottom, bk) - 2*indent)
				if err <= 1 {
					return tk, bk, indent
				}
			}
		}
	}
	return 1, 1, 0
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// splashLayout 是一行开屏头部在该宽度下要渲染哪些部件。
type splashLayout struct {
	showWhale      bool
	showBigTitle   bool
	showPlainTitle bool
}

// resolveSplashLayout 解析开屏头部的阶梯档位（splashLayout.ts 逐行照抄）。
//
//	① 像素鲸鱼 + DEEPSEEK/HARNESS 大字 —— 两者都放得下
//	② 只留大字 logo                  —— 先撤鲸鱼（大字被截断会毁掉品牌字形）
//	③ 只留像素鲸鱼                    —— 大字放不下，鲸鱼顶上撑住开屏
//	④ 一行纯文字                      —— 两者都放不下
//
// columns 是**内容区**列数（本端不套页边距，就是终端宽）。
func resolveSplashLayout(columns int, whale bool, f splashFont) splashLayout {
	// 阈值按**画出来**的列数算（每行末尾还会画出一格字距），空格子宁可少画一格，
	// 字形被吃掉才可惜；两行取较宽的那行——只看上排会把下排截掉。
	titleWidth := maxInt(
		bigTextWidth(f.glyphWidth, f.topWord, f.topKerning)+f.topKerning,
		bigTextWidth(f.glyphWidth, f.bottomWord, f.bottomKerning)+f.bottomKerning,
	)
	fitsTitle := columns >= titleWidth
	showWhale := whale && columns >= whaleBoxWidth &&
		(columns >= titleWidth+splashColumnGap+whaleBoxWidth || !fitsTitle)
	return splashLayout{
		showWhale:      showWhale,
		showBigTitle:   fitsTitle,
		showPlainTitle: !fitsTitle && !showWhale,
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// splashTitleWidth 是两排大字占的列数（测试与缩进都用它，避免两处各算一遍）。
func splashTitleWidth(f splashFont) int {
	return maxInt(
		bigTextWidth(f.glyphWidth, f.topWord, f.topKerning)+f.topKerning,
		bigTextWidth(f.glyphWidth, f.bottomWord, f.bottomKerning)+f.bottomKerning,
	)
}

// splashTitleIndents 复刻 LogoV2 的两排水平对齐。普通款使用求解器 bottomIndent；
// shadow 的 uniformKerning 不追求等宽，而是把较窄一排按 paintedWidth 半差居中。
// JS Math.round 对这里的非负半差等价于 (delta+1)/2（0.5 向上）。
func splashTitleIndents(f splashFont) (topIndent, bottomShift int) {
	if f.uniformKerning == 0 {
		return 0, f.bottomIndent
	}
	topPainted := paintedWidth(f.glyphWidth, f.topWord, f.topKerning)
	bottomPainted := paintedWidth(f.glyphWidth, f.bottomWord, f.bottomKerning)
	return centeredTitleIndent(bottomPainted - topPainted), centeredTitleIndent(topPainted - bottomPainted)
}

func centeredTitleIndent(delta int) int {
	if delta <= 0 {
		return 0
	}
	return (delta + 1) / 2
}

// lerpColor 在 from→to 之间按 t∈[0,1] 取色（逐通道线性）。
func lerpColor(from, to color, t float64) color {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	mix := func(a, b uint8) uint8 {
		return uint8(math.Round(float64(a) + (float64(b)-float64(a))*t))
	}
	return color{r: mix(from.r, to.r), g: mix(from.g, to.g), b: mix(from.b, to.b), set: true}
}

// renderBigText 用某款字体渲染一行大字：渐变从 from 走到 to。
//
// 画法照大上游 bigfont.ts：整块宽度 = paintedWidth + indent，
// 逐格按 `x/(width-1)` 取渐变；空格与 `·`（透明）画成空格且不上色，
// 但**照样占一格 x**——少算一格会让渐变整体左移。
//
// 扫光高光（sweep）第一阶段不做：静态帧 t=0 时上游的高光窗口恰好落在画布左侧
// 之外（sweepStart = -8），所以不画高光与上游 t=0 的画面**逐格一致**。
func renderBigText(f splashFont, word string, kerning, indent int, from, to color) []styledLine {
	width := paintedWidth(f.glyphWidth, word, kerning) + indent
	rows := make([]styledLine, 0, bigTextRows)
	for row := 0; row < bigTextRows; row++ {
		x := 0
		line := styledLine{}
		// push 把一个格子接到行尾；同色的相邻格子合并成一个 span。
		push := func(text string, c color) {
			if n := len(line); n > 0 && line[n-1].fg == c {
				line[n-1].text += text
				return
			}
			line = append(line, span{text: text, fg: c})
		}
		for i := 0; i < indent; i++ {
			push(" ", color{})
			x++
		}
		for _, ch := range word {
			if ch == ' ' {
				for i := 0; i < bigWordGap; i++ {
					push(" ", color{})
					x++
				}
				continue
			}
			glyph, ok := f.glyphs[ch]
			if !ok {
				glyph = f.fallback
			}
			for _, cell := range glyph[row] {
				if cell == ' ' || cell == '·' {
					push(" ", color{})
					x++
					continue
				}
				t := 0.0
				if width > 1 {
					t = float64(x) / float64(width-1)
				}
				push(string(cell), lerpColor(from, to, t))
				x++
			}
			for i := 0; i < kerning; i++ {
				push(" ", color{})
				x++
			}
		}
		rows = append(rows, trimTrailingSpaces(line))
	}
	return rows
}

// trimTrailingSpaces 把行尾的空白格裁掉（上游 `row.replace(/\s+$/u,”)`）。
// 不裁的话，盒子按含尾距的宽度居中，墨迹整体被往左拽约半个字距。
func trimTrailingSpaces(l styledLine) styledLine {
	for len(l) > 0 {
		last := &l[len(l)-1]
		last.text = strings.TrimRight(last.text, " ")
		if last.text != "" {
			break
		}
		l = l[:len(l)-1]
	}
	return l
}

// whaleArtLines 把 40×25 的精灵编码成 13 行 × 40 列（半块：每格两个像素）。
//
// 前景=上像素、背景=下像素，字符固定 `▀`；只有下半有像素时用 `▄`。
// 只有一个半格有像素时**必须把背景复位**：SGR 会跨格保持，上一格留下的背景
// 会在空格半格里画出幽灵像素——那正是轮廓一圈毛刺的来源（Whale.tsx 的注释）。
func whaleArtLines(injected ...[]string) []styledLine {
	sprite := assets.WhaleRows()
	if len(injected) > 0 && len(injected[0]) > 0 {
		sprite = injected[0]
	}
	rows := make([]styledLine, 0, whaleArtRows)
	for y := 0; y < len(sprite); y += 2 {
		line := styledLine{}
		push := func(text string, fg, bg color) {
			if n := len(line); n > 0 && line[n-1].fg == fg && line[n-1].bg == bg {
				line[n-1].text += text
				return
			}
			line = append(line, span{text: text, fg: fg, bg: bg})
		}
		for x := 0; x < whaleBoxWidth; x++ {
			up, okUp := whalePixel(sprite, x, y)
			lo, okLo := whalePixel(sprite, x, y+1)
			switch {
			case okUp && okLo:
				push("▀", up, lo)
			case okUp:
				push("▀", up, color{})
			case okLo:
				push("▄", lo, color{})
			default:
				push(" ", color{}, color{})
			}
		}
		rows = append(rows, trimTrailingSpaces(line))
	}
	return rows
}

// whalePixel 取精灵 (x,y) 的颜色；透明或越界返回 ok=false。
func whalePixel(sprite []string, x, y int) (color, bool) {
	if y < 0 || y >= len(sprite) || x < 0 || x >= len(sprite[y]) {
		return color{}, false
	}
	rgb, ok := assets.WhalePalette[rune(sprite[y][x])]
	if !ok {
		return color{}, false
	}
	return color{r: rgb.R, g: rgb.G, b: rgb.B, set: true}, true
}

// splashChrome 是首屏的信息面：full 带词标/版本/模型/目录/提示/欢迎语，
// minimal 只有立绘与大写字母（上游的落地页形态）。行数不够时用它降级。
type splashChrome int

const (
	chromeFull splashChrome = iota
	chromeMinimal
)

// splashInfo 是拼首屏 header 需要的全部真实数据。
//
// 每一个字段都来自宿主或本地环境，**没有一个是从别处猜的**：
// model/cwd 来自 `session.load`，version 来自构建注入，tip 是本端真支持的键，
// welcome 是上游 i18n 原文。不知道的字段传空，由渲染层显示 `?`。
type splashInfo struct {
	columns int
	model   string
	effort  string
	cwd     string
	tip     string
	// face 是本次渲染的当地日期字体；零值仅供旧的直接渲染测试回退到兼容缝。
	face splashFont
	// sprite 为空时保持旧快照的 standard；交互 app 注入开屏整帧或 idle 合成帧。
	sprite []string
}

// splashHeader 拼出 header 的全部行（不含"放不放得下"的判断）。
//
// 组合顺序照 LogoV2.tsx 的 full/minimal 两种形态：
//
//	[空行（marginTop 1）]
//	┌ 立绘槽（40 列） ┬ gap 2 ┬ 文字列 ┐
//	│  13 行鲸鱼       │        │ 词标+版本 / 两排大字 / 模型 / 目录 / 提示 │
//	└─────────────────┴────────┴──────────┘
//	[空行]
//	欢迎语（在鲸鱼盒中线下方居中）
func splashHeader(info splashInfo, chrome splashChrome) []styledLine {
	theme := currentTheme()
	face := info.face
	if face.id == "" {
		face = splashFace
	}
	lay := resolveSplashLayout(info.columns, true, face)

	// 文字列。鲸鱼独占一档（③）时**整列不画**：只剩几列的话画出来是
	// `✦ DSH…` 这种残句，开屏就留鲸鱼 + 下面的标语。
	var text []styledLine
	if lay.showBigTitle || lay.showPlainTitle {
		if chrome == chromeFull {
			text = append(text, splashWordmarkLine(theme))
		}
		if lay.showBigTitle {
			topIndent, bottomShift := splashTitleIndents(face)
			text = append(text, renderBigText(face, face.topWord,
				face.topKerning, topIndent, theme.accent, theme.activity)...)
			text = append(text, blankLine(0))
			// 普通款用求解器 bottomIndent；shadow 用 paintedWidth 半差居中，
			// 保持 uniformKerning=1，绝不为了等宽把两行字距拉成 5/7。
			text = append(text, renderBigText(face, face.bottomWord,
				face.bottomKerning, bottomShift, theme.activity, splashPale)...)
		} else {
			// 纯文字档（④）：品牌词的 plain 写法，一行。
			text = append(text, styledLine{{text: splashPlainWord, fg: theme.accent, bold: true}})
		}
		if chrome == chromeFull {
			text = append(text, splashModelLine(info, theme))
			text = append(text, styledLine{{text: info.cwd, fg: theme.inactive}})
			text = append(text, splashTipLine(info, theme))
		}
	}

	var out []styledLine
	out = append(out, blankLine(0)) // marginTop 1

	switch {
	case lay.showWhale && len(text) > 0:
		art := whaleArtLines(info.sprite)
		// 立绘槽与文字列等高，鲸鱼在其中垂直居中（Ink alignItems="center"）。
		total := maxInt(len(text), len(art))
		off := (total - len(art)) / 2
		for i := 0; i < total; i++ {
			row := styledLine{}
			if j := i - off; j >= 0 && j < len(art) {
				row = append(row, art[j]...)
			} else {
				row = append(row, span{text: strings.Repeat(" ", whaleBoxWidth)})
			}
			// 立绘盒固定 40 列：即使这一行被裁过尾空白，也要补回来，
			// 不然 gap 与文字列会跟着行内的墨迹左右跳。
			if pad := whaleBoxWidth - row.width(); pad > 0 {
				row = append(row, span{text: strings.Repeat(" ", pad)})
			}
			row = append(row, span{text: strings.Repeat(" ", splashColumnGap)})
			if i < len(text) {
				row = append(row, text[i]...)
			}
			out = append(out, row)
		}
	case lay.showWhale:
		out = append(out, whaleArtLines(info.sprite)...)
	case len(text) > 0:
		out = append(out, text...)
	}

	// 欢迎语：只在 full 形态画（上游 minimal 形态整块不画）。
	if chrome == chromeFull {
		out = append(out, blankLine(0))
		pad := splashWelcomePad(lay.showWhale)
		for _, ln := range strings.Split(welcomeTagline, "\n") {
			row := styledLine{{text: strings.Repeat(" ", pad)}}
			row = append(row, span{text: ln, fg: theme.activity})
			out = append(out, row)
		}
	}
	// 上游 Ink 会在终端边界裁掉溢出；本端快照/真屏共用同一行数组，
	// 必须在出口做同一件事。截断保留 span 渐变，不把鲸鱼和大字压成单色。
	for i := range out {
		out[i] = truncStyledLine(out[i], info.columns)
	}
	return out
}

// splashPale 是大字下排的收尾色（shimmer.ts: PALE #D7E4FF）。
//
// deepseek 档的两排色阶是：上排 accent → activity，下排 activity → PALE。
// 它不随主题变——上游 deepseek 分支就是写死这一档"蓝白"收尾。
var splashPale = color{r: 215, g: 228, b: 255, set: true}

// splashWordmarkLine 是 `✦ DSH TUI GO  v0.2.1-go.NN` 那一行。
//
// 上游写的是 `✦ dsh-TUI`（他们的产品名）。本端的产品名只有一个出处（brand.go），
// 所以这里取 productName——两处各写一个名字，迟早有一处是旧的。
func splashWordmarkLine(theme themeTokens) styledLine {
	return styledLine{
		{text: "✦ " + productName, fg: theme.accent, bold: true},
		{text: "  v" + version, fg: theme.inactive},
	}
}

// splashModelLine 是 `model · <Effort> effort`。
//
// 两条都可能是未知：model 来自 `session.load`（老宿主/新会话可能没有），
// effort 在 Transport 里**没有投影**（宿主有 `--reasoning`，但没进 load）。
// 未知就写 `?`——上游的注释也是这个意思："不许拿我们的默认值冒充"。
func splashModelLine(info splashInfo, theme themeTokens) styledLine {
	model := info.model
	if model == "" {
		model = "?"
	}
	effort := info.effort
	if effort == "" {
		effort = "?"
	}
	return styledLine{
		{text: model, fg: theme.text},
		{text: " · " + effort + " effort", fg: theme.inactive},
	}
}

// splashTipLine 是提示行：`提示：<一条真的能用的键> · /help 更多技巧`。
func splashTipLine(info splashInfo, theme themeTokens) styledLine {
	return styledLine{
		{text: splashTipPrefix, fg: theme.inactive},
		{text: info.tip, fg: theme.text},
		{text: splashTipMore, fg: theme.inactive},
	}
}

// splashWelcomePad 是欢迎语相对立绘盒中线的左缩进。
//
// 上游：有立绘就把标语钉在鲸鱼包围盒中线下（WHALE_CENTER），
// 没有立绘就退到 2 列——那 2 列正好是文字列原本的起点，视觉上对齐。
func splashWelcomePad(showWhale bool) int {
	if !showWhale {
		return 2
	}
	return maxInt(0, int(math.Round(whaleCenter-float64(displayWidth(welcomeTagline))/2)))
}

// splashFirstScreen 返回"空会话时的首屏正文"（不超过 bodyRows 行）。
//
// 上游没有"终端太矮怎么办"这一档（Ink 会把放不下的部分裁掉，而裁掉的是
// **底部**——欢迎语和提示行最先消失，正好是最不重要的两块）。本端的降级顺序：
//
//	① full（词标/大字/模型/目录/提示 + 欢迎语）放得下 → 用它
//	② 放不下 → minimal（立绘 + 大字），这是上游自己的落地页形态
//	③ 还放不下 → 一行纯文字（阶梯第 ④ 档）
//
// 三档都是上游存在过的形态，不是这里新发明的版面。
func splashFirstScreen(info splashInfo, bodyRows int) []styledLine {
	if bodyRows < 1 {
		bodyRows = 1
	}
	for _, chrome := range []splashChrome{chromeFull, chromeMinimal} {
		if lines := splashHeader(info, chrome); len(lines) <= bodyRows {
			return lines
		}
	}
	return []styledLine{{{text: splashPlainWord, fg: currentTheme().accent, bold: true}}}
}

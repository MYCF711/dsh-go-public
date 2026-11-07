package main

// 首屏视觉与消息区/状态行的**屏幕级**断言。
//
// 这里断言的都是"屏幕上看得见什么"：鲸鱼几行、标题在不在、欢迎语在不在、
// 用户/助手/思考的前缀对不对、上下文条未知时是不是 `?`。
//
// 为什么用快照而不是逐个字段查：布局是**整体**的性质——某一块多占一行，
// 另一块就被挤出屏幕，而每个字段单独看都"对"。所以这里既有逐字段的
// Contains，也有"恰好 N 行"和"鲸鱼恰好 13 行"这种结构性断言。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// whaleRowCount 数快照里"鲸鱼的行"。
//
// 判据：含半块字符（▀/▄）但**不含** █。—— 大字点阵用的是 █，鲸鱼用的是
// 半块编码（▀/▄），所以这条判据把两者分得干干净净，
// 而不是靠"第 1..13 行"这种和版式耦合的位置假设。
func whaleRowCount(lines []string) int {
	n := 0
	for _, ln := range lines {
		runes := []rune(ln)
		if len(runes) > whaleBoxWidth {
			runes = runes[:whaleBoxWidth]
		}
		head := string(runes)
		if strings.ContainsAny(head, "▀▄") && !strings.ContainsRune(head, '█') {
			n++
		}
	}
	return n
}

func dumpLines(t *testing.T, size string) []string {
	t.Helper()
	t.Setenv("DSH_TUI_SPLASH_FONT", "bold")
	out, err := dumpScreenSnapshot(size, false)
	if err != nil {
		t.Fatalf("dump %s: %v", size, err)
	}
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

// whaleInkedRows 是立绘里"真有像素"的行数。
//
// 精灵的 25 个像素行两端各留了一行白的，按半块编码后 13 行里有两行整行空白
// ——那两行**也是鲸鱼**（它们撑住 13 行的高度），只是没有墨迹。
func whaleInkedRows() int {
	n := 0
	for _, r := range whaleArtLines() {
		if strings.TrimSpace(r.plain()) != "" {
			n++
		}
	}
	return n
}

// 并排组合的逐行契约：13 行立绘从正文区第 1 行起，横向钉在 40 列盒里，
// 后面正好 2 列 gap，再后面才是文字列。
//
// 这条是"鲸鱼和文字不会互相挤"的**结构**证明：只要有一行的立绘槽宽度不是
// 40 列（比如某帧甩尾多出 4 列），文字列就会跟着左右跳——那是肉眼很难
// 归因的一类错。
func TestSplashHeaderComposesWhaleBesideTitle(t *testing.T) {
	info := splashInfo{columns: 120, model: "m", effort: "high", cwd: "/w", tip: "t", face: splashFaces[0]}
	lines := splashHeader(info, chromeFull)
	art := whaleArtLines()

	// 第 0 行是 marginTop（上游 LogoV2 的 marginTop={1}）。
	if strings.TrimSpace(lines[0].plain()) != "" {
		t.Fatalf("首行应当是空行：%q", lines[0].plain())
	}
	const (
		margin = 1 // marginTop
		off    = 1 // 立绘在 15 行的文字列里垂直居中
	)
	for i, a := range art {
		want := padRight(a.plain(), whaleBoxWidth) + strings.Repeat(" ", splashColumnGap)
		if got := lines[margin+off+i].plain(); !strings.HasPrefix(got, want) {
			t.Fatalf("第 %d 行（立绘第 %d 行）：立绘槽不对；得到 %q，应为 %q 开头",
				margin+off+i, i, got, want)
		}
	}
	// 词标与两排大字的起点都钉在 40 + 2 列之后。
	col := whaleBoxWidth + splashColumnGap
	if got := lines[margin].plain(); !strings.HasPrefix(got[col:], "✦ "+productName) {
		t.Fatalf("词标不在第 %d 列：%q", col, got)
	}
	if got := lines[margin+1].plain(); !strings.HasPrefix(got[col:], "██▀▀▄▄") {
		t.Fatalf("大字第一排不在第 %d 列：%q", col, got)
	}
	// 文字列共 15 行（词标 1 + 两排 10 + 之间空 1 + 模型/目录/提示 3）。
	if got := len(lines); got != 1+15+2 {
		t.Fatalf("full 形态应为 1(上边距)+15(文字列)+1(空行)+1(欢迎语)=18 行，实际 %d", got)
	}
}

// 120×36 这一档：13 行鲸鱼 + 两排大字 + 版本行 + 欢迎语 + 状态行，一个都不能少。
func TestDumpScreen120HasWhaleTitlesAndStatus(t *testing.T) {
	lines := dumpLines(t, "120x36")
	if len(lines) != 36 {
		t.Fatalf("快照应恰好 36 行（终端高度），实际 %d", len(lines))
	}
	if got, want := whaleRowCount(lines), whaleInkedRows(); got != want {
		t.Fatalf("鲸鱼有墨迹的行应占 %d 行，实际数到 %d 行", want, got)
	}
	// 每一行都不许超过终端宽度：超了终端会自己折行，把整屏推下去一格。
	for i, ln := range lines {
		if w := displayWidth(ln); w > 120 {
			t.Fatalf("第 %d 行宽 %d 列，超过 120", i, w)
		}
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"✦ " + productName, // 词标
		"v" + version,      // 版本行
		"effort",           // 模型/effort 行（未知时显示 ?）
		"提示：",              // 提示行
		welcomeTagline,     // 欢迎语
		"工具 ",              // 状态行里的工具数
		"ctx ",             // 状态行里的上下文用量
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("120×36 快照缺少 %q：\n%s", want, joined)
		}
	}
	// 大字的字形必须真的画出来了：D 的顶行是 `██▀▀▄▄`。
	if !strings.Contains(joined, "██▀▀▄▄") {
		t.Fatalf("没看到 DEEPSEEK 的点阵字形：\n%s", joined)
	}
}

// 80×24 这一档：按阶梯**先撤鲸鱼**（大字放得下、并排放不下），
// 但标题、欢迎语、状态行都还在。
func TestDumpScreen80DegradesToBigTitleOnly(t *testing.T) {
	lines := dumpLines(t, "80x24")
	if len(lines) != 24 {
		t.Fatalf("快照应恰好 24 行，实际 %d", len(lines))
	}
	if got := whaleRowCount(lines); got != 0 {
		t.Fatalf("80 列放不下「标题 56 + gap 2 + 鲸鱼 40」，鲸鱼应当整块撤掉，实际还有 %d 行", got)
	}
	for i, ln := range lines {
		if w := displayWidth(ln); w > 80 {
			t.Fatalf("第 %d 行宽 %d 列，超过 80", i, w)
		}
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "██▀▀▄▄") {
		t.Fatalf("80 列应当还留得住大字：\n%s", joined)
	}
	for _, want := range []string{welcomeTagline, "工具 ", "ctx ", "effort"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("80×24 快照缺少 %q：\n%s", want, joined)
		}
	}
}

// 再窄/再矮也不能破"恰好 height 行、不超宽"这两条：
// 破了就是终端折行 + 底部残留，而那看起来像"消息重复了"。
func TestDumpScreenKeepsSizeAtAnyTerminal(t *testing.T) {
	for _, size := range [][2]int{{120, 36}, {80, 24}, {40, 10}, {30, 8}, {100, 4}, {20, 3}, {8, 2}, {1, 1}} {
		w, h := size[0], size[1]
		lines := dumpLines(t, itoa(w)+"x"+itoa(h))
		if len(lines) != h {
			t.Fatalf("%dx%d：快照 %d 行，应为 %d", w, h, len(lines), h)
		}
		for i, ln := range lines {
			if got := displayWidth(ln); got > w {
				t.Fatalf("%dx%d：第 %d 行 %d 列，超过终端宽", w, h, i, got)
			}
		}
	}
}

// 真实 cwd/model 可能比文字列长：并排时应裁在文字列内，纯文字档也不许超屏。
// 这条不依赖运行测试的当前目录长度，避免长 cwd 恰好通过、短 cwd 反而破版。
func TestSplashMetadataAlwaysFitsTerminal(t *testing.T) {
	for _, width := range []int{120, 80} {
		info := splashInfo{
			columns: width,
			model:   strings.Repeat("模型", 40),
			effort:  "high",
			cwd:     strings.Repeat("目录", 60),
			tip:     strings.Repeat("长提示", 40),
			face:    splashFaces[0],
		}
		for i, ln := range splashHeader(info, chromeFull) {
			if got := ln.width(); got > width {
				t.Fatalf("%d 列首屏第 %d 行宽 %d：%q", width, i, got, ln.plain())
			}
		}
	}
}

// 空会话才画首屏；一旦有了内容，屏幕上就不该再出现开屏 header
// （否则每滚一屏都要和鲸鱼抢地方）。
func TestSplashOnlyWhenTranscriptEmpty(t *testing.T) {
	a := newTestApp(120, 36)
	if got, want := whaleRowCount(a.view()), whaleInkedRows(); got != want {
		t.Fatalf("空会话应当画 %d 行有墨迹的鲸鱼，实际 %d", want, got)
	}
	a.pushRole(roleUser, "你好")
	if got := whaleRowCount(a.view()); got != 0 {
		t.Fatalf("有内容之后不该再画鲸鱼，实际还有 %d 行", got)
	}
}

// 消息区的前缀：用户 `❯`、助手 `●`，续行只缩进不重复前缀。
func TestMessageRolePrefixes(t *testing.T) {
	a := newTestApp(80, 20)
	a.pushBlock(roleUser, roleUserCont, "第一段\n第二段")
	a.handleFrame(ServerFrame{Type: "assistant/message",
		Data: json.RawMessage(`{"text":"回答第一行\n回答第二行"}`)})

	screen := a.view()
	if !strings.HasPrefix(screen[0], "❯ 第一段") {
		t.Fatalf("用户消息应以 ❯ 开头：%q", screen[0])
	}
	if !strings.HasPrefix(screen[1], "  第二段") {
		t.Fatalf("用户消息续行应只缩进两格：%q", screen[1])
	}
	var assistantLine int = -1
	for i, ln := range screen {
		if strings.HasPrefix(ln, "● ") {
			assistantLine = i
			break
		}
	}
	if assistantLine < 0 {
		t.Fatalf("助手消息应以 ● 开头：\n%s", strings.Join(screen, "\n"))
	}
	if !strings.HasPrefix(screen[assistantLine], "● 回答第一行") {
		t.Fatalf("助手消息前缀不对：%q", screen[assistantLine])
	}
	if !strings.HasPrefix(screen[assistantLine+1], "  回答第二行") {
		t.Fatalf("助手续行应只缩进：%q", screen[assistantLine+1])
	}
}

// 思考行：默认折叠成 `⚓ 思考 · 12s（Ctrl+O 展开）`，正文不可见；
// 12s 只来自显式 durationMs；Ctrl+O 展开后正文出现，提示改成"收起"。
func TestThinkingRowFoldsAndUsesExplicitDuration(t *testing.T) {
	clk := time.Date(2026, 11, 4, 20, 0, 0, 0, time.Local)
	a := newTestApp(100, 24)
	a.now = func() time.Time { return clk }
	a.start = clk.Add(-12 * time.Second)
	a.busy = true
	a.handleFrame(ServerFrame{Type: "assistant/message",
		Data: json.RawMessage(`{"reasoning":"先想清楚这一句\n再想下一句","durationMs":12000}`)})

	joined := strings.Join(a.view(), "\n")
	if !strings.Contains(joined, "⚓ 思考 · 12s") {
		t.Fatalf("折叠行应是「⚓ 思考 · 12s」：\n%s", joined)
	}
	if !strings.Contains(joined, "Ctrl+O 展开") {
		t.Fatalf("折叠行应带展开提示：\n%s", joined)
	}
	if strings.Contains(joined, "先想清楚这一句") {
		t.Fatalf("默认折叠时思考正文不该出现在屏幕上：\n%s", joined)
	}

	a.handleRune(15) // Ctrl+O
	joined = strings.Join(a.view(), "\n")
	if !strings.Contains(joined, "先想清楚这一句") || !strings.Contains(joined, "再想下一句") {
		t.Fatalf("展开后思考正文应当出现：\n%s", joined)
	}
	if !strings.Contains(joined, "Ctrl+O 收起") {
		t.Fatalf("展开后提示应变成收起：\n%s", joined)
	}
	a.handleRune(15) // 再按一次
	if strings.Contains(strings.Join(a.view(), "\n"), "先想清楚这一句") {
		t.Fatal("再按一次应当折回去")
	}
}

// 短于 1 秒的思考不写时长：上游同样是 `durationMs >= 1000` 才显示，
// 写一个 `0s` 会让人以为"它没思考"。
func TestThinkingOmitsSubSecondDuration(t *testing.T) {
	clk := time.Date(2026, 11, 4, 20, 0, 0, 0, time.Local)
	a := newTestApp(100, 24)
	a.now = func() time.Time { return clk }
	a.start = clk.Add(-300 * time.Millisecond)
	a.busy = true
	a.handleFrame(ServerFrame{Type: "assistant/message",
		Data: json.RawMessage(`{"reasoning":"短","durationMs":300}`)})
	joined := strings.Join(a.view(), "\n")
	if strings.Contains(joined, "思考 · 0s") || strings.Contains(joined, "思考 · 1s") {
		t.Fatalf("不到 1 秒不该显示时长：\n%s", joined)
	}
	if !strings.Contains(joined, "⚓ 思考") {
		t.Fatalf("思考行本身要显示：\n%s", joined)
	}
}

// 输入行的前缀是 `❯`（上游 POINTER），搜索模式仍是本端既有的 `/ `。
func TestInputPromptUsesPointer(t *testing.T) {
	a := newTestApp(80, 12)
	a.input = []rune("你好")
	got := a.view()
	if !strings.HasPrefix(got[len(got)-1], "❯ 你好") {
		t.Fatalf("输入行应以「❯ 」开头：%q", got[len(got)-1])
	}
	a.enterSearch()
	if !strings.HasPrefix(a.view()[len(a.view())-1], "/ ") {
		t.Fatalf("搜索模式前缀应保持 /：%q", a.view()[len(a.view())-1])
	}
}

// 上下文条：用量未知时读数是 `?`（不是 0、不是一条空轨就完事），
// 已知时右对齐显示 `已用/上限 百分比`。
func TestContextBarUnknownAndKnown(t *testing.T) {
	a := newTestApp(80, 24)
	// 未知：整条是空余段 + 右对齐的 `?`
	unknown := a.contextBarRow(currentTheme()).plain()
	if len(unknown) != 80 {
		t.Fatalf("上下文条应恰好 80 列，实际 %d", len(unknown))
	}
	if !strings.HasSuffix(unknown, "?") {
		t.Fatalf("未知用量时读数应是 ?：%q", unknown)
	}
	if strings.TrimSpace(strings.TrimSuffix(unknown, "?")) != "" {
		t.Fatalf("未知用量时不该画出已用段：%q", unknown)
	}

	a.ctxTokens, a.ctxWindow = 25000, 100000
	known := a.contextBarRow(currentTheme()).plain()
	if len(known) != 80 {
		t.Fatalf("上下文条应恰好 80 列，实际 %d", len(known))
	}
	trimmed := strings.TrimRight(known, " ")
	if !strings.HasSuffix(trimmed, "25.0%") {
		t.Fatalf("已知用量时读数应带百分比：%q", known)
	}
	// 已用段在纯文字下就是一串空格（它只有底色），所以"画了已用段"只能按位置判：
	// 读数右对齐，行尾不该再有多余空格——多出来一格就说明已用段没占满。
	if trimmed != known {
		t.Fatalf("读数应当贴到最后一列：%q", known)
	}
}

// 状态行：模型未知写 `?`、已知写模型 id；effort 缺失时写 `?`，
// 但**不许**因为未知就整段不画（那会让人以为这个界面不看 effort）。
func TestStatusRowModelUnknownAndKnown(t *testing.T) {
	a := newTestApp(120, 24)
	row := a.statusRow(currentTheme(), 0, 0).plain()
	if !strings.Contains(row, "? · ? effort") {
		t.Fatalf("模型与 effort 都未知时都应是 ?：%q", row)
	}
	a.model = "group/deepseek-v4.1-flash-1017"
	row = a.statusRow(currentTheme(), 0, 0).plain()
	if !strings.Contains(row, "group/deepseek-v4.1-flash-1017 · ? effort") {
		t.Fatalf("已知模型时应显示模型 id：%q", row)
	}
	if !strings.Contains(row, "工具 0") || !strings.Contains(row, "ctx ?") {
		t.Fatalf("状态行缺少工具数或上下文用量：%q", row)
	}
}

// 状态行右边是会话短 id；窄到放不下时**整段不画**，而不是把左边的状态挤掉。
func TestStatusRowDropsSessionIdWhenNarrow(t *testing.T) {
	a := newTestApp(120, 24)
	a.sid = "session-abcdef12"
	wide := a.statusRow(currentTheme(), 0, 0).plain()
	if !strings.Contains(wide, "会话 session-") {
		t.Fatalf("宽终端应显示会话 id：%q", wide)
	}
	a.scr.width = 40
	narrow := a.statusRow(currentTheme(), 0, 0).plain()
	if strings.Contains(narrow, "会话 ") {
		t.Fatalf("40 列放不下右边那组，应当整段不画：%q", narrow)
	}
	if !strings.Contains(narrow, "工具 ") {
		t.Fatalf("窄终端先牺牲身份、不该牺牲状态：%q", narrow)
	}
}

// 主题：深浅两档都要真的不一样，且都来自上游 theme.ts 的雾蓝档。
func TestThemeTokensDarkAndLight(t *testing.T) {
	dark, light := mistDark(), mistLight()
	if dark.name != "dark" || light.name != "light" {
		t.Fatalf("主题名不对：%q / %q", dark.name, light.name)
	}
	if dark.text == light.text || dark.accent == light.accent {
		t.Fatal("深浅两档的正文/品牌色必须不同")
	}
	// 上游 darkTheme.accent = #7DA1DE，lightTheme.accent = #3F6CC4。
	if got := dark.accent; got.r != 0x7D || got.g != 0xA1 || got.b != 0xDE {
		t.Fatalf("dark accent 应为 #7DA1DE，实际 %+v", got)
	}
	if got := light.accent; got.r != 0x3F || got.g != 0x6C || got.b != 0xC4 {
		t.Fatalf("light accent 应为 #3F6CC4，实际 %+v", got)
	}
	if !dark.accent.set || !light.accent.set {
		t.Fatal("颜色必须标成 set，否则不会输出转义")
	}
}

// 主题选择：显式 > 环境变量 > COLORFGBG > 默认 dark。
func TestResolveThemeName(t *testing.T) {
	t.Setenv("DSH_TUI_THEME", "")
	t.Setenv("COLORFGBG", "")
	if got := resolveThemeName(""); got != "dark" {
		t.Fatalf("没有线索时默认 dark，实际 %q", got)
	}
	if got := resolveThemeName("light"); got != "light" {
		t.Fatalf("显式 light 没生效：%q", got)
	}
	t.Setenv("DSH_TUI_THEME", "light")
	if got := resolveThemeName(""); got != "light" {
		t.Fatalf("DSH_TUI_THEME 没生效：%q", got)
	}
	t.Setenv("DSH_TUI_THEME", "")
	t.Setenv("COLORFGBG", "0;15")
	if got := resolveThemeName(""); got != "light" {
		t.Fatalf("COLORFGBG=0;15（浅底）应判成 light：%q", got)
	}
	t.Setenv("COLORFGBG", "15;0")
	if got := resolveThemeName(""); got != "dark" {
		t.Fatalf("COLORFGBG=15;0（深底）应判成 dark：%q", got)
	}
}

// itoa 是测试里拼尺寸串用的小工具（避免每个用例都 fmt.Sprintf）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// --dump-screen 的**接线**：不连宿主、不挂终端，按 --size 吐出入参尺寸的快照。
//
// 为什么值得单独测：flag 定义了却没接上，症状是"传了没反应"，不报错也不冒烟
// （scripts/flag-wired.py 只能证明变量被引用过）。这里跑的是真的 run()。
func TestRunDumpScreenWiring(t *testing.T) {
	for _, tc := range []struct {
		size      string
		wantLines int
	}{
		{"120x36", 36},
		{"80x24", 24},
	} {
		var out, errb strings.Builder
		if code := run([]string{"--dump-screen", "--size", tc.size}, &out, &errb); code != 0 {
			t.Fatalf("--dump-screen --size %s 退出码 %d，stderr=%q", tc.size, code, errb.String())
		}
		lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
		if len(lines) != tc.wantLines {
			t.Fatalf("--size %s 应输出 %d 行，实际 %d", tc.size, tc.wantLines, len(lines))
		}
		if strings.Contains(out.String(), "\x1b[") {
			t.Fatalf("--dump-screen 不该输出转义序列（无颜色快照）")
		}
	}
	// 尺寸写错要**明确报错**，而不是静默按默认尺寸画一屏。
	var out, errb strings.Builder
	if code := run([]string{"--dump-screen", "--size", "120"}, &out, &errb); code == 0 {
		t.Fatal("非法尺寸应当退出码非 0")
	}
	if !strings.Contains(errb.String(), "尺寸") {
		t.Fatalf("非法尺寸的错误信息没说清是尺寸：%q", errb.String())
	}
}

// --dump-splash 只吐 header（不含 footer 四行），且空会话下就是首屏那一块。
func TestRunDumpSplashWiring(t *testing.T) {
	t.Setenv("DSH_TUI_SPLASH_FONT", "bold")
	var out, errb strings.Builder
	if code := run([]string{"--dump-splash", "--size", "80x24"}, &out, &errb); code != 0 {
		t.Fatalf("--dump-splash 退出码 %d，stderr=%q", code, errb.String())
	}
	text := out.String()
	if !strings.Contains(text, "██▀▀▄▄") {
		t.Fatalf("--dump-splash 没画出大字：\n%s", text)
	}
	if strings.Contains(text, "───") {
		t.Fatalf("--dump-splash 不该带 footer 的分隔横线：\n%s", text)
	}
	if strings.Contains(text, "\x1b[") {
		t.Fatal("--dump-splash 默认不带颜色")
	}
}

// 运行中那一格：时长是**量出来的**真值，且回合结束后不许留在屏幕上。
//
// 这条钉的是一个具体的谎言：把"工作中 12s"存进 status，回合结束了它还挂着，
// 人回来看到"工作中"，其实早就答完了。所以时长只在渲染时按 start 现算。
func TestStatusRowRunningElapsedIsMeasured(t *testing.T) {
	clk := time.Date(2026, 11, 4, 20, 0, 0, 0, time.Local)
	a := newTestApp(120, 24)
	a.now = func() time.Time { return clk }
	a.busy = true
	a.start = clk.Add(-65 * time.Second)
	if row := a.statusRow(currentTheme(), 0, 0).plain(); !strings.Contains(row, "运行中 1m05s") {
		t.Fatalf("运行中应报出真实时长：%q", row)
	}
	// 不到 1 秒不报秒数（不写 0s）。
	a.start = clk.Add(-300 * time.Millisecond)
	if row := a.statusRow(currentTheme(), 0, 0).plain(); strings.Contains(row, "运行中 0s") {
		t.Fatalf("不到一秒不该报时长：%q", row)
	}
	// 回合结束：状态回到就绪，屏幕不许还挂着"运行中"。
	a.busy = false
	a.status = ""
	a.tickStatus()
	row := a.statusRow(currentTheme(), 0, 0).plain()
	if strings.Contains(row, "运行中") {
		t.Fatalf("回合结束后还挂着运行中：%q", row)
	}
	if !strings.Contains(row, "就绪") {
		t.Fatalf("空闲时状态行应说就绪：%q", row)
	}
}

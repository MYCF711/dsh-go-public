// dshtui：DeepSeek Harness 的终端界面（客户端），经 Transport 与宿主通信。
//
// 产品名是 **DSH TUI GO**，产物文件名逐字相同（`DSH TUI GO.exe`，见 brand.go
// 与 scripts/build.sh）。下面用法里的 `dshtui` 是命令行 token，脚本与文档照旧。
//
// 界面视觉按 MIT 上游 dsh-TUI 的**源码**移植（不是照截图手绘）：
// 点阵字体由 `scripts/import-splash.py` 从 `splashFonts.ts` 机械生成到
// `splash_gen.go`；鲸鱼精灵原样落在 `assets/whale_standard.txt`；
// 4 档窄终端阶梯、字距求解器、配色令牌逐项对照，见 docs/tui-visual.md。
//
// 用法：
//
//	dshtui --version                只打印版本（不连宿主、不启动界面）
//	dshtui --dump-screen            打印 120×36 整屏快照（无颜色，不连宿主）
//	dshtui --dump-screen --size 80x24   同一档的窄终端快照
//	dshtui --dump-splash            只打印首屏 header（无颜色）
//	dshtui --dump-splash --color    带真彩 ANSI
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 把"命令行 → 行为"写成可测的函数。
//
// 为什么要拆出来：`--version` 的判据是「只读版本号、**不启动界面**」——
// 这件事只有把 main 变成纯函数才验得了。真跑 main 会去连宿主，
// 在测试环境里那不是"失败"，是挂在那里等超时。stdout/stderr 也传进来，
// 测试可以逐字节断言输出。
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dshtui", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dump := fs.Bool("dump-splash", false, "打印开屏 header 并退出（无颜色，用于逐格比对）")
	dumpScreen := fs.Bool("dump-screen", false, "打印整屏快照并退出（无颜色；尺寸见 --size）")
	dumpTranscript := fs.String("dump-transcript", "", "读取 Transport 帧 JSON（数组或 session.history 的 frames）并打印消息层快照")
	expand := fs.Bool("expand", false, "配合 --dump-transcript：展开思考与长内容")
	showTools := fs.Bool("show-tool-results", false, "配合 --dump-transcript：展开工具结果")
	size := fs.String("size", "120x36", "配合 --dump-screen/--dump-splash/--dump-transcript 的终端尺寸，格式 列x行")
	color := fs.Bool("color", false, "开屏带真彩 ANSI")
	theme := fs.String("theme", "", "主题档：dark | light（缺省按 DSH_TUI_THEME / COLORFGBG 猜）")
	addr := fs.String("addr", "http://127.0.0.1:7788", "宿主 Transport 地址")
	token := fs.String("token", "", "宿主 /connect 的一次性 token")
	prompt := fs.String("send", "", "不带界面地发一条消息并打印收到的帧（验证链路）")
	// 回放一串"人本来会敲的键"，把每一轮的帧与屏幕快照写成一份 JSON。
	// 只给探测用（scripts/tui-session-e2e.py）：不给 --script 时行为与以前完全一致。
	script := fs.String("script", "", "回放一份脚本（JSON）并把帧与屏幕快照写到 --out")
	out := fs.String("out", "", "配合 --script：报告 JSON 的落点")
	// 只读版本：**不连宿主、不进 alt-screen**，脚本和任务管理器都能安全地调它。
	showVersion := fs.Bool("version", false, "只打印版本号并退出（不启动界面）")
	// flag 默认的 Usage 是一行 "Usage of dshtui"，连产品名都没有。
	fs.Usage = func() {
		usage(stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0 // -h/--help：已经打印过说明
		}
		return 2 // 与 flag 包默认的用法错误退出码一致
	}

	// 主题档在**任何**渲染之前定下来：它决定后面每一行的颜色。
	themeName = resolveThemeName(*theme)

	if *showVersion {
		fmt.Fprintln(stdout, versionLine())
		return 0
	}
	if *dumpScreen {
		text, err := dumpScreenSnapshot(*size, *color)
		if err != nil {
			fmt.Fprintln(stderr, "dshtui:", err)
			return 2
		}
		fmt.Fprint(stdout, text)
		return 0
	}
	if *dumpTranscript != "" {
		text, err := dumpTranscriptSnapshot(*dumpTranscript, *size, *color, *expand, *showTools)
		if err != nil {
			fmt.Fprintln(stderr, "dshtui:", err)
			return 2
		}
		fmt.Fprint(stdout, text)
		return 0
	}
	if *dump {
		text, err := dumpSplashOnly(*size, *color)
		if err != nil {
			fmt.Fprintln(stderr, "dshtui:", err)
			return 2
		}
		fmt.Fprint(stdout, text)
		return 0
	}
	// --script 走无界面回放（见 script.go）；它不改默认路径，只是多一个入口。
	if *script != "" {
		if *token == "" {
			fmt.Fprintln(stderr, "dshtui: 缺 --token（宿主 ready 行里的 token，一次性）")
			return 1
		}
		if err := runScript(*addr, *token, *script, *out); err != nil {
			fmt.Fprintln(stderr, "dshtui:", err)
			return 1
		}
		return 0
	}
	// 没给 --send 就是进全屏界面（这才是默认用法）
	if *prompt == "" {
		a, err := newApp(*addr, *token)
		if err != nil {
			fmt.Fprintln(stderr, "dshtui:", err)
			return 1
		}
		if err := a.run(); err != nil {
			fmt.Fprintln(stderr, "dshtui:", err)
			return 1
		}
		return 0
	}
	if err := sendAndTail(*addr, *token, *prompt); err != nil {
		fmt.Fprintln(stderr, "dshtui:", err)
		return 1
	}
	return 0
}

// parseSize 解析 `列x行`（也接受 `列X行` 与 `列*行`）。
//
// 为什么要 --size 而不是直接问终端：快照要**可比**——终端大小随时会变，
// 而"80 列这一档长什么样"必须能在任何机器上复现，否则断言跟着机器走。
func parseSize(size string) (int, int, error) {
	s := strings.ToLower(strings.TrimSpace(size))
	s = strings.NewReplacer("×", "x", "*", "x", ",", "x").Replace(s)
	parts := strings.Split(s, "x")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("尺寸要写成 列x行（例如 120x36），收到 %q", size)
	}
	w, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || w <= 0 {
		return 0, 0, fmt.Errorf("列数不合法：%q", parts[0])
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || h <= 0 {
		return 0, 0, fmt.Errorf("行数不合法：%q", parts[1])
	}
	return w, h, nil
}

// dumpScreenSnapshot 渲染整屏快照（无颜色、不连宿主）。
//
// 它走的是**同一条** layout()：快照与真屏同源，所以"快照里有、屏幕上没有"
// 这种分叉在结构上不存在。行尾空格裁掉（那只是补宽），空行保留。
func dumpScreenSnapshot(size string, withColor bool) (string, error) {
	w, h, err := parseSize(size)
	if err != nil {
		return "", err
	}
	a := &app{scr: &Screen{width: w, height: h}, cwd: dumpCWD(), color: withColor}
	return renderSnapshot(a.layout(), withColor), nil
}

// dumpTranscriptSnapshot 从调用方给的 Transport 帧文件排版消息层，不连宿主。
// 接受 session.history 的 {"frames":[…]} 以及裸 […]；除此以外不补消息、不编状态。
func dumpTranscriptSnapshot(path, size string, withColor, expand, showTools bool) (string, error) {
	w, h, err := parseSize(size)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读转储帧失败: %w", err)
	}
	frames, err := decodeTranscriptFrames(raw)
	if err != nil {
		return "", fmt.Errorf("转储帧格式不对: %w", err)
	}
	a := &app{
		scr: &Screen{width: w, height: h}, cwd: dumpCWD(), color: withColor,
		expandAll: expand, showToolResults: showTools,
	}
	for _, f := range frames {
		a.handleFrame(f)
	}
	return renderSnapshot(a.layout(), withColor), nil
}

func decodeTranscriptFrames(raw []byte) ([]ServerFrame, error) {
	var history struct {
		Frames []ServerFrame `json:"frames"`
	}
	if err := json.Unmarshal(raw, &history); err == nil && history.Frames != nil {
		return history.Frames, nil
	}
	var frames []ServerFrame
	if err := json.Unmarshal(raw, &frames); err != nil {
		return nil, err
	}
	return frames, nil
}

// dumpSplashOnly 只渲染首屏 header（正文区那一块），不带 footer。
func dumpSplashOnly(size string, withColor bool) (string, error) {
	w, h, err := parseSize(size)
	if err != nil {
		return "", err
	}
	a := &app{scr: &Screen{width: w, height: h}, cwd: dumpCWD(), color: withColor}
	rows := splashFirstScreen(a.splashInfo(), a.bodyHeight())
	return renderSnapshot(rows, withColor), nil
}

// dumpCWD 是快照里的工作目录：本进程的 cwd。
//
// 它是**真值**（不是编的占位），所以在不同机器上快照会不同——
// 断言不许钉这一行，只钉结构与其它字段（见 visuals_test.go 的注释）。
func dumpCWD() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// renderSnapshot 把若干行拼成快照输出：每行一处，行尾空格裁掉。
func renderSnapshot(rows []styledLine, withColor bool) string {
	var b strings.Builder
	for _, r := range rows {
		text := r.render(withColor)
		if !withColor {
			text = strings.TrimRight(text, " ")
		}
		b.WriteString(text)
		b.WriteString("\n")
	}
	return b.String()
}

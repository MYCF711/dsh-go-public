package main

// 主题令牌：Gentle Mist Blue（雾蓝）深浅两档。
//
// 逐项搬自上游 `src/theme.ts` 的 `darkTheme` / `lightTheme`（MIT），
// 只搬这个界面真正用到的槽位；键名保持上游语义（accent=品牌、activity=活动行、
// text=正文、inactive=次要文字/`dimColor` 的来源、subtle=更弱一档）。
//
// 上游的设计原则写在 theme.ts 开头，这里照录一句，因为它决定了配色不能随手改：
// 「两套真彩配色共用一个身份：雾蓝承担品牌、焦点与交互，正文永远是中性灰」。
// 换句话说：彩色是**有含义的**（品牌/焦点/成功/失败），正文不该是蓝的。
//
// 深浅色从哪来：显式 `--theme` / DSH_TUI_THEME 优先；未显式指定时，
// 交互启动先经 OSC 11 查询真实终端背景（见 osc.go），失败再沿用
// DSH_TUI_THEME / COLORFGBG / dark 的既有回退。探测与 readKeys 共用输入源，
// 不靠抢 stdin，也不会把终端响应写进用户草稿。

import (
	"fmt"
	"os"
	"strings"
)

// themeTokens 是一档主题用到的全部颜色。
type themeTokens struct {
	name string

	accent        color // 品牌色：词标、选中、焦点
	accentShimmer color // 品牌色的高光档
	activity      color // 活动行/欢迎语
	text          color // 正文
	inactive      color // 次要文字（上游 dimColor 的来源）
	subtle        color // 再弱一档（分界线、占位）
	success       color
	warning       color
	error         color
	userLabel     color // 用户消息的标记色（上游 userPromptLabel）
	promptBorder  color // 输入框边框
	toolName      color // 工具名

	barSystem    color // 上下文条 5 段（雾蓝阶梯，深浅共用）
	barPrompt    color
	barAssistant color
	barThinking  color
	barTools     color
	barFreeFill  color // 空余段的底色
	barFreeText  color // 空余段里读数文字
}

// mistDark 是 `theme.ts` 的 darkTheme（Gentle Mist Blue dark adaptation）。
func mistDark() themeTokens {
	return themeTokens{
		name:          "dark",
		accent:        hexColor("#7DA1DE"), // Accent Soft — mist brand blue
		accentShimmer: hexColor("#ABC2EC"), // Border Blue
		activity:      hexColor("#7DA1DE"),
		text:          hexColor("#E8E6E0"), // Warm off-white
		inactive:      hexColor("#8D95A6"), // Mist gray-blue — feeds dimColor
		subtle:        hexColor("#5E6673"),
		success:       hexColor("#82B89D"),
		warning:       hexColor("#D8B270"),
		error:         hexColor("#DA8A93"),
		userLabel:     hexColor("#D77757"), // 用户消息披 DeepSeek 档陶土红
		promptBorder:  hexColor("#55606F"),
		toolName:      hexColor("#56B6C2"),
		// MIST_BAR_RAMP：雾蓝阶梯，深浅两档共用
		barSystem:    hexColor("#22305F"),
		barPrompt:    hexColor("#2B3D78"),
		barAssistant: hexColor("#344A92"),
		barThinking:  hexColor("#4D6BFE"), // DeepSeek brand blue
		barTools:     hexColor("#5A7CFF"),
		// 深色下空余段用一条"比底色亮一点"的冷灰（StatusLine 的 deepseek 档）
		barFreeFill: hexColor("#2E3440"),
		barFreeText: hexColor("#8D95A6"),
	}
}

// mistLight 是 `theme.ts` 的 lightTheme（Gentle Mist Blue light — 严格原稿）。
func mistLight() themeTokens {
	return themeTokens{
		name:          "light",
		accent:        hexColor("#3F6CC4"), // Primary Blue — brand
		accentShimmer: hexColor("#5E88CC"),
		activity:      hexColor("#3F6CC4"),
		text:          hexColor("#343945"), // Ink
		inactive:      hexColor("#8991A0"), // Text-muted — feeds dimColor
		subtle:        hexColor("#A6ADBA"),
		success:       hexColor("#4E9675"),
		warning:       hexColor("#C08A3E"),
		error:         hexColor("#C65D6B"),
		userLabel:     hexColor("#C96442"),
		promptBorder:  hexColor("#ABC2EC"), // Border Blue
		toolName:      hexColor("#0F7A8A"),
		barSystem:     hexColor("#22305F"),
		barPrompt:     hexColor("#2B3D78"),
		barAssistant:  hexColor("#344A92"),
		barThinking:   hexColor("#4D6BFE"),
		barTools:      hexColor("#5A7CFF"),
		// 浅色下空余段是主题无关的浅灰（FREE_SEGMENT_FILL / _TEXT）
		barFreeFill: hexColor("#E8E8E8"),
		barFreeText: hexColor("#4A4A4A"),
	}
}

// themeName 是当前生效的档名（dark/light）。
//
// 进程级、启动时定一次：切主题要重画整屏，而本端第一阶段的主题不随会话变。
var themeName = "dark"

// themeForced 记录 resolveThemeName 是否看到了用户显式选择。
// main 在任何渲染前调用 resolveThemeName；newApp 把这一位带进交互启动，
// 因而合法的 --theme / DSH_TUI_THEME 不被 OSC 11 覆盖，也不必发探测。
var themeForced = false

// resolveThemeName 决定回退档：`--theme` > `DSH_TUI_THEME` > `COLORFGBG` > dark。
// 交互启动在未 forced 时会再用 OSC 11 的真实背景覆盖这个回退档。
//
// 为什么认 COLORFGBG：它是 xterm 起的惯例（`前景;背景` 的 ANSI 色号），
// Windows Terminal 与多数 Linux 终端都会设；背景色号 ≥ 8 或
// 0/8 之外的暗色号说明是深色底。判定粗糙，但**只影响配色**，
// 判错的后果是浅色终端上用深色主题（文字仍然可读）。
func resolveThemeName(forced string) string {
	themeForced = false
	if s := strings.ToLower(strings.TrimSpace(forced)); s == "dark" || s == "light" {
		themeForced = true
		return s
	}
	if s := strings.ToLower(strings.TrimSpace(os.Getenv("DSH_TUI_THEME"))); s == "dark" || s == "light" {
		themeForced = true
		return s
	}
	if fgbg := strings.TrimSpace(os.Getenv("COLORFGBG")); fgbg != "" {
		if i := strings.LastIndex(fgbg, ";"); i >= 0 {
			switch strings.TrimSpace(fgbg[i+1:]) {
			case "7", "9", "10", "11", "12", "13", "14", "15":
				return "light"
			}
		}
	}
	return "dark"
}

// currentTheme 返回当前档的令牌。每次现取，不缓存指针——
// 令牌表是值类型，取值比维护一份"已解析"状态更不容易错。
func currentTheme() themeTokens {
	if themeName == "light" {
		return mistLight()
	}
	return mistDark()
}

// hexColor 解析 `#RRGGBB`。解析失败返回"未设"——
// 颜色错一个字符时宁可不上色，也不要把整行刷成黑色。
func hexColor(h string) color {
	s := strings.TrimPrefix(strings.TrimSpace(h), "#")
	if len(s) != 6 {
		return color{}
	}
	var r, g, b uint8
	if _, err := fmt.Sscanf(s, "%02x%02x%02x", &r, &g, &b); err != nil {
		return color{}
	}
	return color{r: r, g: g, b: b, set: true}
}

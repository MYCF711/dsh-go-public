package main

// 「这个程序叫什么」只有一个出处。
//
// 以前名字是散在各处的：窗口标题写 `dsh-TUI (dsh-go)`、状态行写 `dsh-go`、
// 导出的 markdown 又写 `dsh-TUI(dsh-go)`，`--help` 连名字都没有。
// 三处不一样，人在任务管理器 / 终端标题栏 / 导出的文件里看到的就不是同一个应用。
//
// 产品名 = **DSH TUI GO**，产物文件名也是它（Windows 上 `DSH TUI GO.exe`，
// 见 scripts/build.sh）：文件名和产品名是同一个东西，脚本里要引用就整段加引号
// ——路径中间有空格。宿主是另一个程序「DSH GO」，不叫 TUI。

import (
	"fmt"
	"io"
)

// productName 是唯一的产品名常量。标题、状态行、--help、--version 都从这里取。
const productName = "DSH TUI GO"

// version / commit / buildTime 由 scripts/build.sh 用 `-ldflags -X` 注入。
//
// 默认值与宿主 `cmd/dshd/main.go` 的 version 对齐（两边各自是 main 包，不能互相
// import；build.sh 从宿主源码读出同一个号，同时注入两边）。直接 `go build` 时用
// 这里的默认值，所以它必须跟着宿主版本走。
var (
	version   = "0.2.2-go.1"
	commit    = "dev"
	buildTime = ""
)

// versionLine 是 `--version` 与"关于"类输出的唯一格式：
// 首行以产品名开头，后面跟版本号，有 commit/构建时间就补在括号里。
func versionLine() string {
	s := productName + " " + version
	if commit != "" && commit != "dev" {
		s += " (" + commit + ")"
	}
	if buildTime != "" {
		s += " " + buildTime
	}
	return s
}

// usage 是 `--help` 的开头。第一行必须是「产品名 + 版本」：
// 人第一眼要看的就是"我在跑哪个程序、哪个版本"。
func usage(w io.Writer) {
	fmt.Fprintf(w, "%s\n\nDeepSeek Harness 的终端界面（客户端），经 Transport 连宿主。\n\n用法:\n  dshtui [选项]\n\n选项:\n", versionLine())
}

// oscTitle 是设置终端窗口标题的 ANSI 序列（OSC 0: `ESC ] 0 ; 标题 BEL`）。
//
// 进 alt-screen 时设成产品名，退出时恢复原标题：否则关掉 TUI 之后那个窗口
// 会永远顶着「DSH TUI GO」，人分不清它还是不是活的。
func oscTitle(s string) string { return "\x1b]0;" + s + "\x07" }

//go:build windows

package main

// Windows 控制台的两个必需动作：打开 VT 处理、取窗口大小。
//
// 为什么不用 x/sys 或别的库：只要两个 syscall，引一个依赖不划算
// （本仓对二进制体积有硬上限，且这个客户端要能单文件跑）。

import (
	"syscall"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleMode             = kernel32.NewProc("GetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
	procGetConsoleTitleW           = kernel32.NewProc("GetConsoleTitleW")
	procSetConsoleTitleW           = kernel32.NewProc("SetConsoleTitleW")
)

const (
	enableVirtualTerminalProcessing = 0x0004
	enableProcessedOutput           = 0x0001
	enableWrapAtEOL                 = 0x0002
	handleStdout                    = -11
	handleStdin                     = -10
)

type coord struct{ X, Y int16 }
type smallRect struct{ Left, Top, Right, Bottom int16 }
type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

func getStdHandle(h int32) uintptr {
	h2, _, _ := kernel32.NewProc("GetStdHandle").Call(uintptr(h))
	return h2
}

// enableVT 打开 ANSI 转义支持。Win10 以前会失败——那就不开，
// 由调用方看到"全是转义字符"而不是黑屏，更容易判断。
func enableVT() bool {
	var mode uint32
	h := getStdHandle(handleStdout)
	if r, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false
	}
	// 同时打开 PROCESSED_OUTPUT 与 WRAP_AT_EOL，否则长行会被截断而不是折行
	mode |= enableVirtualTerminalProcessing | enableProcessedOutput | enableWrapAtEOL
	r, _, _ := procSetConsoleMode.Call(h, uintptr(mode))
	return r != 0
}

// termSize 返回终端可见区域的列数与行数。
func termSize() (int, int) {
	var info consoleScreenBufferInfo
	h := getStdHandle(handleStdout)
	r, _, _ := procGetConsoleScreenBufferInfo.Call(h, uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return 100, 30
	}
	w := int(info.Window.Right-info.Window.Left) + 1
	hgt := int(info.Window.Bottom-info.Window.Top) + 1
	if w <= 0 || hgt <= 0 {
		return 100, 30
	}
	return w, hgt
}

// setConsoleTitle 改控制台窗口标题。任务管理器的「窗口标题」列、
// `tasklist /v` 的 Window Title 读的就是它——所以标题只设 OSC 序列不够。
func setConsoleTitle(s string) {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return
	}
	procSetConsoleTitleW.Call(uintptr(unsafe.Pointer(p)))
}

// currentTitle 读当前控制台标题（进 alt-screen 前存下来，退出时还回去）。
// 读不到就返回空串——调用方据此跳过"恢复"，不去覆盖一个不知道的值。
func currentTitle() string {
	buf := make([]uint16, 1024)
	n, _, _ := procGetConsoleTitleW.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	if int(n) < len(buf) {
		buf = buf[:n]
	}
	return syscall.UTF16ToString(buf)
}

// 输入侧：默认控制台是"行缓冲 + 回显"，按键要回车才到程序里，
// 而且会自己回显一遍——全屏界面必须关掉这两样，自己画。
const (
	enableLineInput     = 0x0002
	enableEchoInput     = 0x0004
	enableVirtualTermIn = 0x0200
)

var savedInMode uint32

// rawInput 关掉行缓冲与回显。返回的 restore 必须被调用（否则退出后终端不回显，人以为坏了）。
func rawInput() func() {
	h := getStdHandle(handleStdin)
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode))); r == 0 {
		return func() {}
	}
	savedInMode = mode
	// 保留 VT 输入（方向键会变成转义序列），去掉行缓冲/回显
	newMode := (mode &^ uint32(enableLineInput|enableEchoInput)) | enableVirtualTermIn
	procSetConsoleMode.Call(h, uintptr(newMode))
	return func() { procSetConsoleMode.Call(h, uintptr(savedInMode)) }
}

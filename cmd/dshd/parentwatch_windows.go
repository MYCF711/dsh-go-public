//go:build windows

package main

// Windows 上"父进程还活着吗"不能靠 `os.FindProcess`：它对**任何** PID 都
// 成功返回（拿到的是个句柄壳子），`Signal(0)` 在这里也没有"探活"语义。
//
// 两条能用的路，这里都用：
//   - 启动时 `OpenProcess(SYNCHRONIZE|PROCESS_QUERY_LIMITED_INFORMATION)`
//     打开一个**常驻**句柄。句柄在，这个 PID 就不会被别人复用——所以后续探测
//     不会把"复用了同一个 PID 的陌生进程"误读成"父进程还活着"。
//   - 之后每次探测 `WaitForSingleObject(h, 0)`：WAIT_TIMEOUT = 还在跑，
//     WAIT_OBJECT_0 = 已退出。问不出来时退回 `GetExitCodeProcess` 判
//     STILL_ACTIVE（259）。
//
// 句柄拿不到（跨权限：父进程属于另一个用户/更高完整性级别）时退回
// "每次 OpenProcess + GetExitCodeProcess"。**打不开不等于死了**：那种情况
// 按活着处理——宁可漏停一个孤儿，也不要误杀一个正在干活的宿主。

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"unsafe"
)

var (
	pwKernel32            = syscall.NewLazyDLL("kernel32.dll")
	pwOpenProcess         = pwKernel32.NewProc("OpenProcess")
	pwGetExitCodeProcess  = pwKernel32.NewProc("GetExitCodeProcess")
	pwWaitForSingleObject = pwKernel32.NewProc("WaitForSingleObject")
	pwCloseHandle         = pwKernel32.NewProc("CloseHandle")
)

const (
	pwProcessQueryLimitedInformation = 0x1000
	pwSynchronize                    = 0x00100000
	// pwStillActive 就是 STILL_ACTIVE(259)：看得见的进程退出码不会是这个值。
	pwStillActive = 259
	pwWaitObject0 = 0x00000000
	pwWaitTimeout = 0x00000102
	pwWaitFailed  = 0xFFFFFFFF
)

// errParentAccessDenied 是"这个父进程打不开"（权限不足）的哨兵。
// 它不是"死了"：跨权限的父进程本来就不让开。
var errParentAccessDenied = errors.New("父进程打不开（权限不足）")

type windowsParentProbe struct {
	pid int
	// note 是这次判活方式的一句说明（空 = 用常驻同步句柄，最可靠那种）。
	note string

	mu     sync.Mutex
	handle uintptr // 0 = 没有常驻句柄，每次按 PID 重开
	closed bool
}

func openParentProbe(pid int) parentProbe {
	if pid <= 0 {
		return &parentProbeGone{note: fmt.Sprintf(
			"父进程 PID=%d 不是有效 PID，按已不存在处理", pid)}
	}
	h, err := openProcessHandle(pid, pwSynchronize|pwProcessQueryLimitedInformation)
	if err == nil {
		return &windowsParentProbe{pid: pid, handle: h}
	}
	if errors.Is(err, errParentAccessDenied) {
		return &windowsParentProbe{pid: pid, note: fmt.Sprintf(
			"父进程 %d 打不开（权限不足），只能靠退出码判存活（仍要连续 %d 次判死才停）",
			pid, defaultParentWatchMisses)}
	}
	return &parentProbeGone{note: fmt.Sprintf("父进程 %d 已经不存在（OpenProcess: %v）", pid, err)}
}

// openProcessHandle 是 OpenProcess 的薄封装。返回 0 句柄时 err 非空。
func openProcessHandle(pid int, access uint32) (uintptr, error) {
	h, _, err := pwOpenProcess.Call(uintptr(access), 0, uintptr(uint32(pid)))
	if h != 0 {
		return h, nil
	}
	if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		return 0, errParentAccessDenied
	}
	if err == nil {
		err = syscall.EINVAL
	}
	return 0, err
}

func (p *windowsParentProbe) Alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		// 看门狗已经收了：探测结果没人读，报"活着"最安全（不会因此误杀）。
		return true
	}
	if p.handle != 0 {
		switch r, _, _ := pwWaitForSingleObject.Call(p.handle, 0); r {
		case pwWaitObject0:
			return false // 进程对象已 signal = 已退出
		case pwWaitFailed:
			return exitCodeAlive(p.handle) // 句柄失效：退回退出码
		default: // WAIT_TIMEOUT
			return true
		}
	}
	h, err := openProcessHandle(p.pid, pwProcessQueryLimitedInformation)
	if err != nil {
		// 打不开：权限不足按活着（拿不准就不杀），其它按已不在。
		return errors.Is(err, errParentAccessDenied)
	}
	defer pwCloseHandle.Call(h)
	return exitCodeAlive(h)
}

// exitCodeAlive 用 GetExitCodeProcess 判 STILL_ACTIVE。
// 问不出来（调用失败）也按活着：这是那条"拿不准就别杀"的规则。
func exitCodeAlive(h uintptr) bool {
	var code uint32
	if r, _, _ := pwGetExitCodeProcess.Call(h, uintptr(unsafe.Pointer(&code))); r == 0 {
		return true
	}
	return code == pwStillActive
}

func (p *windowsParentProbe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	if p.handle != 0 {
		pwCloseHandle.Call(p.handle)
		p.handle = 0
	}
	return nil
}

func (p *windowsParentProbe) Note() string { return p.note }

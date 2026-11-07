//go:build !windows

package main

// 非 Windows 的判活：`kill(pid, 0)`。
//   - 成功 = 进程在；
//   - EPERM = 进程在，但不是我的（照样算活着）；
//   - ESRCH = 真没有这个进程。
//
// 和 Windows 那侧同一个口径：**拿不准就算活着**。

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

type unixParentProbe struct{ pid int }

func openParentProbe(pid int) parentProbe {
	if pid <= 0 {
		return &parentProbeGone{note: fmt.Sprintf(
			"父进程 PID=%d 不是有效 PID，按已不存在处理", pid)}
	}
	return &unixParentProbe{pid: pid}
}

func (p *unixParentProbe) Alive() bool {
	if p.pid == os.Getpid() {
		return true // 自己看自己：永远活着（防御性，正常不会走到）
	}
	err := syscall.Kill(p.pid, syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

func (p *unixParentProbe) Close() error { return nil }

func (p *unixParentProbe) Note() string { return "" }

//go:build windows

package bash

import (
	"golang.org/x/sys/windows"
)

// 仅清理测试临时 helper：先按创建代次+映像复核，再终止已打开的同一个句柄。
func cleanupOwnedTestProcess(r recordedPID) {
	if r.pid <= 0 || r.identity.CreationID == "" {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, uint32(r.pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	current, err := queryProcessIdentity(r.pid)
	if err != nil || validateProcessIdentity(r.identity, current) != nil {
		return
	}
	_ = windows.TerminateProcess(h, 1)
}

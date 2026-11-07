//go:build windows

package main

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

// verifyProcessImage 只查询调用方给出的精确 PID；不枚举镜像名、不调用 taskkill。
func verifyProcessImage(pid int, expectedImage string) error {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("打开 PID %d 失败: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return fmt.Errorf("查询 PID %d 完整映像路径失败: %w", pid, err)
	}
	actual, err := canonicalProcessImage(windows.UTF16ToString(buf[:n]))
	if err != nil {
		return err
	}
	if !strings.EqualFold(actual, expectedImage) {
		return fmt.Errorf("PID %d 映像=%q，期望=%q", pid, actual, expectedImage)
	}
	return nil
}

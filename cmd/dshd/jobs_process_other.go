//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func verifyProcessImage(pid int, expectedImage string) error {
	actual, err := os.Readlink(filepath.Join("/proc", fmt.Sprint(pid), "exe"))
	if err != nil {
		return fmt.Errorf("查询 PID %d 映像失败: %w", pid, err)
	}
	actual, err = canonicalProcessImage(actual)
	if err != nil {
		return err
	}
	if actual != expectedImage {
		return fmt.Errorf("PID %d 映像=%q，期望=%q", pid, actual, expectedImage)
	}
	return nil
}

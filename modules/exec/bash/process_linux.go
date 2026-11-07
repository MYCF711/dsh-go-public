//go:build linux

package bash

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"dshgo/internal/spi"
)

type unixProcessGroupTree struct{ pgid int }

func (t *unixProcessGroupTree) terminate() error {
	if t.pgid <= 0 {
		return nil
	}
	err := syscall.Kill(-t.pgid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return spi.ErrJobProcessExited
	}
	return err
}
func (t *unixProcessGroupTree) release() error                   { return nil }
func (t *unixProcessGroupTree) terminateOnClose(valid bool) bool { return valid }

func prepareProcessTree(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

func attachProcessTree(cmd *exec.Cmd) (*processControl, error) {
	identity, err := queryProcessIdentity(cmd.Process.Pid)
	if err != nil {
		return nil, err
	}
	if !identity.Active || identity.Image == "" || identity.CreationID == "" {
		return nil, fmt.Errorf("%w: 新 bash 元数据不完整: %+v", spi.ErrProcessTreeUnsafe, identity)
	}
	identity.ProcessTree = "unix-process-group"
	identity.ProcessGroupID = cmd.Process.Pid
	return &processControl{identity: identity, native: &unixProcessGroupTree{pgid: cmd.Process.Pid}}, nil
}

func queryProcessIdentity(pid int) (processIdentity, error) {
	identity := processIdentity{PID: pid}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return identity, spi.ErrJobProcessExited
		}
		return identity, err
	}
	// comm 可以包含空格和右括号；最后一个 ") " 后从 field 3 (state) 开始。
	text := string(stat)
	endComm := strings.LastIndex(text, ") ")
	if endComm < 0 {
		return identity, fmt.Errorf("/proc/%d/stat 格式异常", pid)
	}
	fields := strings.Fields(text[endComm+2:])
	if len(fields) <= 19 {
		return identity, fmt.Errorf("/proc/%d/stat 缺 starttime", pid)
	}
	if fields[0] == "Z" {
		identity.Active = false
	} else {
		identity.Active = true
	}
	identity.CreationID = fields[19] // field 22，单位与系统 CLK_TCK 一致，仅作同机代次比较
	if identity.Active {
		identity.Image, err = os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if err != nil {
			return identity, err
		}
		identity.Image = strings.TrimSuffix(identity.Image, " (deleted)")
	}
	return identity, nil
}

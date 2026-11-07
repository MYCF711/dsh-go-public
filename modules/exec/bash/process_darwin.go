//go:build darwin

package bash

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"dshgo/internal/spi"
	"golang.org/x/sys/unix"
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
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) {
			return identity, spi.ErrJobProcessExited
		}
		return identity, err
	}
	if proc.Proc.P_pid != int32(pid) {
		return identity, spi.ErrJobProcessExited
	}
	// XNU sys/proc.h: SZOMB=5；不能把它当成可停止的活进程。
	identity.Active = proc.Proc.P_stat != 5
	identity.CreationID = strconv.FormatInt(proc.Proc.P_starttime.Sec, 10) + ":" +
		strconv.FormatInt(int64(proc.Proc.P_starttime.Usec), 10)
	if identity.Active {
		out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
		if err != nil {
			return identity, err
		}
		identity.Image = strings.TrimSpace(string(out))
		if identity.Image == "" {
			return identity, fmt.Errorf("ps 未返回 PID %d 的映像", pid)
		}
	}
	return identity, nil
}

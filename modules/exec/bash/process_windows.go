//go:build windows

package bash

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"unsafe"

	"dshgo/internal/spi"
	"golang.org/x/sys/windows"
)

const (
	jobObjectLimitKillOnJobClose = 0x00002000
	stillActive                  = 259
)

type windowsJobTree struct {
	job windows.Handle
}

func (t *windowsJobTree) terminate() error {
	if t.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(t.job, 1)
}

func (t *windowsJobTree) release() error {
	if t.job == 0 {
		return nil
	}
	h := t.job
	t.job = 0
	return windows.CloseHandle(h)
}

func (t *windowsJobTree) terminateOnClose(bool) bool { return true }

func prepareProcessTree(*exec.Cmd) error { return nil }

// attachProcessTree 在 startSession 返回、任何用户命令写入 stdin 之前调用。
// bash 以 --noprofile --norc -s 启动，因此此窗口内不会执行用户命令或派生用户子进程。
func attachProcessTree(cmd *exec.Cmd) (*processControl, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	native := &windowsJobTree{job: job}
	ok := false
	defer func() {
		if !ok {
			_ = native.terminate() // 若已 assign，KILL_ON_JOB_CLOSE 也只收这棵新树
			_ = native.release()
		}
	}()
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	access := uint32(windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.PROCESS_SET_QUOTA | windows.PROCESS_TERMINATE)
	h, err := windows.OpenProcess(access, false, uint32(cmd.Process.Pid))
	if err != nil {
		return nil, fmt.Errorf("OpenProcess(new bash): %w", err)
	}
	err = windows.AssignProcessToJobObject(job, h)
	_ = windows.CloseHandle(h)
	if err != nil {
		return nil, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	identity, err := queryProcessIdentity(cmd.Process.Pid)
	if err != nil {
		return nil, err
	}
	if !identity.Active || identity.Image == "" || identity.CreationID == "" {
		return nil, fmt.Errorf("%w: 新 bash 元数据不完整: %+v", spi.ErrProcessTreeUnsafe, identity)
	}
	identity.ProcessTree = "windows-job-object"
	ok = true
	return &processControl{identity: identity, native: native}, nil
}

func queryProcessImage(h windows.Handle) (string, error) {
	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:size]), nil
}

func queryProcessIdentity(pid int) (processIdentity, error) {
	identity := processIdentity{PID: pid}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_NOT_FOUND) || errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return identity, spi.ErrJobProcessExited
		}
		return identity, err
	}
	defer windows.CloseHandle(h)
	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return identity, err
	}
	identity.Active = exitCode == stillActive
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return identity, err
	}
	identity.CreationID = strconv.FormatUint(uint64(creation.HighDateTime)<<32|uint64(creation.LowDateTime), 16)
	if identity.Active {
		identity.Image, err = queryProcessImage(h)
		if err != nil {
			return identity, err
		}
	}
	return identity, nil
}

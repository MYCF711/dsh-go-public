//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

type mcpProcessTree struct{ job windows.Handle }

func attachMCPProcessTree(cmd *exec.Cmd) (*mcpProcessTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("MCP CreateJobObject: %w", err)
	}
	tree := &mcpProcessTree{job: job}
	ok := false
	defer func() {
		if !ok {
			_ = windows.TerminateJobObject(job, 1)
			_ = windows.CloseHandle(job)
		}
	}()
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = 0x00002000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return nil, fmt.Errorf("MCP SetInformationJobObject: %w", err)
	}
	access := uint32(windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.PROCESS_SET_QUOTA | windows.PROCESS_TERMINATE)
	handle, err := windows.OpenProcess(access, false, uint32(cmd.Process.Pid))
	if err != nil {
		return nil, fmt.Errorf("MCP OpenProcess: %w", err)
	}
	err = windows.AssignProcessToJobObject(job, handle)
	_ = windows.CloseHandle(handle)
	if err != nil {
		return nil, fmt.Errorf("MCP AssignProcessToJobObject: %w", err)
	}
	ok = true
	return tree, nil
}

func (t *mcpProcessTree) Stop() error {
	if t == nil || t.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(t.job, 1)
}

func (t *mcpProcessTree) Close() error {
	if t == nil || t.job == 0 {
		return nil
	}
	handle := t.job
	t.job = 0
	return windows.CloseHandle(handle)
}

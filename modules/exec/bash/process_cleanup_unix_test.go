//go:build linux || darwin

package bash

import "syscall"

func cleanupOwnedTestProcess(r recordedPID) {
	if r.pid <= 0 || r.identity.CreationID == "" {
		return
	}
	current, err := queryProcessIdentity(r.pid)
	if err != nil || validateProcessIdentity(r.identity, current) != nil {
		return
	}
	_ = syscall.Kill(r.pid, syscall.SIGKILL)
}

//go:build darwin

package service

import (
	"errors"

	"golang.org/x/sys/unix"
)

func ProcessParentPID(pid int) (int, error) {
	if pid <= 1 {
		return 0, errors.New("valid process PID is required")
	}
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	parent := int(process.Eproc.Ppid)
	if parent <= 0 {
		return 0, errors.New("process parent PID is unavailable")
	}
	return parent, nil
}

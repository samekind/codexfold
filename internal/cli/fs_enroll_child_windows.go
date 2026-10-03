//go:build windows

package cli

import (
	"golang.org/x/sys/windows"
	"os/exec"
	"syscall"
)

func configureEnrollmentChild(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW | windows.BELOW_NORMAL_PRIORITY_CLASS}
}

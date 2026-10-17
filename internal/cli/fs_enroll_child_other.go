//go:build !windows

package cli

import "os/exec"

func configureEnrollmentChild(*exec.Cmd) {}

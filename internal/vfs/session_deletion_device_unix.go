//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package vfs

import (
	"errors"
	"os"
	"syscall"
)

func sessionDeletionFileDevice(info os.FileInfo) (uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, errors.New("session deletion file has no Unix device identity")
	}
	return uint64(stat.Dev), nil
}

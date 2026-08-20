//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package vfs

import "os"

func sessionDeletionFileDevice(os.FileInfo) (uint64, error) {
	return 0, nil
}

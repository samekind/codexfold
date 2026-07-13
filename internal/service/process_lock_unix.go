//go:build !windows

package service

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryLockProcessFile(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

func unlockProcessFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

//go:build windows

package vfs

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func tryLockWriterFile(file *os.File) (bool, error) {
	overlapped := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

func unlockWriterFile(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, new(windows.Overlapped))
}

func cleanupStaleWriterLease(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	locked, err := tryLockWriterFile(file)
	if err != nil {
		_ = file.Close()
		return err
	}
	if !locked {
		return file.Close()
	}
	if err := unlockWriterFile(file); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

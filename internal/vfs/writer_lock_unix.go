//go:build !windows

package vfs

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func acquireInitialWriterLease(staging, _ string) (*os.File, error) {
	return acquireWriterLease(filepath.Join(staging, "writer.lease"))
}

func openWriterLeaseFile(path string, create bool) (*os.File, error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	return os.OpenFile(path, flags, 0o600)
}

func tryLockWriterFile(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

func unlockWriterFile(file *os.File) error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }

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

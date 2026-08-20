package vfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// TryAcquireWriterLeaseGuardAtPath reserves an existing writer lease without
// changing its diagnostic payload. It is used when recovery metadata has
// already moved out of the canonical fs/sessions directory.
func TryAcquireWriterLeaseGuardAtPath(leasePath string) (*WriterLeaseGuard, bool, error) {
	leasePath = filepath.Clean(leasePath)
	if !filepath.IsAbs(leasePath) || filepath.Base(leasePath) != "writer.lease" {
		return nil, false, errors.New("absolute writer lease path is required")
	}
	before, err := os.Lstat(leasePath)
	if err != nil {
		return nil, false, fmt.Errorf("inspect writer lease guard: %w", err)
	}
	if !before.Mode().IsRegular() {
		return nil, false, errors.New("writer lease guard is not a regular file")
	}
	file, err := os.OpenFile(leasePath, os.O_RDWR, 0)
	if err != nil {
		return nil, false, fmt.Errorf("open writer lease guard: %w", err)
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, false, fmt.Errorf("stat writer lease guard: %w", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, false, errors.New("writer lease guard changed while it was opened")
	}
	locked, err := tryLockWriterFile(file)
	if err != nil {
		_ = file.Close()
		return nil, false, err
	}
	if !locked {
		_ = file.Close()
		return nil, false, nil
	}
	after, err := os.Lstat(leasePath)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		closeErr := errors.Join(unlockWriterFile(file), file.Close())
		if err == nil {
			err = errors.New("writer lease guard changed after it was locked")
		}
		return nil, false, errors.Join(err, closeErr)
	}
	return &WriterLeaseGuard{file: file}, true, nil
}

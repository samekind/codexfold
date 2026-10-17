//go:build windows

package vfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Sharing delete access lets initial publication unlink the temporary lease
// name while retaining its lock through the canonical hard link.
func openWriterLeaseFile(path string, create bool) (*os.File, error) {
	if filepath.Base(path) != "writer.lease" {
		return openSharedWriterLeaseFile(path, create)
	}
	// Published sessions can be moved into retired storage while their writer
	// lease is held. Open through an external alias so that the lock does not
	// pin the containing directory on Windows.
	file, err := openSharedWriterLeaseFile(path, create)
	if err != nil {
		return nil, err
	}
	before, statErr := file.Stat()
	closeErr := file.Close()
	if err := errors.Join(statErr, closeErr); err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("writer lease is not a regular file")
	}
	aliasFile, err := os.CreateTemp(filepath.Dir(filepath.Dir(path)), ".codexfold-writer-*.lease")
	if err != nil {
		return nil, err
	}
	alias := aliasFile.Name()
	if err := aliasFile.Close(); err != nil {
		_ = os.Remove(alias)
		return nil, err
	}
	if err := os.Remove(alias); err != nil {
		return nil, err
	}
	if err := os.Link(path, alias); err != nil {
		return nil, fmt.Errorf("link published writer lease: %w", err)
	}
	defer os.Remove(alias)
	file, err = openSharedWriterLeaseFile(alias, false)
	if err != nil {
		return nil, err
	}
	opened, statErr := file.Stat()
	after, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		_ = file.Close()
		return nil, errors.Join(statErr, pathErr, errors.New("writer lease changed while opening its external alias"))
	}
	if err := os.Remove(alias); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openSharedWriterLeaseFile(path string, create bool) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	disposition := uint32(windows.OPEN_EXISTING)
	if create {
		disposition = windows.OPEN_ALWAYS
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, disposition, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

// Windows cannot rename a directory containing an open child handle, even
// with FILE_SHARE_DELETE. Open the lease through a name outside staging, then
// hard-link it into staging before publishing. Both names identify the same
// locked file, so no writer can race the directory publication. The caller
// holds the session initialization lease while using this temporary name.
func acquireInitialWriterLease(staging, sessionID string) (*os.File, error) {
	external := filepath.Join(filepath.Dir(staging), initialSessionLockName(sessionID)+".writer")
	lease, err := acquireWriterLease(external)
	if err != nil {
		return nil, err
	}
	if err := os.Link(external, filepath.Join(staging, "writer.lease")); err != nil {
		_ = unlockWriterFile(lease)
		_ = lease.Close()
		_ = os.Remove(external)
		return nil, fmt.Errorf("link initial writer lease (store requires hard-link support): %w", err)
	}
	if err := os.Remove(external); err != nil {
		_ = unlockWriterFile(lease)
		_ = lease.Close()
		return nil, fmt.Errorf("unlink temporary writer lease: %w", err)
	}
	return lease, nil
}

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
	file, err := openWriterLeaseFile(path, true)
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

package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrOperationLockHeld = errors.New("operation lock is already held")

// OperationLock serializes mutations to one store-owned resource.
type OperationLock struct {
	file      *os.File
	root      *os.Root
	closeOnce sync.Once
	closeErr  error
}

func AcquireOperationLock(storeDir string, name string) (*OperationLock, error) {
	if storeDir == "" || name == "" || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00") {
		return nil, errors.New("store directory and safe operation lock name are required")
	}
	storeDir = filepath.Clean(storeDir)
	storeInfo, err := os.Lstat(storeDir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(storeDir, 0o700); err != nil {
			return nil, fmt.Errorf("create operation lock store root: %w", err)
		}
		storeInfo, err = os.Lstat(storeDir)
	}
	if err != nil {
		return nil, err
	}
	if !storeInfo.IsDir() || storeInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("operation lock store root is not a plain directory")
	}
	root, err := os.OpenRoot(storeDir)
	if err != nil {
		return nil, err
	}
	closeRoot := true
	defer func() {
		if closeRoot {
			_ = root.Close()
		}
	}()
	openedRoot, err := root.Stat(".")
	if err != nil || !openedRoot.IsDir() || !os.SameFile(storeInfo, openedRoot) {
		if err == nil {
			err = errors.New("operation lock store root changed while opening")
		}
		return nil, err
	}
	currentStore, err := os.Lstat(storeDir)
	if err != nil || !currentStore.IsDir() || currentStore.Mode()&os.ModeSymlink != 0 || !os.SameFile(storeInfo, currentStore) {
		if err == nil {
			err = errors.New("operation lock store root changed while opening")
		}
		return nil, err
	}
	if err := root.Mkdir("locks", 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create operation lock directory: %w", err)
	}
	locksInfo, err := root.Lstat("locks")
	if err != nil || !locksInfo.IsDir() || locksInfo.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("operation lock directory is not a plain directory")
		}
		return nil, err
	}
	lockPath := filepath.Join("locks", name+".lock")
	before, beforeErr := root.Lstat(lockPath)
	if beforeErr != nil && !errors.Is(beforeErr, os.ErrNotExist) {
		return nil, beforeErr
	}
	if beforeErr == nil && !before.Mode().IsRegular() {
		return nil, errors.New("operation lock path is not a regular file")
	}
	file, err := root.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open operation lock: %w", err)
	}
	opened, err := file.Stat()
	after, afterErr := root.Lstat(lockPath)
	if err != nil || afterErr != nil || !opened.Mode().IsRegular() || !after.Mode().IsRegular() || !os.SameFile(opened, after) || (beforeErr == nil && !os.SameFile(before, opened)) {
		_ = file.Close()
		if err == nil {
			err = afterErr
		}
		if err == nil {
			err = errors.New("operation lock path changed while opening")
		}
		return nil, err
	}
	locked, err := tryLockLease(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock operation: %w", err)
	}
	if !locked {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %q", ErrOperationLockHeld, name)
	}
	afterLock, err := root.Lstat(lockPath)
	if err != nil || !afterLock.Mode().IsRegular() || !os.SameFile(opened, afterLock) {
		closeErr := errors.Join(unlockLease(file), file.Close())
		if err == nil {
			err = errors.New("operation lock path changed after locking")
		}
		return nil, errors.Join(err, closeErr)
	}
	closeRoot = false
	return &OperationLock{file: file, root: root}, nil
}

// AcquireOperationLockWithin waits up to timeout for a store-owned mutation
// lock. A timeout returns ErrOperationLockHeld so callers can surface a
// retryable health incident instead of hanging indefinitely.
func AcquireOperationLockWithin(storeDir string, name string, timeout time.Duration) (*OperationLock, error) {
	if timeout <= 0 {
		return AcquireOperationLock(storeDir, name)
	}
	deadline := time.Now().Add(timeout)
	for {
		lock, err := AcquireOperationLock(storeDir, name)
		if err == nil || !errors.Is(err, ErrOperationLockHeld) {
			return lock, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, err
		}
		pause := 10 * time.Millisecond
		if remaining < pause {
			pause = remaining
		}
		time.Sleep(pause)
	}
}

func (l *OperationLock) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		if l.file == nil {
			return
		}
		l.closeErr = errors.Join(unlockLease(l.file), l.file.Close())
		l.file = nil
		if l.root != nil {
			l.closeErr = errors.Join(l.closeErr, l.root.Close())
			l.root = nil
		}
	})
	return l.closeErr
}

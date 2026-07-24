package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// OperationLock serializes mutations to one store-owned resource.
type OperationLock struct {
	file      *os.File
	closeOnce sync.Once
	closeErr  error
}

func AcquireOperationLock(storeDir string, name string) (*OperationLock, error) {
	if storeDir == "" || name == "" || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00") {
		return nil, errors.New("store directory and safe operation lock name are required")
	}
	directory := filepath.Join(filepath.Clean(storeDir), "locks")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create operation lock directory: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(directory, name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open operation lock: %w", err)
	}
	locked, err := tryLockLease(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock operation: %w", err)
	}
	if !locked {
		_ = file.Close()
		return nil, fmt.Errorf("operation lock %q is already held", name)
	}
	return &OperationLock{file: file}, nil
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
	})
	return l.closeErr
}

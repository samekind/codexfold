package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type ProcessLock struct {
	file *os.File
}

func AcquireProcessLock(path string) (*ProcessLock, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("absolute process lock path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	locked, err := tryLockProcessFile(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !locked {
		_ = file.Close()
		return nil, errors.New("filesystem service process lock is already held")
	}
	if err := file.Truncate(0); err != nil {
		_ = unlockProcessFile(file)
		_ = file.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		_ = unlockProcessFile(file)
		_ = file.Close()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		_ = unlockProcessFile(file)
		_ = file.Close()
		return nil, err
	}
	return &ProcessLock{file: file}, nil
}

func (l *ProcessLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	unlockErr := unlockProcessFile(file)
	closeErr := file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type ProcessLock struct {
	file *os.File
}

type ProcessLockStatus struct {
	Held bool
	PID  int
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

func InspectProcessLock(path string) (ProcessLockStatus, error) {
	if !filepath.IsAbs(path) {
		return ProcessLockStatus{}, errors.New("absolute process lock path is required")
	}
	file, err := os.OpenFile(filepath.Clean(path), os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return ProcessLockStatus{}, nil
	}
	if err != nil {
		return ProcessLockStatus{}, err
	}
	defer file.Close()
	locked, err := tryLockProcessFile(file)
	if err != nil {
		return ProcessLockStatus{}, err
	}
	if locked {
		return ProcessLockStatus{}, unlockProcessFile(file)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return ProcessLockStatus{Held: true}, err
	}
	value, err := io.ReadAll(io.LimitReader(file, 64))
	if err != nil {
		return ProcessLockStatus{Held: true}, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(value)))
	if err != nil || pid <= 1 {
		return ProcessLockStatus{Held: true}, errors.New("held process lock has an invalid owner PID")
	}
	return ProcessLockStatus{Held: true, PID: pid}, nil
}

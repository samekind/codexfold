package storage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Lease struct {
	file      *os.File
	path      string
	closeOnce sync.Once
	closeErr  error
}

func AcquireLease(directory string, label string) (*Lease, error) {
	if directory == "" || label == "" || filepath.Base(label) != label || strings.ContainsAny(label, "/\\\x00") {
		return nil, errors.New("lease directory and safe label are required")
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create lease directory: %w", err)
		}
		info, statErr := os.Lstat(directory)
		if statErr != nil {
			return nil, fmt.Errorf("inspect lease directory: %w", statErr)
		}
		if !info.IsDir() {
			return nil, errors.New("lease path is not a directory")
		}
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, fmt.Sprintf(".lease-%s-%d-%s", label, os.Getpid(), hex.EncodeToString(random)))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create lease file: %w", err)
	}
	locked, err := tryLockLease(file)
	if err != nil || !locked {
		_ = file.Close()
		_ = os.Remove(path)
		if err == nil {
			err = errors.New("new lease file could not be locked")
		}
		return nil, err
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		_ = unlockLease(file)
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := file.Sync(); err != nil {
		_ = unlockLease(file)
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &Lease{file: file, path: path}, nil
}

func (l *Lease) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		var errs []error
		if l.file != nil {
			if err := unlockLease(l.file); err != nil {
				errs = append(errs, err)
			}
			if err := l.file.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
		l.closeErr = errors.Join(errs...)
	})
	return l.closeErr
}

func DirectoryHasActiveLease(directory string, cleanStale bool) (bool, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	active := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), ".lease-") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		locked, lockErr := tryLockLease(file)
		if lockErr != nil {
			_ = file.Close()
			return false, lockErr
		}
		if !locked {
			active = true
			_ = file.Close()
			continue
		}
		unlockErr := unlockLease(file)
		closeErr := file.Close()
		if unlockErr != nil || closeErr != nil {
			return false, errors.Join(unlockErr, closeErr)
		}
		if cleanStale {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
		}
	}
	return active, nil
}

func FileHasActiveLock(path string) (bool, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	locked, lockErr := tryLockLease(file)
	if lockErr != nil {
		_ = file.Close()
		return false, lockErr
	}
	if !locked {
		_ = file.Close()
		return true, nil
	}
	unlockErr := unlockLease(file)
	closeErr := file.Close()
	return false, errors.Join(unlockErr, closeErr)
}

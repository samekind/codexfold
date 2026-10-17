package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var errForegroundAccess = errors.New("maintenance yielded to foreground access")

// Maintenance makes a background operation interruptible by a foreground file
// open. Cancellation never releases a writer lock on its own: the operation
// unwinds normally and releases its locks before closing this registration.
type Maintenance struct {
	ctx       context.Context
	cancel    context.CancelCauseFunc
	lease     *Lease
	request   string
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func maintenanceDirectory(store string) string {
	return filepath.Join(filepath.Clean(store), "fs", "maintenance")
}

func BeginMaintenance(ctx context.Context, store string) (*Maintenance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(store) {
		return nil, errors.New("absolute maintenance store path is required")
	}
	directory := maintenanceDirectory(store)
	if err := os.MkdirAll(filepath.Dir(directory), 0o700); err != nil {
		return nil, err
	}
	lease, err := AcquireLease(directory, "maintenance")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	m := &Maintenance{ctx: ctx, cancel: cancel, lease: lease, request: lease.path + ".yield", done: make(chan struct{})}
	go func() {
		defer close(m.done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if info, err := os.Lstat(m.request); err == nil && info.Mode().IsRegular() {
					cancel(errForegroundAccess)
					return
				}
			}
		}
	}()
	return m, nil
}

func (m *Maintenance) Context() context.Context { return m.ctx }

func (m *Maintenance) YieldError(err error) error {
	if m != nil && err != nil && errors.Is(context.Cause(m.ctx), errForegroundAccess) {
		return fmt.Errorf("%w: %w", ErrManagedSessionBusy, errForegroundAccess)
	}
	return err
}

func (m *Maintenance) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		m.cancel(context.Canceled)
		<-m.done
		m.closeErr = m.lease.Close()
		if err := os.Remove(m.request); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.closeErr = errors.Join(m.closeErr, err)
		}
	})
	return m.closeErr
}

// RequestForegroundAccess only signals live, lock-proven maintenance owners.
// A leftover registration from a crashed process cannot turn a real second
// foreground writer into a maintenance retry or authorize releasing any lock.
func RequestForegroundAccess(store string) (bool, error) {
	directory := maintenanceDirectory(store)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	requested := false
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".lease-maintenance-") || strings.HasSuffix(entry.Name(), ".yield") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			if err != nil {
				return requested, err
			}
			continue
		}
		active, err := FileHasActiveLock(path)
		if err != nil {
			return requested, err
		}
		if !active {
			continue
		}
		request, err := os.OpenFile(path+".yield", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			if info, statErr := os.Lstat(path + ".yield"); statErr == nil && info.Mode().IsRegular() {
				requested = true
			}
			continue
		}
		if err != nil {
			return requested, err
		}
		if err := request.Close(); err != nil {
			return requested, err
		}
		requested = true
	}
	return requested, nil
}

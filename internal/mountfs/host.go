package mountfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

var ErrPrerequisite = errors.New("FUSE host prerequisite is unavailable in this build")

// HostBackend is the I/O boundary between a resident mount and its storage
// engine. Filesystem remains the default backend on every platform.
type HostBackend interface {
	Getattr(string) (Attr, syscall.Errno)
	ReadDir(string) ([]string, syscall.Errno)
	Open(string, int) (uint64, syscall.Errno)
	Read(uint64, []byte, int64) (int, syscall.Errno)
	Write(uint64, []byte, int64) (int, syscall.Errno)
	TruncatePath(string, int64) syscall.Errno
	Truncate(uint64, int64) syscall.Errno
	Flush(uint64) syscall.Errno
	Fsync(uint64) syscall.Errno
	Release(uint64) syscall.Errno
	Mkdir(string, uint32) syscall.Errno
	Rename(string, string) syscall.Errno
	Unlink(string) syscall.Errno
}

type HostMetadataBackend interface {
	Metadata(string, string, uint32, uint32, time.Time, time.Time) syscall.Errno
}

type HostOptions struct {
	MountPoint        string
	StorageRoot       string
	NamespaceRoot     string
	Filesystem        *Filesystem
	Backend           HostBackend
	BackendBuild      func() string
	BackendHealthy    func() bool
	Foreground        bool
	OperationRecorder func(string)
	BuildSHA256       string
	Activity          *IOActivityCounter
}

func Mount(ctx context.Context, options HostOptions) error {
	if options.MountPoint == "" || (options.Filesystem == nil && options.Backend == nil) {
		return errors.New("mount point and filesystem are required")
	}
	if !Available() {
		return ErrPrerequisite
	}
	if err := prepareMountPoint(options.MountPoint); err != nil {
		return err
	}
	return mountHost(ctx, options)
}

func prepareMountPoint(path string) error {
	if err := recoverStaleMount(path); err != nil {
		return fmt.Errorf("recover stale mount: %w", err)
	}
	if runtime.GOOS == "windows" {
		return prepareWindowsMountPoint(path)
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create mount backing directory: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("inspect mount backing directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("mount backing path must not be a symlink")
	}
	if !info.IsDir() {
		return errors.New("mount backing path is not a directory")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("inspect mount backing contents: %w", err)
	}
	if len(entries) != 0 {
		return errors.New("mount backing directory must be empty")
	}
	if err := os.Chmod(path, 0o500); err != nil {
		return fmt.Errorf("seal mount backing directory: %w", err)
	}
	return nil
}

// WinFsp creates its own directory mount point. Leaving the path absent also
// prevents an unmounted filesystem from silently accepting ordinary writes.
func prepareWindowsMountPoint(path string) error {
	path = filepath.Clean(path)
	if handled, err := prepareWindowsDriveMount(path); handled {
		return err
	}
	if !filepath.IsAbs(path) || filepath.Dir(path) == path {
		return errors.New("Windows mount point must be an absolute non-root directory path")
	}
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("mount backing path must be an ordinary directory")
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return fmt.Errorf("inspect mount backing contents: %w", err)
		}
		if len(entries) != 0 {
			return errors.New("mount backing directory must be empty")
		}
		// Remove only this empty directory; never recursively remove contents.
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("clear empty WinFsp mount point: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect mount backing directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create mount parent directory: %w", err)
	}
	return nil
}

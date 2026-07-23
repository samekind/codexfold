package mountfs

import (
	"context"
	"errors"
	"fmt"
	"os"
)

var ErrPrerequisite = errors.New("FUSE host prerequisite is unavailable in this build")

type HostOptions struct {
	MountPoint        string
	Filesystem        *Filesystem
	Foreground        bool
	OperationRecorder func(string)
	BuildSHA256       string
}

func Mount(ctx context.Context, options HostOptions) error {
	if options.MountPoint == "" || options.Filesystem == nil {
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

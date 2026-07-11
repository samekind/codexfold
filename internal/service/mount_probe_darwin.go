//go:build darwin

package service

import (
	"errors"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func defaultMountProbe(path string) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return err
	}
	mountedAt := unix.ByteSliceToString(stat.Mntonname[:])
	filesystem := strings.ToLower(unix.ByteSliceToString(stat.Fstypename[:]))
	if filepath.Clean(mountedAt) != filepath.Clean(path) {
		return errors.New("path is not a mount root")
	}
	if !strings.Contains(filesystem, "fuse") {
		return errors.New("mount root is not backed by FUSE")
	}
	return nil
}

//go:build darwin

package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jstar0/codexfold/internal/mountid"
	"golang.org/x/sys/unix"
)

func defaultMountProbe(path string) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return err
	}
	mountedAt := unix.ByteSliceToString(stat.Mntonname[:])
	filesystem := strings.ToLower(unix.ByteSliceToString(stat.Fstypename[:]))
	mountedFrom := strings.ToLower(unix.ByteSliceToString(stat.Mntfromname[:]))
	requestedPath := canonicalMountPath(path)
	actualPath := canonicalMountPath(mountedAt)
	if actualPath != requestedPath {
		return errors.New("path is not a mount root")
	}
	macFUSE := strings.Contains(filesystem, "fuse")
	fuseT := filesystem == "nfs" && strings.HasPrefix(mountedFrom, "fuse-t:")
	if !macFUSE && !fuseT {
		return errors.New("mount root is not backed by a supported FUSE provider")
	}
	value, err := os.ReadFile(filepath.Join(path, mountid.Path))
	if err != nil {
		return fmt.Errorf("read CodexFold mount identity: %w", err)
	}
	if len(value) == 0 || len(value) > 256 {
		return errors.New("CodexFold mount identity size is invalid")
	}
	if err := mountid.Validate(value); err != nil {
		return err
	}
	return nil
}

func canonicalMountPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

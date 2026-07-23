//go:build darwin

package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samekind/codexfold/internal/mountid"
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
	if canonicalMountPath(mountedAt) != canonicalMountPath(path) {
		return errors.New("path is not a mount root")
	}
	if !validDarwinMountProvider(filesystem, mountedFrom) {
		return errors.New("mount root is not backed by CodexFold native FSKit or the supported FUSE-T fallback")
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

// MountPresent reports whether path is currently the root of any mounted
// filesystem. It deliberately does not require a healthy CodexFold identity:
// update code must not treat a damaged but still-mounted filesystem as absent.
func MountPresent(path string) (bool, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, err
	}
	return canonicalMountPath(unix.ByteSliceToString(stat.Mntonname[:])) == canonicalMountPath(path), nil
}

func validDarwinMountProvider(filesystem string, mountedFrom string) bool {
	filesystem = strings.ToLower(strings.TrimSpace(filesystem))
	mountedFrom = strings.ToLower(strings.TrimSpace(mountedFrom))
	return filesystem == "codexfold" || filesystem == "nfs" && strings.HasPrefix(mountedFrom, "fuse-t:")
}

func canonicalMountPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

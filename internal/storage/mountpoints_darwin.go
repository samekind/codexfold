//go:build darwin

package storage

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

func nestedMountPoints(root string) (map[string]struct{}, error) {
	count, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	stats := make([]unix.Statfs_t, count+16)
	count, err = unix.Getfsstat(stats, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	result := make(map[string]struct{})
	for _, stat := range stats[:count] {
		mountPoint := filepath.Clean(unix.ByteSliceToString(stat.Mntonname[:]))
		if mountPoint != root && pathWithin(root, mountPoint) {
			result[mountPoint] = struct{}{}
		}
	}
	return result, nil
}

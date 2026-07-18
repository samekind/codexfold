//go:build darwin || linux

package storage

import (
	"errors"
	"math"

	"golang.org/x/sys/unix"
)

func AvailableBytes(path string) (int64, error) {
	path, err := existingSpaceProbePath(path)
	if err != nil {
		return 0, err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	if stat.Bsize <= 0 {
		return 0, errors.New("filesystem block size is invalid")
	}
	available := uint64(stat.Bavail) * uint64(stat.Bsize)
	if available > math.MaxInt64 {
		return math.MaxInt64, nil
	}
	return int64(available), nil
}

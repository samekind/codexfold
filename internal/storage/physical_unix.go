//go:build darwin || linux

package storage

import (
	"fmt"
	"os"
	"syscall"
)

func physicalFile(path string, info os.FileInfo) (string, int64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", 0, fmt.Errorf("read physical file identity for %s", path)
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), stat.Blocks * 512, nil
}

func physicalLinkCount(_ string, info os.FileInfo) (uint64, error) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Nlink), nil
	}
	return 1, nil
}

//go:build darwin

package mountfs

import (
	"os"
	"path/filepath"

	"github.com/jstar0/codexfold/internal/fskitproto"
	"golang.org/x/sys/unix"
)

func nativeFSKitStat(root string) (fskitproto.StatFS, error) {
	if root == "" {
		root = os.TempDir()
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return fskitproto.StatFS{}, err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(root, &stat); err != nil {
		return fskitproto.StatFS{}, err
	}
	blockSize := uint64(stat.Bsize)
	total := stat.Blocks * blockSize
	available := stat.Bavail * blockSize
	free := stat.Bfree * blockSize
	return fskitproto.StatFS{
		BlockSize: uint32(stat.Bsize), IOSize: 4 * 1024 * 1024,
		TotalBytes: total, AvailableBytes: available, FreeBytes: free, UsedBytes: total - free,
		TotalFiles: stat.Files, FreeFiles: stat.Ffree,
	}, nil
}

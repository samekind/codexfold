//go:build linux && fuse && fuse3 && cgo

package mountfs

import (
	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/unix"
)

func populateFilesystemStat(path string, stat *fuse.Statfs_t) int {
	var source unix.Statfs_t
	result := unixResult(unix.Statfs(path, &source))
	if result != 0 {
		return result
	}
	stat.Bsize = uint64(source.Bsize)
	stat.Frsize = uint64(source.Bsize)
	stat.Blocks = source.Blocks
	stat.Bfree = source.Bfree
	stat.Bavail = source.Bavail
	stat.Files = source.Files
	stat.Ffree = source.Ffree
	stat.Favail = source.Ffree
	stat.Namemax = 255
	return 0
}

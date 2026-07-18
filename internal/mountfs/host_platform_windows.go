//go:build windows && winfsp

package mountfs

import (
	"os"
	"syscall"
	"time"

	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
)

func populateFilesystemStat(path string, stat *fuse.Statfs_t) int {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return unixResult(err)
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(pointer, &available, &total, &free); err != nil {
		return unixResult(err)
	}
	const blockSize = uint64(4096)
	stat.Bsize = blockSize
	stat.Frsize = blockSize
	stat.Blocks = total / blockSize
	stat.Bfree = free / blockSize
	stat.Bavail = available / blockSize
	stat.Namemax = 255
	return 0
}

func setFileTimes(path string, times []fuse.Timespec) int {
	atime := time.Unix(times[0].Sec, times[0].Nsec)
	mtime := time.Unix(times[1].Sec, times[1].Nsec)
	return unixResult(os.Chtimes(path, atime, mtime))
}

func setExtendedAttribute(string, string, []byte, int) int {
	return -int(syscall.ENOSYS)
}

func getExtendedAttribute(string, string) (int, []byte) {
	return -int(syscall.ENOSYS), nil
}

func listExtendedAttributes(string) (int, []string) {
	return -int(syscall.ENOSYS), nil
}

func removeExtendedAttribute(string, string) int {
	return -int(syscall.ENOSYS)
}

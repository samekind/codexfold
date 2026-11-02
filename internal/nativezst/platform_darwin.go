//go:build darwin

package nativezst

import (
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

var noAttribute = unix.ENOATTR

type fpunchhole struct {
	flags    uint32
	reserved uint32
	offset   int64
	length   int64
}

// releaseBlocks frees every data block while keeping the size, so reads
// outside the mount see zeros and the inode keeps its identity.
func releaseBlocks(file *os.File, size int64) error {
	arguments := fpunchhole{length: (size + 4095) &^ 4095}
	if _, _, errno := unix.Syscall(unix.SYS_FCNTL, file.Fd(), uintptr(unix.F_PUNCHHOLE), uintptr(unsafe.Pointer(&arguments))); errno != 0 {
		return &os.PathError{Op: "fcntl F_PUNCHHOLE", Path: file.Name(), Err: errno}
	}
	return nil
}

// durableSync reaches the disk itself: plain fsync on macOS stops at the
// drive cache, and the marker must survive power loss before data is freed.
func durableSync(file *os.File) error {
	if _, err := unix.FcntlInt(file.Fd(), unix.F_FULLFSYNC, 0); err != nil {
		return file.Sync()
	}
	return nil
}

func fileTimes(info os.FileInfo) (time.Time, time.Time) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(stat.Atimespec.Unix()), info.ModTime()
	}
	return info.ModTime(), info.ModTime()
}

func changeTime(info os.FileInfo) time.Time {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(stat.Ctimespec.Unix())
	}
	return time.Time{}
}

//go:build linux

package nativezst

import (
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var noAttribute = unix.ENODATA

func releaseBlocks(file *os.File, size int64) error {
	if err := unix.Fallocate(int(file.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, 0, size); err != nil {
		return &os.PathError{Op: "fallocate punch hole", Path: file.Name(), Err: err}
	}
	return nil
}

func durableSync(file *os.File) error { return file.Sync() }

func fileTimes(info os.FileInfo) (time.Time, time.Time) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(stat.Atim.Unix()), info.ModTime()
	}
	return info.ModTime(), info.ModTime()
}

func changeTime(info os.FileInfo) time.Time {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(stat.Ctim.Unix())
	}
	return time.Time{}
}

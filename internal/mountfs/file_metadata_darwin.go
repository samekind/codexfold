//go:build darwin

package mountfs

import (
	"os"
	"syscall"
	"time"
)

func fileOwnershipAndTimes(info os.FileInfo) (uint32, uint32, time.Time, time.Time) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return uint32(os.Getuid()), uint32(os.Getgid()), info.ModTime(), info.ModTime()
	}
	return stat.Uid, stat.Gid,
		time.Unix(stat.Atimespec.Sec, stat.Atimespec.Nsec),
		time.Unix(stat.Ctimespec.Sec, stat.Ctimespec.Nsec)
}

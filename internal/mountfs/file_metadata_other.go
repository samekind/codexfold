//go:build !darwin && !linux

package mountfs

import (
	"os"
	"time"
)

func fileOwnershipAndTimes(info os.FileInfo) (uint32, uint32, time.Time, time.Time) {
	return uint32(os.Getuid()), uint32(os.Getgid()), info.ModTime(), info.ModTime()
}

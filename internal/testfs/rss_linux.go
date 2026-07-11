//go:build linux

package testfs

import (
	"time"

	"golang.org/x/sys/unix"
)

func processResourceUsage() resourceUsage {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil || usage.Maxrss < 0 {
		return resourceUsage{}
	}
	return resourceUsage{MaxRSSBytes: uint64(usage.Maxrss) * 1024, UserCPU: time.Duration(unix.TimevalToNsec(usage.Utime)), SystemCPU: time.Duration(unix.TimevalToNsec(usage.Stime))}
}

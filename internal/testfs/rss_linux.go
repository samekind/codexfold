//go:build linux

package testfs

import "golang.org/x/sys/unix"

func maxRSSBytes() uint64 {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil || usage.Maxrss < 0 {
		return 0
	}
	return uint64(usage.Maxrss) * 1024
}

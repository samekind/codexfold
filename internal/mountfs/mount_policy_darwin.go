//go:build darwin && fuse && cgo

package mountfs

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

func configureMountedFilesystem(ctx context.Context, mountPoint string) error {
	deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var lastErr error
	for {
		var stat unix.Statfs_t
		if err := unix.Statfs(mountPoint, &stat); err == nil && statfsType(stat) == "nfs" {
			output, err := exec.CommandContext(deadline, "/sbin/mount", "-u", "-o", "sync", mountPoint).CombinedOutput()
			if err == nil {
				if err := unix.Statfs(mountPoint, &stat); err == nil && stat.Flags&unix.MNT_SYNCHRONOUS != 0 {
					return nil
				}
				lastErr = fmt.Errorf("NFS mount did not report synchronous I/O")
			} else if deadline.Err() == nil {
				lastErr = fmt.Errorf("update NFS mount: %w: %s", err, bytes.TrimSpace(output))
			}
		}

		select {
		case <-deadline.Done():
			if lastErr != nil {
				return lastErr
			}
			return deadline.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func statfsType(stat unix.Statfs_t) string {
	length := bytes.IndexByte(stat.Fstypename[:], 0)
	if length < 0 {
		length = len(stat.Fstypename)
	}
	return string(stat.Fstypename[:length])
}

//go:build linux && fuse && fuse3 && cgo

package mountfs

import (
	"context"
	"errors"
	"time"
)

func configureMountedFilesystem(ctx context.Context, mountPoint string) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if linuxFuseMountVisible(mountPoint) {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("FUSE3 mount did not become visible before cancellation")
		case <-ticker.C:
		}
	}
}

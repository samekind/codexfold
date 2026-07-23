package service

import (
	"context"
	"fmt"
	"time"
)

func waitHealthy(ctx context.Context, timeout time.Duration, status func() Status) (Status, error) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var last Status
	for {
		last = status()
		if last.DaemonRunning && last.MountHealthy {
			return last, nil
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-deadline.C:
			return last, fmt.Errorf("filesystem service did not become healthy: daemon=%t mount=%t daemon_error=%q mount_error=%q", last.DaemonRunning, last.MountHealthy, last.DaemonError, last.MountError)
		case <-ticker.C:
		}
	}
}

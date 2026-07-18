package launcher

import (
	"context"
	"errors"
	"os"
	"strconv"
	"time"
)

const ParentPIDEnvironment = "CODEXFOLD_LAUNCHER_PARENT_PID"

func MonitorContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	return monitorParent(parent, os.Getenv(ParentPIDEnvironment), os.Getppid, 50*time.Millisecond)
}

func monitorParent(parent context.Context, value string, currentParent func() int, interval time.Duration) (context.Context, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(parent)
	if value == "" {
		return ctx, cancel, nil
	}
	expected, err := strconv.Atoi(value)
	if err != nil || expected <= 1 {
		cancel()
		return nil, nil, errors.New("invalid CodexFold launcher parent PID")
	}
	if currentParent == nil || currentParent() != expected {
		cancel()
		return nil, nil, errors.New("CodexFold launcher parent is already unavailable")
	}
	if interval <= 0 {
		interval = 50 * time.Millisecond
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if currentParent() != expected {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel, nil
}

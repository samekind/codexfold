//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	"golang.org/x/sys/windows"
)

func detectEnrollmentWriters(ctx context.Context, sessions []codex.Session) (map[string]bool, error) {
	writers := make(map[string]bool)
	for _, session := range sessions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path, err := windows.UTF16PtrFromString(session.RolloutPath)
		if err != nil {
			return nil, fmt.Errorf("encode rollout path for native handle probe: %w", err)
		}
		var handle windows.Handle
		deadline := time.Now().Add(250 * time.Millisecond)
		for {
			handle, err = windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
			if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
				break
			}
			if time.Now().After(deadline) {
				break
			}
			// Closing a WinFsp alias can finish its backing-handle cleanup
			// asynchronously. Do not mistake our preceding probe for a writer.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
		if err == nil {
			if closeErr := windows.CloseHandle(handle); closeErr != nil {
				return nil, fmt.Errorf("close native rollout probe: %w", closeErr)
			}
			continue
		}
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			writers[session.ID] = true
			continue
		}
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			// SQLite can retain a route after its rollout has been removed.
			// Let the planner mark that individual path invalid; its mount and
			// namespace gates still block enrollment during an actual outage.
			continue
		}
		return nil, fmt.Errorf("probe native rollout handle %s: %w", session.ID, err)
	}
	return writers, nil
}

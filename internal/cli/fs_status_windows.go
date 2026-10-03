//go:build windows

package cli

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/samekind/codexfold/internal/fskitstatus"
	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/service"
)

// The tray consumes aggregate status only; it never reads rollout contents.
func startPlatformDaemonStatus(parent context.Context, store, mount string, activity *mountfs.IOActivityCounter, diagnostics io.Writer) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		path := filepath.Join(store, "fs", "status", "daemon.json")
		instance := rand.Text()
		var sequence uint64
		publish := func(state, detail string) {
			sequence++
			totals := activity.Snapshot()
			snapshot := fskitstatus.Snapshot{
				Component: "daemon", State: state, Detail: detail,
				MountPoint: mount, ResourcePath: filepath.Join(store, "fs"), PID: os.Getpid(),
				PublisherInstanceID: instance, BackendID: filepath.Clean(store), ObservationSequence: sequence,
				ReadBytesTotal: &totals.ReadBytes, WrittenBytesTotal: &totals.WrittenBytes,
			}
			if err := fskitstatus.Write(path, snapshot); err != nil {
				_, _ = fmt.Fprintf(diagnostics, "write Windows daemon status: %v\n", err)
			}
		}
		publish("starting", "Waiting for the WinFsp mount")
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				publish("stopped", "Filesystem service stopped")
				return
			case <-ticker.C:
				if err := service.ProbeMount(mount); err != nil {
					publish("unavailable", err.Error())
				} else {
					publish("healthy", "WinFsp mount is responding")
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}

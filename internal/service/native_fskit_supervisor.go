package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/fskitstatus"
	"github.com/samekind/codexfold/internal/launcher"
)

var ErrForeignMount = errors.New("mount point is occupied by a foreign filesystem")

const NativeFSKitSupervisorLockName = "supervisor.lock"

type NativeFSKitMountState struct {
	Mounted bool
	Owned   bool
	Healthy bool
}

type NativeFSKitOperations interface {
	DaemonHealthy(context.Context, string) error
	MountState(context.Context, string, time.Duration) (NativeFSKitMountState, error)
	Mount(context.Context, string, string) error
	Unmount(context.Context, string, bool) error
}

type NativeFSKitSupervisorOptions struct {
	ResourcePath    string
	MountPoint      string
	Interval        time.Duration
	ProbeTimeout    time.Duration
	RecoveryTimeout time.Duration
	StatusPath      string
	Operations      NativeFSKitOperations
	Event           func(string)
}

type nativeFSKitSupervisorStatusTracker struct {
	publisherInstanceID string
	backendID           string
	observationSequence uint64
	incidentID          string
	incidentSince       time.Time
}

func (t *nativeFSKitSupervisorStatusTracker) resume(snapshot fskitstatus.Snapshot) {
	if t == nil || snapshot.Component != "supervisor" || snapshot.BackendID != t.backendID || snapshot.State == "healthy" || snapshot.IncidentID == "" {
		return
	}
	since, err := time.Parse(time.RFC3339Nano, snapshot.RecoveryStartedAt)
	if err != nil {
		return
	}
	t.incidentID = snapshot.IncidentID
	t.incidentSince = since
}

func newNativeFSKitSupervisorStatusTracker(options NativeFSKitSupervisorOptions) *nativeFSKitSupervisorStatusTracker {
	var token [16]byte
	publisherInstanceID := ""
	if _, err := rand.Read(token[:]); err == nil {
		publisherInstanceID = "supervisor-publisher-" + hex.EncodeToString(token[:])
	} else {
		publisherInstanceID = fmt.Sprintf("supervisor-publisher-%x", time.Now().UnixNano())
	}
	identity := strings.Join([]string{
		"codexfold-supervisor-backend-v1",
		filepath.Clean(options.ResourcePath),
		filepath.Clean(options.MountPoint),
	}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return &nativeFSKitSupervisorStatusTracker{
		publisherInstanceID: publisherInstanceID,
		backendID:           "supervisor-backend-sha256-" + hex.EncodeToString(digest[:]),
	}
}

func (t *nativeFSKitSupervisorStatusTracker) snapshot(
	options NativeFSKitSupervisorOptions,
	state string,
	summary string,
	statusErr error,
) fskitstatus.Snapshot {
	if t.observationSequence != ^uint64(0) {
		t.observationSequence++
	}
	detail := ""
	if statusErr != nil {
		detail = statusErr.Error()
	}
	now := time.Now().UTC()
	unhealthy := state != "healthy"
	if unhealthy && t.incidentID == "" {
		var token [16]byte
		if _, err := rand.Read(token[:]); err == nil {
			t.incidentID = "supervisor-incident-" + hex.EncodeToString(token[:])
		} else {
			t.incidentID = fmt.Sprintf("supervisor-incident-%x", now.UnixNano())
		}
		t.incidentSince = now
	}
	snapshot := fskitstatus.Snapshot{
		Component: "supervisor", State: state, Summary: summary, Detail: detail,
		MountPoint: options.MountPoint, ResourcePath: options.ResourcePath,
		ObservationSequence: t.observationSequence,
		PublisherInstanceID: t.publisherInstanceID,
		BackendID:           t.backendID,
	}
	if t.incidentID != "" {
		snapshot.IncidentID = t.incidentID
		snapshot.RecoveryStartedAt = t.incidentSince.Format(time.RFC3339Nano)
		snapshot.ElapsedMilliseconds = max(0, now.Sub(t.incidentSince).Milliseconds())
		if unhealthy {
			snapshot.Reason = summary
			snapshot.Impact = "Codex session file operations may wait or fail while supervision is recovering."
			snapshot.Recommendations = []string{
				"Keep Codex running while CodexFold restores file-service health.",
				"Open CodexFold to inspect the current incident.",
			}
		}
	}
	if state == "healthy" {
		t.incidentID = ""
		t.incidentSince = time.Time{}
	}
	return snapshot
}

func RunNativeFSKitSupervisor(ctx context.Context, options NativeFSKitSupervisorOptions) error {
	if !filepath.IsAbs(options.ResourcePath) || !filepath.IsAbs(options.MountPoint) {
		return errors.New("absolute FSKit resource and mount paths are required")
	}
	if options.StatusPath != "" && !filepath.IsAbs(options.StatusPath) {
		return errors.New("absolute FSKit supervisor status path is required")
	}
	if options.Interval <= 0 {
		options.Interval = time.Second
	}
	if options.ProbeTimeout <= 0 {
		options.ProbeTimeout = 2 * time.Second
	}
	if options.RecoveryTimeout <= 0 {
		options.RecoveryTimeout = 15 * time.Second
	}
	var statusPublisher *fskitstatus.Publisher
	statusTracker := newNativeFSKitSupervisorStatusTracker(options)
	if options.StatusPath != "" {
		statusPublisher = fskitstatus.NewPublisher(options.StatusPath, func(err error) {
			if options.Event != nil {
				options.Event("write supervisor status: " + err.Error())
			}
		})
		if previous, readErr := fskitstatus.Read(options.StatusPath); readErr == nil {
			statusTracker.resume(previous)
		}
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_ = statusPublisher.Close(closeCtx)
		}()
	}
	operations := options.Operations
	if operations == nil {
		var err error
		operations, err = defaultNativeFSKitOperations()
		if err != nil {
			writeNativeFSKitSupervisorStatus(options, statusPublisher, statusTracker, "unavailable", "File service supervision is unavailable", err)
			return err
		}
	}
	writeNativeFSKitSupervisorStatus(options, statusPublisher, statusTracker, "recovering", "File service supervision is starting", nil)
	ticker := time.NewTicker(options.Interval)
	defer ticker.Stop()
	for {
		err := reconcileNativeFSKit(ctx, options, operations)
		if err == nil {
			writeNativeFSKitSupervisorStatus(options, statusPublisher, statusTracker, "healthy", "File service is available", nil)
		} else if errors.Is(err, ErrForeignMount) {
			writeNativeFSKitSupervisorStatus(options, statusPublisher, statusTracker, "unavailable", "Another filesystem occupies the session path", err)
		} else {
			writeNativeFSKitSupervisorStatus(options, statusPublisher, statusTracker, "recovering", "File service is recovering", err)
		}
		if err != nil && options.Event != nil {
			options.Event(err.Error())
		}
		select {
		case <-ctx.Done():
			if launcher.ParentUnavailable(ctx) {
				if options.Event != nil {
					options.Event("FSKit launcher parent exited; preserving the owned mount for launchd recovery")
				}
				return nil
			}
			err := shutdownNativeFSKit(options, operations)
			if err != nil {
				writeNativeFSKitSupervisorStatus(options, statusPublisher, statusTracker, "unavailable", "File service supervision could not stop cleanly", err)
			} else {
				writeNativeFSKitSupervisorStatus(options, statusPublisher, statusTracker, "stopped", "File service supervision is stopped", nil)
			}
			return err
		case <-ticker.C:
		}
	}
}

func writeNativeFSKitSupervisorStatus(
	options NativeFSKitSupervisorOptions,
	publisher *fskitstatus.Publisher,
	tracker *nativeFSKitSupervisorStatusTracker,
	state string,
	summary string,
	statusErr error,
) {
	if publisher == nil {
		return
	}
	publisher.Publish(tracker.snapshot(options, state, summary, statusErr))
}

func reconcileNativeFSKit(
	ctx context.Context,
	options NativeFSKitSupervisorOptions,
	operations NativeFSKitOperations,
) error {
	mountState, mountErr := operations.MountState(ctx, options.MountPoint, options.ProbeTimeout)
	if mountState.Mounted && !mountState.Owned {
		return fmt.Errorf("%w: %s", ErrForeignMount, options.MountPoint)
	}
	if mountErr != nil {
		return fmt.Errorf("probe native FSKit mount before reconciliation: %w", mountErr)
	}
	daemonErr := operations.DaemonHealthy(ctx, options.ResourcePath)
	if mountState.Owned && mountState.Healthy && daemonErr == nil {
		return nil
	}
	if daemonErr != nil {
		if mountState.Owned {
			return errors.Join(mountErr, fmt.Errorf("FSKit backend is recovering while the owned mount remains available: %w", daemonErr))
		}
		return errors.Join(mountErr, fmt.Errorf("FSKit daemon unavailable: %w", daemonErr))
	}
	if mountState.Owned && !mountState.Healthy {
		return errors.Join(mountErr, errors.New("owned FSKit mount is waiting for frontend recovery"))
	}
	if mountState.Owned && mountState.Healthy {
		return nil
	}
	if err := operations.Mount(ctx, options.ResourcePath, options.MountPoint); err != nil {
		return fmt.Errorf("mount native FSKit volume: %w", err)
	}
	return waitForNativeFSKitMount(ctx, options, operations)
}

func waitForNativeFSKitMount(ctx context.Context, options NativeFSKitSupervisorOptions, operations NativeFSKitOperations) error {
	deadline := time.NewTimer(options.RecoveryTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		mountState, mountErr := operations.MountState(ctx, options.MountPoint, options.ProbeTimeout)
		daemonErr := operations.DaemonHealthy(ctx, options.ResourcePath)
		if mountState.Mounted && !mountState.Owned {
			return fmt.Errorf("%w: %s", ErrForeignMount, options.MountPoint)
		}
		if mountErr == nil && mountState.Owned && mountState.Healthy && daemonErr == nil {
			return nil
		}
		lastErr = errors.Join(mountErr, daemonErr)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("native FSKit mount did not become healthy: %w", lastErr)
		case <-ticker.C:
		}
	}
}

func shutdownNativeFSKit(options NativeFSKitSupervisorOptions, operations NativeFSKitOperations) error {
	ctx, cancel := context.WithTimeout(context.Background(), options.RecoveryTimeout)
	defer cancel()
	mountState, err := operations.MountState(ctx, options.MountPoint, options.ProbeTimeout)
	if err != nil {
		return err
	}
	if !mountState.Owned {
		return nil
	}
	if unmountErr := operations.Unmount(ctx, options.MountPoint, false); unmountErr == nil {
		return nil
	}
	return operations.Unmount(ctx, options.MountPoint, true)
}

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

var (
	ErrForeignMount                    = errors.New("mount point is occupied by a foreign filesystem")
	ErrNativeFSKitMountProbeInProgress = errors.New("native FSKit mount health probe is already in progress")
	// ErrNativeFSKitMountProbeInconclusive reports a mount-table lookup that ran
	// out of time while the backend was still answering. It is an unusable
	// observation, not evidence of an outage.
	ErrNativeFSKitMountProbeInconclusive = errors.New("native FSKit mount health probe was inconclusive")
)

const NativeFSKitSupervisorLockName = "supervisor.lock"

const NativeFSKitMountType = "codexfoldnative"

func ValidNativeFSKitMountType(value string) bool {
	if len(value) == 0 || len(value) > 32 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value[1:] {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

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
	ResourcePath string
	MountPoint   string
	FSKitType    string
	Interval     time.Duration
	// MountObservationInterval is how long a conclusive "this mount is ours and
	// healthy" reading stays usable. Defaults to five reconciliation intervals.
	MountObservationInterval time.Duration
	ProbeTimeout             time.Duration
	RecoveryTimeout          time.Duration
	StatusPath               string
	Operations               NativeFSKitOperations
	Event                    func(string)
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
			if strings.Contains(detail, "FSKit module is disabled") {
				snapshot.Reason = "CodexFold FSKit module is disabled by macOS"
				snapshot.Recommendations = []string{
					"Open System Settings > General > Login Items & Extensions.",
					"Open CodexFoldFSKit FSKit Modules and turn it on.",
					"Retry the CodexFold service after macOS enables the module.",
				}
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
	if options.FSKitType == "" {
		options.FSKitType = NativeFSKitMountType
	}
	if !ValidNativeFSKitMountType(options.FSKitType) {
		return errors.New("FSKit mount type must start with a lowercase letter and contain only lowercase letters or digits")
	}
	if options.Interval <= 0 {
		options.Interval = time.Second
	}
	if options.ProbeTimeout <= 0 {
		options.ProbeTimeout = 2 * time.Second
	}
	if options.MountObservationInterval <= 0 {
		options.MountObservationInterval = 5 * options.Interval
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
			closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := statusPublisher.Close(closeCtx); err != nil && options.Event != nil {
				options.Event("close supervisor status: " + err.Error())
			}
		}()
	}
	operations := options.Operations
	if operations == nil {
		var err error
		operations, err = nativeFSKitOperationsForType(options.FSKitType)
		if err != nil {
			writeNativeFSKitSupervisorStatus(options, statusPublisher, statusTracker, "unavailable", "File service supervision is unavailable", err)
			return err
		}
	}
	writeNativeFSKitSupervisorStatus(options, statusPublisher, statusTracker, "recovering", "File service supervision is starting", nil)
	// Publication must not wait on reconciliation. Mount recovery alone can hold
	// a single pass for RecoveryTimeout, and a kernel mount-table lookup can
	// outlive its probe deadline; while either ran, the status file stopped
	// advancing and the UI reported the resulting read gap as an incident even
	// though supervision was healthy. Reconcile on its own cadence and keep the
	// heartbeat on the ticker, so a stale channel means the supervisor is
	// actually gone rather than merely busy.
	observations := make(chan nativeFSKitSupervisorObservation, 1)
	reconcileCtx, stopReconciling := context.WithCancel(ctx)
	defer stopReconciling()
	go reconcileNativeFSKitContinuously(reconcileCtx, options, operations, observations)
	current := nativeFSKitSupervisorObservation{state: "recovering", summary: "File service supervision is starting"}
	ticker := time.NewTicker(options.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Never unmount on shutdown. A daemon/supervisor restart must keep
			// the path present (grill lock 5.2); unmounting here is what made a
			// routine restart show Codex "file not found". The mount stays owned
			// by this resource and the next supervisor adopts it, or the `stop`
			// command reclaims it with an explicit unmount.
			if launcher.ParentUnavailable(ctx) {
				if options.Event != nil {
					options.Event("FSKit launcher parent exited; preserving the owned mount for launchd recovery")
				}
			} else if options.Event != nil {
				options.Event("FSKit supervisor stopped; preserving the owned mount (explicit stop unmounts it)")
			}
			writeNativeFSKitSupervisorStatus(options, statusPublisher, statusTracker, "stopped", "File service supervision is stopped; mount preserved", nil)
			return nil
		case observation := <-observations:
			current = observation
			writeNativeFSKitSupervisorStatus(options, statusPublisher, statusTracker, current.state, current.summary, current.err)
			// A fresh observation is this interval's publication; restarting the
			// ticker keeps the channel at one write per interval instead of
			// doubling it whenever reconciliation keeps pace.
			ticker.Reset(options.Interval)
		case <-ticker.C:
			writeNativeFSKitSupervisorStatus(options, statusPublisher, statusTracker, current.state, current.summary, current.err)
		}
	}
}

// nativeFSKitSupervisorObservation is the latest reconciliation outcome the
// heartbeat republishes until reconciliation reports a different one.
type nativeFSKitSupervisorObservation struct {
	state   string
	summary string
	err     error
}

func reconcileNativeFSKitContinuously(
	ctx context.Context,
	options NativeFSKitSupervisorOptions,
	operations NativeFSKitOperations,
	observations chan<- nativeFSKitSupervisorObservation,
) {
	ticker := time.NewTicker(options.Interval)
	defer ticker.Stop()
	observed := &nativeFSKitMountObservation{validFor: options.MountObservationInterval}
	for {
		err := reconcileNativeFSKit(ctx, options, operations, observed)
		var observation nativeFSKitSupervisorObservation
		switch {
		case errors.Is(err, ErrNativeFSKitMountProbeInProgress),
			errors.Is(err, ErrNativeFSKitMountProbeInconclusive):
			// A previous non-cancellable kernel mount-table lookup still owns the
			// single probe slot. Nothing observable about the mount changed, so
			// leave the last observation in place instead of turning local probe
			// serialization into a new recovery incident. The heartbeat keeps
			// republishing it, so the channel still proves the supervisor is live.
		case err == nil:
			observation = nativeFSKitSupervisorObservation{state: "healthy", summary: "File service is available"}
		case errors.Is(err, ErrForeignMount):
			observation = nativeFSKitSupervisorObservation{state: "unavailable", summary: "Another filesystem occupies the session path", err: err}
		default:
			observation = nativeFSKitSupervisorObservation{state: "recovering", summary: "File service is recovering", err: err}
		}
		if observation.state != "" {
			select {
			case observations <- observation:
			case <-ctx.Done():
				return
			}
		}
		if err != nil && observation.state != "" && options.Event != nil {
			options.Event(err.Error())
		}
		select {
		case <-ctx.Done():
			return
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

// nativeFSKitMountObservation remembers the last mount-table reading that was
// conclusive and favourable, so steady-state reconciliation does not re-read the
// whole table every interval.
type nativeFSKitMountObservation struct {
	validFor   time.Duration
	observedAt time.Time
	valid      bool
}

func (o *nativeFSKitMountObservation) usable(now time.Time) bool {
	if o == nil || !o.valid || o.validFor <= 0 {
		return false
	}
	return now.Sub(o.observedAt) < o.validFor
}

func (o *nativeFSKitMountObservation) record(state NativeFSKitMountState, now time.Time) {
	if o == nil {
		return
	}
	// Only an owned, healthy mount may be reused. Every other reading describes
	// something the supervisor has to act on at the next interval.
	if !state.Owned || !state.Healthy {
		o.valid = false
		return
	}
	o.observedAt = now
	o.valid = true
}

func (o *nativeFSKitMountObservation) invalidate() {
	if o != nil {
		o.valid = false
	}
}

func reconcileNativeFSKit(
	ctx context.Context,
	options NativeFSKitSupervisorOptions,
	operations NativeFSKitOperations,
	observed *nativeFSKitMountObservation,
) error {
	now := time.Now()
	if observed.usable(now) {
		// The kernel recently confirmed this mount as ours, and a mount does not
		// stop being ours without the backend noticing. Re-reading the whole
		// mount table every interval buys nothing and, on a loaded machine, is
		// mostly an opportunity for the bounded probe to miss its deadline: the
		// production store logged 1,906 probes that never got scheduled at all.
		// The backend socket is the cheap liveness signal, so ask it instead and
		// fall through to a full reading the moment it stops answering.
		if operations.DaemonHealthy(ctx, options.ResourcePath) == nil {
			return nil
		}
		observed.invalidate()
	}
	mountState, mountErr := operations.MountState(ctx, options.MountPoint, options.ProbeTimeout)
	if mountErr == nil {
		observed.record(mountState, now)
	} else {
		observed.invalidate()
	}
	if mountState.Mounted && !mountState.Owned {
		return fmt.Errorf("%w: %s", ErrForeignMount, options.MountPoint)
	}
	if mountErr != nil {
		// A mount-table lookup that ran out of time proves nothing about the
		// mount. Even MNT_NOWAIT contends with unrelated volumes and with the
		// scheduler, so under load this deadline expires while the file service
		// is perfectly healthy; reporting it as recovery is what turned ordinary
		// system load into a stream of "needs attention" alerts. A mount that
		// actually went away is reported by a lookup that completes, not by one
		// that runs out of time. Ask the backend directly instead: a reachable
		// daemon means file operations are being served and the probe was merely
		// slow, so keep the last conclusive observation.
		if errors.Is(mountErr, context.DeadlineExceeded) &&
			operations.DaemonHealthy(ctx, options.ResourcePath) == nil {
			return ErrNativeFSKitMountProbeInconclusive
		}
		return fmt.Errorf("probe native FSKit mount before reconciliation: %w", mountErr)
	}
	daemonErr := operations.DaemonHealthy(ctx, options.ResourcePath)
	// An owned mount plus a live daemon is healthy. Do not treat a busy FSKit
	// getattr path as recovery: Codex I/O and the old through-mount probe
	// shared one control lock, which is what tripped the 2s/10s incident.
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
	// Poll quickly at first so a mount that lands immediately is noticed
	// immediately, then back off. A fixed 50ms poll issues twenty mount-table
	// reads a second for the whole recovery window, which is the pressure that
	// makes the bounded probe miss its deadline in the first place.
	const (
		minimumPoll = 50 * time.Millisecond
		maximumPoll = 500 * time.Millisecond
	)
	poll := minimumPoll
	timer := time.NewTimer(poll)
	defer timer.Stop()
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
		case <-timer.C:
			if poll < maximumPoll {
				poll = min(2*poll, maximumPoll)
			}
			timer.Reset(poll)
		}
	}
}

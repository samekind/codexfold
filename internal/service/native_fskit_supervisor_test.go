package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/fskitstatus"
	"github.com/samekind/codexfold/internal/launcher"
)

func TestNativeFSKitSupervisorStatusTrackerPreservesOutageAcrossPublisherReplacement(t *testing.T) {
	options := NativeFSKitSupervisorOptions{
		ResourcePath: "/private/tmp/codexfold-resource",
		MountPoint:   "/private/tmp/codexfold-mount",
	}
	first := newNativeFSKitSupervisorStatusTracker(options)
	failure := first.snapshot(options, "recovering", "File service is recovering", errors.New("test"))
	second := newNativeFSKitSupervisorStatusTracker(options)
	second.resume(failure)
	replayed := second.snapshot(options, "recovering", "File service is recovering", errors.New("test"))
	if first.publisherInstanceID == second.publisherInstanceID {
		t.Fatal("replacement supervisor reused publisher identity")
	}
	if first.backendID != second.backendID {
		t.Fatalf("logical backend changed: %q != %q", first.backendID, second.backendID)
	}
	if replayed.IncidentID != failure.IncidentID || replayed.RecoveryStartedAt != failure.RecoveryStartedAt {
		t.Fatalf("continuous outage identity changed: failure=%#v replayed=%#v", failure, replayed)
	}
	recovery := second.snapshot(options, "healthy", "File service is available", nil)
	if recovery.IncidentID != failure.IncidentID {
		t.Fatalf("healthy recovery lost outage identity: %#v", recovery)
	}
	thirdFailure := second.snapshot(options, "recovering", "File service is recovering", errors.New("again"))
	if thirdFailure.IncidentID == failure.IncidentID {
		t.Fatal("new failure after healthy recovery reused the old incident identity")
	}
}

func TestNativeFSKitSupervisorStatusExplainsDisabledModule(t *testing.T) {
	options := NativeFSKitSupervisorOptions{
		ResourcePath: "/private/tmp/codexfold-resource",
		MountPoint:   "/private/tmp/codexfold-mount",
	}
	tracker := newNativeFSKitSupervisorStatusTracker(options)
	snapshot := tracker.snapshot(
		options,
		"recovering",
		"File service is recovering",
		errors.New("mount native FSKit volume: FSKit module is disabled by macOS: Module vip.jstar.codexfold.fskitprofileprobe.module is disabled; enable CodexFoldFSKit FSKit Modules in System Settings > General > Login Items & Extensions, then retry"),
	)
	if snapshot.Reason != "CodexFold FSKit module is disabled by macOS" {
		t.Fatalf("unexpected disabled-module reason: %q", snapshot.Reason)
	}
	if len(snapshot.Recommendations) != 3 || snapshot.Recommendations[1] != "Open CodexFoldFSKit FSKit Modules and turn it on." {
		t.Fatalf("unexpected disabled-module recommendations: %#v", snapshot.Recommendations)
	}
}

func TestNativeFSKitSupervisorMountsAndPreservesMountOnShutdown(t *testing.T) {
	operations := &fakeNativeFSKitOperations{
		daemonHealthy: true,
		mounted:       make(chan struct{}), forceUnmounted: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunNativeFSKitSupervisor(ctx, NativeFSKitSupervisorOptions{
			ResourcePath: "/tmp/resource", MountPoint: "/tmp/mount",
			Interval: time.Millisecond, RecoveryTimeout: 100 * time.Millisecond,
			Operations: operations,
		})
	}()

	select {
	case <-operations.mounted:
	case <-time.After(time.Second):
		t.Fatal("supervisor did not mount")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("supervisor shutdown: %v", err)
	}
	operations.mu.Lock()
	defer operations.mu.Unlock()
	// Shutdown must never unmount: the path stays present across a restart
	// (grill lock 5.2). The explicit `stop` command reclaims the mount instead.
	if operations.mountCalls != 1 || operations.unmountCalls != 0 || operations.forceUnmountCalls != 0 {
		t.Fatalf("mount calls=%d unmount=%d force=%d", operations.mountCalls, operations.unmountCalls, operations.forceUnmountCalls)
	}
}

func TestNativeFSKitSupervisorPreservesOwnedMountWhenLauncherParentDisappears(t *testing.T) {
	operations := &fakeNativeFSKitOperations{
		daemonHealthy: true,
		state:         NativeFSKitMountState{Mounted: true, Owned: true, Healthy: true},
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunNativeFSKitSupervisor(ctx, NativeFSKitSupervisorOptions{
			ResourcePath: "/tmp/resource", MountPoint: "/tmp/mount",
			Interval: time.Millisecond, RecoveryTimeout: 100 * time.Millisecond,
			Operations: operations,
		})
	}()

	deadline := time.Now().Add(time.Second)
	for {
		operations.mu.Lock()
		probes := operations.probeCalls
		operations.mu.Unlock()
		if probes > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor did not observe the healthy owned mount")
		}
		time.Sleep(time.Millisecond)
	}
	cancel(launcher.ErrParentUnavailable)
	if err := <-done; err != nil {
		t.Fatalf("supervisor launcher-loss exit: %v", err)
	}

	operations.mu.Lock()
	defer operations.mu.Unlock()
	if operations.unmountCalls != 0 || operations.forceUnmountCalls != 0 {
		t.Fatalf("launcher loss mutated owned mount: unmount=%d force=%d", operations.unmountCalls, operations.forceUnmountCalls)
	}
}

func TestNativeFSKitSupervisorReAdoptsOwnedMountAfterRestart(t *testing.T) {
	operations := &fakeNativeFSKitOperations{
		daemonHealthy: true,
		state:         NativeFSKitMountState{Mounted: true, Owned: true, Healthy: true},
	}
	options := NativeFSKitSupervisorOptions{
		ResourcePath: "/tmp/resource", MountPoint: "/tmp/mount",
		RecoveryTimeout: 100 * time.Millisecond, Operations: operations,
	}
	if err := reconcileNativeFSKit(context.Background(), options, operations, &nativeFSKitMountObservation{}); err != nil {
		t.Fatalf("first supervisor epoch: %v", err)
	}
	if err := reconcileNativeFSKit(context.Background(), options, operations, &nativeFSKitMountObservation{}); err != nil {
		t.Fatalf("replacement supervisor epoch: %v", err)
	}
	operations.mu.Lock()
	defer operations.mu.Unlock()
	if operations.probeCalls != 2 || operations.mountCalls != 0 || operations.unmountCalls != 0 || operations.forceUnmountCalls != 0 {
		t.Fatalf("owned mount was not re-adopted: %#v", operations)
	}
}

func TestNativeFSKitSupervisorKeepsOwnedMountDuringBackendFailure(t *testing.T) {
	operations := &fakeNativeFSKitOperations{
		daemonErr: errors.New("daemon unavailable"),
		state:     NativeFSKitMountState{Mounted: true, Owned: true, Healthy: false},
		mounted:   make(chan struct{}), forceUnmounted: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunNativeFSKitSupervisor(ctx, NativeFSKitSupervisorOptions{
			ResourcePath: "/tmp/resource", MountPoint: "/tmp/mount",
			Interval: time.Millisecond, RecoveryTimeout: 100 * time.Millisecond,
			Operations: operations,
		})
	}()

	deadline := time.Now().Add(time.Second)
	for {
		operations.mu.Lock()
		probes := operations.probeCalls
		operations.mu.Unlock()
		if probes >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor did not continue probing the recovering mount")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("supervisor shutdown: %v", err)
	}
	operations.mu.Lock()
	defer operations.mu.Unlock()
	if operations.forceUnmountCalls != 0 || operations.unmountCalls != 0 || operations.probeCalls < 3 {
		t.Fatalf("force unmount=%d normal unmount=%d probes=%d", operations.forceUnmountCalls, operations.unmountCalls, operations.probeCalls)
	}
}

func TestNativeFSKitSupervisorKeepsReportingForeignMountWithoutMutation(t *testing.T) {
	operations := &fakeNativeFSKitOperations{
		daemonHealthy: true,
		state:         NativeFSKitMountState{Mounted: true, Owned: false, Healthy: false},
		mounted:       make(chan struct{}), forceUnmounted: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunNativeFSKitSupervisor(ctx, NativeFSKitSupervisorOptions{
			ResourcePath: "/tmp/resource", MountPoint: "/tmp/mount",
			Interval: time.Millisecond, RecoveryTimeout: 100 * time.Millisecond,
			Operations: operations,
		})
	}()
	deadline := time.Now().Add(time.Second)
	for {
		operations.mu.Lock()
		probes := operations.probeCalls
		operations.mu.Unlock()
		if probes >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor exited or stopped probing a foreign mount")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("supervisor shutdown: %v", err)
	}
	operations.mu.Lock()
	defer operations.mu.Unlock()
	if operations.mountCalls != 0 || operations.unmountCalls != 0 || operations.forceUnmountCalls != 0 {
		t.Fatalf("foreign mount was mutated: %#v", operations)
	}
}

func TestNativeFSKitSupervisorStatusCarriesOrderedBackendIdentity(t *testing.T) {
	options := NativeFSKitSupervisorOptions{
		ResourcePath: "/tmp/resource",
		MountPoint:   "/tmp/mount",
	}
	tracker := newNativeFSKitSupervisorStatusTracker(options)
	first := tracker.snapshot(options, "recovering", "starting", nil)
	second := tracker.snapshot(options, "healthy", "available", nil)
	if first.PublisherInstanceID == "" || first.BackendID == "" {
		t.Fatalf("supervisor status identity is incomplete: %#v", first)
	}
	if second.PublisherInstanceID != first.PublisherInstanceID || second.BackendID != first.BackendID {
		t.Fatalf("supervisor status identity changed within one publisher epoch: first=%#v second=%#v", first, second)
	}
	if first.ObservationSequence != 1 || second.ObservationSequence != 2 {
		t.Fatalf("supervisor status sequence = %d,%d, want 1,2", first.ObservationSequence, second.ObservationSequence)
	}
	if first.MountPoint != options.MountPoint || first.ResourcePath != options.ResourcePath {
		t.Fatalf("supervisor backend paths = %q,%q", first.MountPoint, first.ResourcePath)
	}
}

func TestNativeFSKitSupervisorDoesNotMountWhenProbeStateIsUnknown(t *testing.T) {
	operations := &fakeNativeFSKitOperations{
		daemonHealthy: true,
		mountErr:      errors.New("temporary statfs failure"),
		mounted:       make(chan struct{}), forceUnmounted: make(chan struct{}),
	}
	options := NativeFSKitSupervisorOptions{
		ResourcePath: "/tmp/resource", MountPoint: "/tmp/mount",
		RecoveryTimeout: 100 * time.Millisecond, Operations: operations,
	}
	if err := reconcileNativeFSKit(context.Background(), options, operations, &nativeFSKitMountObservation{}); err == nil {
		t.Fatal("unknown mount state was treated as definitely unmounted")
	}
	operations.mu.Lock()
	defer operations.mu.Unlock()
	if operations.mountCalls != 0 {
		t.Fatalf("mount calls = %d, want 0", operations.mountCalls)
	}
}

func TestNativeFSKitSupervisorDoesNotPublishProbeSlotContentionAsRecovery(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "supervisor.json")
	operations := &probeContentionNativeFSKitOperations{busy: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunNativeFSKitSupervisor(ctx, NativeFSKitSupervisorOptions{
			ResourcePath: "/tmp/resource", MountPoint: "/tmp/mount", StatusPath: statusPath,
			Interval: 5 * time.Millisecond, ProbeTimeout: time.Millisecond, Operations: operations,
		})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("supervisor shutdown: %v", err)
		}
	}()

	deadline := time.Now().Add(time.Second)
	for {
		snapshot, err := fskitstatus.Read(statusPath)
		if err == nil && snapshot.State == "healthy" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("supervisor did not publish initial healthy state: snapshot=%#v err=%v", snapshot, err)
		}
		time.Sleep(time.Millisecond)
	}
	operations.allowBusy.Store(true)
	select {
	case <-operations.busy:
	case <-time.After(time.Second):
		t.Fatal("supervisor did not observe probe slot contention")
	}
	time.Sleep(20 * time.Millisecond)
	snapshot, err := fskitstatus.Read(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != "healthy" {
		t.Fatalf("probe slot contention changed supervisor state: %#v", snapshot)
	}
}

func TestWaitForNativeFSKitMountDoesNotAcceptHealthyStateWithProbeError(t *testing.T) {
	probeErr := errors.New("temporary statfs failure")
	operations := &fakeNativeFSKitOperations{
		daemonHealthy: true,
		mountErr:      probeErr,
		state:         NativeFSKitMountState{Mounted: true, Owned: true, Healthy: true},
	}
	options := NativeFSKitSupervisorOptions{
		ResourcePath: "/tmp/resource", MountPoint: "/tmp/mount",
		RecoveryTimeout: 5 * time.Millisecond, Operations: operations,
	}
	err := waitForNativeFSKitMount(context.Background(), options, operations)
	if err == nil {
		t.Fatal("mount became healthy despite a failed mount-state probe")
	}
	if !errors.Is(err, probeErr) {
		t.Fatalf("wait error = %v, want probe error", err)
	}
}

type fakeNativeFSKitOperations struct {
	mu sync.Mutex

	daemonHealthy bool
	daemonErr     error
	mountErr      error
	state         NativeFSKitMountState

	probeCalls        int
	mountCalls        int
	unmountCalls      int
	forceUnmountCalls int

	mounted        chan struct{}
	forceUnmounted chan struct{}
}

type probeContentionNativeFSKitOperations struct {
	allowBusy atomic.Bool
	busy      chan struct{}
}

func (*probeContentionNativeFSKitOperations) DaemonHealthy(context.Context, string) error {
	return nil
}

func (p *probeContentionNativeFSKitOperations) MountState(context.Context, string, time.Duration) (NativeFSKitMountState, error) {
	if p.allowBusy.Load() {
		select {
		case p.busy <- struct{}{}:
		default:
		}
		return NativeFSKitMountState{}, ErrNativeFSKitMountProbeInProgress
	}
	return NativeFSKitMountState{Mounted: true, Owned: true, Healthy: true}, nil
}

func (*probeContentionNativeFSKitOperations) Mount(context.Context, string, string) error {
	return errors.New("unexpected mount")
}

func (*probeContentionNativeFSKitOperations) Unmount(context.Context, string, bool) error {
	return errors.New("unexpected unmount")
}

func (f *fakeNativeFSKitOperations) DaemonHealthy(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.daemonErr != nil {
		return f.daemonErr
	}
	if !f.daemonHealthy {
		return errors.New("daemon unavailable")
	}
	return nil
}

func (f *fakeNativeFSKitOperations) MountState(context.Context, string, time.Duration) (NativeFSKitMountState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeCalls++
	return f.state, f.mountErr
}

func (f *fakeNativeFSKitOperations) Mount(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mountCalls++
	f.state = NativeFSKitMountState{Mounted: true, Owned: true, Healthy: true}
	select {
	case <-f.mounted:
	default:
		close(f.mounted)
	}
	return nil
}

func (f *fakeNativeFSKitOperations) Unmount(_ context.Context, _ string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordUnmount(force)
	if f.state.Owned {
		f.state = NativeFSKitMountState{}
	}
	return nil
}

func (f *fakeNativeFSKitOperations) recordUnmount(force bool) {
	if force {
		f.forceUnmountCalls++
		select {
		case <-f.forceUnmounted:
		default:
			close(f.forceUnmounted)
		}
		return
	}
	f.unmountCalls++
}

// stallingNativeFSKitOperations answers the first probes normally and then
// blocks inside MountState, reproducing a kernel mount-table lookup or a mount
// recovery that outlives the UI's status freshness window.
type stallingNativeFSKitOperations struct {
	stall     atomic.Bool
	stalling  chan struct{}
	release   chan struct{}
	releasing sync.Once
}

func (*stallingNativeFSKitOperations) DaemonHealthy(context.Context, string) error { return nil }

func (s *stallingNativeFSKitOperations) MountState(context.Context, string, time.Duration) (NativeFSKitMountState, error) {
	if s.stall.Load() {
		select {
		case s.stalling <- struct{}{}:
		default:
		}
		<-s.release
	}
	return NativeFSKitMountState{Mounted: true, Owned: true, Healthy: true}, nil
}

func (*stallingNativeFSKitOperations) Mount(context.Context, string, string) error {
	return errors.New("unexpected mount")
}

func (*stallingNativeFSKitOperations) Unmount(context.Context, string, bool) error {
	return errors.New("unexpected unmount")
}

func (s *stallingNativeFSKitOperations) releaseProbe() {
	s.releasing.Do(func() { close(s.release) })
}

func TestNativeFSKitSupervisorKeepsPublishingWhileReconciliationStalls(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "supervisor.json")
	operations := &stallingNativeFSKitOperations{
		stalling: make(chan struct{}, 1),
		release:  make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunNativeFSKitSupervisor(ctx, NativeFSKitSupervisorOptions{
			ResourcePath: "/tmp/resource", MountPoint: "/tmp/mount", StatusPath: statusPath,
			Interval: 5 * time.Millisecond, ProbeTimeout: time.Millisecond, Operations: operations,
		})
	}()
	defer func() {
		operations.releaseProbe()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("supervisor shutdown: %v", err)
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot, err := fskitstatus.Read(statusPath)
		if err == nil && snapshot.State == "healthy" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("supervisor did not publish initial healthy state: snapshot=%#v err=%v", snapshot, err)
		}
		time.Sleep(time.Millisecond)
	}

	operations.stall.Store(true)
	select {
	case <-operations.stalling:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor never entered the stalled probe")
	}

	// Let the observation that was already in flight when the stall began drain,
	// so the baseline reflects a reconciliation that is genuinely wedged.
	time.Sleep(50 * time.Millisecond)
	stalled, err := fskitstatus.Read(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	// The reconciliation stays wedged for the rest of this check. A live
	// supervisor must still advance the channel, otherwise the UI escalates the
	// read gap to a "needs attention" incident while nothing is actually wrong.
	time.Sleep(100 * time.Millisecond)
	heartbeat, err := fskitstatus.Read(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if heartbeat.ObservationSequence <= stalled.ObservationSequence {
		t.Fatalf("status channel stalled with reconciliation: before=%d after=%d", stalled.ObservationSequence, heartbeat.ObservationSequence)
	}
	if heartbeat.State != "healthy" {
		t.Fatalf("stalled reconciliation changed the published state: %#v", heartbeat)
	}
}

// probeDeadlineNativeFSKitOperations answers the mount-table lookup with the
// deadline the real bounded probe reports under load, while the backend socket
// keeps answering normally.
type probeDeadlineNativeFSKitOperations struct {
	timeOut     atomic.Bool
	daemonErr   atomic.Bool
	timedOut    chan struct{}
	daemonCalls atomic.Int64
}

func (p *probeDeadlineNativeFSKitOperations) DaemonHealthy(context.Context, string) error {
	p.daemonCalls.Add(1)
	if p.daemonErr.Load() {
		return errors.New("connect to FSKit daemon: i/o timeout")
	}
	return nil
}

func (p *probeDeadlineNativeFSKitOperations) MountState(context.Context, string, time.Duration) (NativeFSKitMountState, error) {
	if p.timeOut.Load() {
		select {
		case p.timedOut <- struct{}{}:
		default:
		}
		return NativeFSKitMountState{}, fmt.Errorf("native FSKit mount health probe exceeded 2s: %w", context.DeadlineExceeded)
	}
	return NativeFSKitMountState{Mounted: true, Owned: true, Healthy: true}, nil
}

func (*probeDeadlineNativeFSKitOperations) Mount(context.Context, string, string) error {
	return errors.New("unexpected mount")
}

func (*probeDeadlineNativeFSKitOperations) Unmount(context.Context, string, bool) error {
	return errors.New("unexpected unmount")
}

func awaitNativeFSKitSupervisorState(t *testing.T, statusPath string, want string) fskitstatus.Snapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot, err := fskitstatus.Read(statusPath)
		if err == nil && snapshot.State == want {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("supervisor never published %q: snapshot=%#v err=%v", want, snapshot, err)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNativeFSKitSupervisorDoesNotReportProbeDeadlineAsRecoveryWhileBackendAnswers(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "supervisor.json")
	operations := &probeDeadlineNativeFSKitOperations{timedOut: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunNativeFSKitSupervisor(ctx, NativeFSKitSupervisorOptions{
			ResourcePath: "/tmp/resource", MountPoint: "/tmp/mount", StatusPath: statusPath,
			Interval: 5 * time.Millisecond, ProbeTimeout: time.Millisecond, Operations: operations,
		})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("supervisor shutdown: %v", err)
		}
	}()

	awaitNativeFSKitSupervisorState(t, statusPath, "healthy")

	operations.timeOut.Store(true)
	select {
	case <-operations.timedOut:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor never hit the probe deadline")
	}
	// The mount-table lookup keeps timing out for the rest of this window. The
	// backend still answers, so this is an unusable reading rather than an
	// outage and must not reach the user as one.
	time.Sleep(100 * time.Millisecond)
	snapshot, err := fskitstatus.Read(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != "healthy" {
		t.Fatalf("a probe deadline was reported as an outage: %#v", snapshot)
	}

	// A backend that stops answering is a real outage and must still be reported.
	operations.daemonErr.Store(true)
	awaitNativeFSKitSupervisorState(t, statusPath, "recovering")
}

// countingNativeFSKitOperations records how often each liveness signal is used
// so probe pressure can be asserted directly.
type countingNativeFSKitOperations struct {
	mountStateCalls atomic.Int64
	daemonCalls     atomic.Int64
	foreign         atomic.Bool
}

func (c *countingNativeFSKitOperations) DaemonHealthy(context.Context, string) error {
	c.daemonCalls.Add(1)
	return nil
}

func (c *countingNativeFSKitOperations) MountState(context.Context, string, time.Duration) (NativeFSKitMountState, error) {
	c.mountStateCalls.Add(1)
	if c.foreign.Load() {
		return NativeFSKitMountState{Mounted: true}, nil
	}
	return NativeFSKitMountState{Mounted: true, Owned: true, Healthy: true}, nil
}

func (*countingNativeFSKitOperations) Mount(context.Context, string, string) error {
	return errors.New("unexpected mount")
}

func (*countingNativeFSKitOperations) Unmount(context.Context, string, bool) error {
	return errors.New("unexpected unmount")
}

func TestNativeFSKitSupervisorDoesNotRereadTheMountTableEveryInterval(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "supervisor.json")
	operations := &countingNativeFSKitOperations{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunNativeFSKitSupervisor(ctx, NativeFSKitSupervisorOptions{
			ResourcePath: "/tmp/resource", MountPoint: "/tmp/mount", StatusPath: statusPath,
			Interval: 2 * time.Millisecond, MountObservationInterval: 200 * time.Millisecond,
			ProbeTimeout: time.Millisecond, Operations: operations,
		})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("supervisor shutdown: %v", err)
		}
	}()

	awaitNativeFSKitSupervisorState(t, statusPath, "healthy")
	time.Sleep(300 * time.Millisecond)

	probes := operations.mountStateCalls.Load()
	daemonChecks := operations.daemonCalls.Load()
	if daemonChecks < 20 {
		t.Fatalf("supervisor stopped reconciling: daemon checks=%d", daemonChecks)
	}
	// Reconciliation ran on a 2ms interval across ~300ms, so an uncached
	// supervisor would have read the mount table well over a hundred times.
	if probes > daemonChecks/4 {
		t.Fatalf("mount table was re-read on nearly every interval: probes=%d daemon checks=%d", probes, daemonChecks)
	}

	// A conclusive reading is still taken often enough to notice the mount being
	// taken over by another filesystem.
	operations.foreign.Store(true)
	awaitNativeFSKitSupervisorState(t, statusPath, "unavailable")
}

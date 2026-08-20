package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
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

func TestNativeFSKitSupervisorMountsAndUnmountsOnShutdown(t *testing.T) {
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
	if operations.mountCalls != 1 || operations.unmountCalls != 1 || operations.forceUnmountCalls != 0 {
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
	if operations.forceUnmountCalls != 0 || operations.unmountCalls != 1 || operations.probeCalls < 3 {
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
	if err := reconcileNativeFSKit(context.Background(), options, operations); err == nil {
		t.Fatal("unknown mount state was treated as definitely unmounted")
	}
	operations.mu.Lock()
	defer operations.mu.Unlock()
	if operations.mountCalls != 0 {
		t.Fatalf("mount calls = %d, want 0", operations.mountCalls)
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

func TestShutdownNativeFSKitReturnsMountStateErrorWithoutUnmounting(t *testing.T) {
	probeErr := errors.New("temporary statfs failure")
	operations := &fakeNativeFSKitOperations{
		mountErr: probeErr,
		state:    NativeFSKitMountState{Mounted: true, Owned: true, Healthy: true},
	}
	err := shutdownNativeFSKit(NativeFSKitSupervisorOptions{
		ResourcePath: "/tmp/resource", MountPoint: "/tmp/mount",
		RecoveryTimeout: time.Second, Operations: operations,
	}, operations)
	if err != probeErr {
		t.Fatalf("shutdown error = %v, want probe error", err)
	}
	operations.mu.Lock()
	defer operations.mu.Unlock()
	if operations.unmountCalls != 0 || operations.forceUnmountCalls != 0 {
		t.Fatalf("shutdown mutated mount after failed probe: unmount=%d force=%d", operations.unmountCalls, operations.forceUnmountCalls)
	}
}

func TestNativeFSKitSupervisorDoesNotReportStoppedWhenShutdownProbeFails(t *testing.T) {
	probeErr := errors.New("temporary statfs failure")
	operations := &fakeNativeFSKitOperations{
		mountErr: probeErr,
		state:    NativeFSKitMountState{Mounted: true, Owned: true, Healthy: true},
	}
	statusPath := filepath.Join(t.TempDir(), "supervisor.json")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := RunNativeFSKitSupervisor(ctx, NativeFSKitSupervisorOptions{
		ResourcePath: "/tmp/resource", MountPoint: "/tmp/mount",
		Interval: time.Millisecond, RecoveryTimeout: time.Second,
		StatusPath: statusPath, Operations: operations,
	})
	if err != probeErr {
		t.Fatalf("supervisor error = %v, want shutdown probe error", err)
	}
	payload, readErr := os.ReadFile(statusPath)
	if readErr != nil {
		t.Fatalf("read supervisor status: %v", readErr)
	}
	var snapshot fskitstatus.Snapshot
	if decodeErr := json.Unmarshal(payload, &snapshot); decodeErr != nil {
		t.Fatalf("decode supervisor status: %v", decodeErr)
	}
	if snapshot.State != "unavailable" {
		t.Fatalf("supervisor status = %q, want unavailable", snapshot.State)
	}
	if snapshot.PublisherInstanceID == "" || snapshot.BackendID == "" || snapshot.ObservationSequence == 0 {
		t.Fatalf("supervisor status lacks causal identity: %#v", snapshot)
	}
	operations.mu.Lock()
	defer operations.mu.Unlock()
	if operations.unmountCalls != 0 || operations.forceUnmountCalls != 0 {
		t.Fatalf("supervisor mutated mount after failed shutdown probe: unmount=%d force=%d", operations.unmountCalls, operations.forceUnmountCalls)
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

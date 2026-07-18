package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

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

func TestNativeFSKitSupervisorForceUnmountsStaleOwnedMountAfterTwoFailures(t *testing.T) {
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

	select {
	case <-operations.forceUnmounted:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("supervisor did not force-unmount stale owned mount")
	}
	if err := <-done; err != nil {
		t.Fatalf("supervisor shutdown: %v", err)
	}
	operations.mu.Lock()
	defer operations.mu.Unlock()
	if operations.forceUnmountCalls != 1 || operations.probeCalls < 2 {
		t.Fatalf("force unmount=%d probes=%d", operations.forceUnmountCalls, operations.probeCalls)
	}
}

func TestNativeFSKitSupervisorRefusesForeignMount(t *testing.T) {
	operations := &fakeNativeFSKitOperations{
		daemonHealthy: true,
		state:         NativeFSKitMountState{Mounted: true, Owned: false, Healthy: false},
		mounted:       make(chan struct{}), forceUnmounted: make(chan struct{}),
	}
	err := RunNativeFSKitSupervisor(context.Background(), NativeFSKitSupervisorOptions{
		ResourcePath: "/tmp/resource", MountPoint: "/tmp/mount",
		Interval: time.Millisecond, RecoveryTimeout: 100 * time.Millisecond,
		Operations: operations,
	})
	if !errors.Is(err, ErrForeignMount) {
		t.Fatalf("foreign mount error = %v", err)
	}
	operations.mu.Lock()
	defer operations.mu.Unlock()
	if operations.mountCalls != 0 || operations.unmountCalls != 0 || operations.forceUnmountCalls != 0 {
		t.Fatalf("foreign mount was mutated: %#v", operations)
	}
}

type fakeNativeFSKitOperations struct {
	mu sync.Mutex

	daemonHealthy bool
	daemonErr     error
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
	return f.state, nil
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

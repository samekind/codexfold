//go:build darwin

package service

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeNativeFSKitMountStateTreatsMissingMountPointAsUnmounted(t *testing.T) {
	state, err := probeNativeFSKitMountState(context.Background(), filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("missing mount point probe: %v", err)
	}
	if state.Mounted || state.Owned || state.Healthy {
		t.Fatalf("missing mount point state = %#v", state)
	}
}

func TestNativeFSKitMountArgumentsForceFSKitModule(t *testing.T) {
	want := []string{"-F", "-t", "codexfoldnative", "/tmp/resource", "/tmp/mount"}
	if got := nativeFSKitMountArguments("/tmp/resource", "/tmp/mount"); !reflect.DeepEqual(got, want) {
		t.Fatalf("mount arguments = %v, want %v", got, want)
	}
}

func TestBoundedNativeFSKitMountStateTimesOutWithoutStartingConcurrentHungProbes(t *testing.T) {
	probeSlot := make(chan struct{}, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	probe := func(context.Context) (NativeFSKitMountState, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return NativeFSKitMountState{Mounted: true, Owned: true, Healthy: true}, nil
	}

	start := time.Now()
	_, err := boundedNativeFSKitMountState(context.Background(), 20*time.Millisecond, probeSlot, probe)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first bounded probe error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("bounded probe returned after %s", elapsed)
	}
	select {
	case <-started:
	default:
		t.Fatal("bounded probe did not start the mount inspection")
	}

	_, err = boundedNativeFSKitMountState(context.Background(), 20*time.Millisecond, probeSlot, probe)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second bounded probe error = %v, want deadline exceeded", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent hung mount probes = %d, want 1", got)
	}

	close(release)
	deadline := time.Now().Add(time.Second)
	for len(probeSlot) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	state, err := boundedNativeFSKitMountState(context.Background(), time.Second, probeSlot, probe)
	if err != nil {
		t.Fatalf("probe after hung inspection completed: %v", err)
	}
	if !state.Mounted || !state.Owned || !state.Healthy {
		t.Fatalf("probe state after recovery = %#v", state)
	}
}

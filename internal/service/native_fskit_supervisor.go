package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"
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
	Operations      NativeFSKitOperations
	Event           func(string)
}

func RunNativeFSKitSupervisor(ctx context.Context, options NativeFSKitSupervisorOptions) error {
	if !filepath.IsAbs(options.ResourcePath) || !filepath.IsAbs(options.MountPoint) {
		return errors.New("absolute FSKit resource and mount paths are required")
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
	operations := options.Operations
	if operations == nil {
		var err error
		operations, err = defaultNativeFSKitOperations()
		if err != nil {
			return err
		}
	}
	state := nativeFSKitSupervisorState{}
	ticker := time.NewTicker(options.Interval)
	defer ticker.Stop()
	for {
		err := reconcileNativeFSKit(ctx, options, operations, &state)
		if errors.Is(err, ErrForeignMount) {
			return err
		}
		if err != nil && options.Event != nil {
			options.Event(err.Error())
		}
		select {
		case <-ctx.Done():
			return shutdownNativeFSKit(options, operations)
		case <-ticker.C:
		}
	}
}

type nativeFSKitSupervisorState struct {
	unhealthyOwnedMounts int
}

func reconcileNativeFSKit(
	ctx context.Context,
	options NativeFSKitSupervisorOptions,
	operations NativeFSKitOperations,
	state *nativeFSKitSupervisorState,
) error {
	mountState, mountErr := operations.MountState(ctx, options.MountPoint, options.ProbeTimeout)
	if mountState.Mounted && !mountState.Owned {
		return fmt.Errorf("%w: %s", ErrForeignMount, options.MountPoint)
	}
	daemonErr := operations.DaemonHealthy(ctx, options.ResourcePath)
	if mountState.Owned && mountState.Healthy && daemonErr == nil {
		state.unhealthyOwnedMounts = 0
		return nil
	}
	if mountState.Owned && !mountState.Healthy {
		state.unhealthyOwnedMounts++
		if state.unhealthyOwnedMounts < 2 {
			return errors.Join(mountErr, daemonErr, errors.New("owned FSKit mount failed its first health probe"))
		}
		if err := operations.Unmount(ctx, options.MountPoint, true); err != nil {
			return errors.Join(mountErr, daemonErr, fmt.Errorf("force-unmount stale FSKit mount: %w", err))
		}
		mountState = NativeFSKitMountState{}
		state.unhealthyOwnedMounts = 0
	}
	if daemonErr != nil {
		return errors.Join(mountErr, fmt.Errorf("FSKit daemon unavailable: %w", daemonErr))
	}
	if mountErr != nil && mountState.Mounted {
		return mountErr
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
		if mountState.Owned && mountState.Healthy && daemonErr == nil {
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
	if err != nil || !mountState.Owned {
		return nil
	}
	if unmountErr := operations.Unmount(ctx, options.MountPoint, false); unmountErr == nil {
		return nil
	}
	return operations.Unmount(ctx, options.MountPoint, true)
}

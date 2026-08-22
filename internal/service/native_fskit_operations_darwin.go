//go:build darwin

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/fskitproto"
	"github.com/samekind/codexfold/internal/mountid"
	"golang.org/x/sys/unix"
)

type nativeFSKitOperations struct {
	mountStateProbe chan struct{}
}

func defaultNativeFSKitOperations() (NativeFSKitOperations, error) {
	return &nativeFSKitOperations{mountStateProbe: make(chan struct{}, 1)}, nil
}

func (*nativeFSKitOperations) DaemonHealthy(ctx context.Context, resourcePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	client, err := fskitproto.DialResource(resourcePath, 2*time.Second)
	if err != nil {
		return err
	}
	defer client.Close()
	_, err = client.Call(fskitproto.OpPing, nil)
	return err
}

func (operations *nativeFSKitOperations) MountState(ctx context.Context, mountPoint string, timeout time.Duration) (NativeFSKitMountState, error) {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return boundedNativeFSKitMountState(ctx, timeout, operations.mountStateProbe, func(probeCtx context.Context) (NativeFSKitMountState, error) {
		return probeNativeFSKitMountState(probeCtx, mountPoint)
	})
}

type nativeFSKitMountStateResult struct {
	state NativeFSKitMountState
	err   error
}

func boundedNativeFSKitMountState(
	ctx context.Context,
	timeout time.Duration,
	probeSlot chan struct{},
	probe func(context.Context) (NativeFSKitMountState, error),
) (NativeFSKitMountState, error) {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case probeSlot <- struct{}{}:
	case <-probeCtx.Done():
		return NativeFSKitMountState{}, fmt.Errorf("native FSKit mount health probe did not start within %s: %w", timeout, probeCtx.Err())
	}
	result := make(chan nativeFSKitMountStateResult, 1)
	go func() {
		defer func() { <-probeSlot }()
		state, err := probe(probeCtx)
		result <- nativeFSKitMountStateResult{state: state, err: err}
	}()
	select {
	case result := <-result:
		return result.state, result.err
	case <-probeCtx.Done():
		return NativeFSKitMountState{}, fmt.Errorf("native FSKit mount health probe exceeded %s: %w", timeout, probeCtx.Err())
	}
}

func probeNativeFSKitMountState(ctx context.Context, mountPoint string) (NativeFSKitMountState, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(mountPoint, &stat); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return NativeFSKitMountState{}, nil
		}
		return NativeFSKitMountState{}, err
	}
	requested := canonicalMountPath(mountPoint)
	actual := canonicalMountPath(unix.ByteSliceToString(stat.Mntonname[:]))
	if requested != actual {
		return NativeFSKitMountState{}, nil
	}
	filesystem := strings.ToLower(unix.ByteSliceToString(stat.Fstypename[:]))
	state := NativeFSKitMountState{Mounted: true, Owned: filesystem == "codexfold"}
	if !state.Owned {
		return state, nil
	}
	for _, directory := range []string{"sessions", "archived_sessions"} {
		command := exec.CommandContext(ctx, "/usr/bin/stat", "-f", "%HT", filepath.Join(mountPoint, directory))
		if output, err := command.CombinedOutput(); err != nil {
			return state, fmt.Errorf("probe native FSKit directory %s: %w: %s", directory, err, strings.TrimSpace(string(output)))
		}
	}
	identity, err := os.ReadFile(filepath.Join(mountPoint, mountid.Path))
	if err != nil {
		return state, fmt.Errorf("read native FSKit mount identity: %w", err)
	}
	if err := mountid.Validate(identity); err != nil {
		return state, fmt.Errorf("validate native FSKit mount identity: %w", err)
	}
	state.Healthy = true
	return state, nil
}

func (*nativeFSKitOperations) Mount(ctx context.Context, resourcePath string, mountPoint string) error {
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		return err
	}
	output, err := exec.CommandContext(ctx, "/sbin/mount", nativeFSKitMountArguments(resourcePath, mountPoint)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func nativeFSKitMountArguments(resourcePath string, mountPoint string) []string {
	return []string{"-F", "-t", "codexfoldnative", resourcePath, mountPoint}
}

func (*nativeFSKitOperations) Unmount(ctx context.Context, mountPoint string, force bool) error {
	arguments := []string{mountPoint}
	if force {
		arguments = []string{"-f", mountPoint}
	}
	output, err := exec.CommandContext(ctx, "/sbin/umount", arguments...).CombinedOutput()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// UnmountNativeFSKit detaches an owned native FSKit mount that a stopped
// supervisor deliberately left behind. The supervisor no longer unmounts on
// shutdown (a restart must keep the session path present), so an explicit
// `stop` reclaims the mount here. A mount that is not owned by CodexFold, or
// one still serving a healthy daemon, is left untouched by the caller.
func UnmountNativeFSKit(ctx context.Context, mountPoint string, force bool) error {
	operations, err := defaultNativeFSKitOperations()
	if err != nil {
		return err
	}
	return operations.Unmount(ctx, mountPoint, force)
}

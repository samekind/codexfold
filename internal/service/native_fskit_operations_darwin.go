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
	"golang.org/x/sys/unix"
)

type nativeFSKitOperations struct {
	mountStateProbe chan struct{}
	filesystemType  string
}

func defaultNativeFSKitOperations() (NativeFSKitOperations, error) {
	return nativeFSKitOperationsForType(NativeFSKitMountType)
}

func nativeFSKitOperationsForType(filesystemType string) (NativeFSKitOperations, error) {
	if !ValidNativeFSKitMountType(filesystemType) {
		return nil, errors.New("invalid native FSKit mount type")
	}
	return &nativeFSKitOperations{
		mountStateProbe: make(chan struct{}, 1),
		filesystemType:  filesystemType,
	}, nil
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
	default:
		return NativeFSKitMountState{}, ErrNativeFSKitMountProbeInProgress
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

func probeNativeFSKitMountState(_ context.Context, mountPoint string) (NativeFSKitMountState, error) {
	return lookupNativeFSKitMountState(mountPoint)
}

// lookupNativeFSKitMountState reads the kernel mount table with MNT_NOWAIT.
// Exact mount paths must be matched without filesystem access, including
// EvalSymlinks (which performs Lstat). Probing the live FSKit mount competes
// with Codex I/O and can turn a healthy but busy mount into a recovery incident.
// Only an unmatched caller-supplied alias may require path resolution.
func lookupNativeFSKitMountState(mountPoint string) (NativeFSKitMountState, error) {
	count, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return NativeFSKitMountState{}, err
	}
	stats := make([]unix.Statfs_t, count+8)
	count, err = unix.Getfsstat(stats, unix.MNT_NOWAIT)
	if err != nil {
		return NativeFSKitMountState{}, err
	}
	return nativeFSKitMountStateFromTable(mountPoint, stats[:count], canonicalMountPath), nil
}

func nativeFSKitMountStateFromTable(
	mountPoint string,
	stats []unix.Statfs_t,
	resolvePath func(string) string,
) NativeFSKitMountState {
	lookup := func(wanted string) NativeFSKitMountState {
		for _, stat := range stats {
			// Kernel mount names are already authoritative. Resolving them can
			// block on unrelated network/removable volumes, not just our mount.
			if filepath.Clean(unix.ByteSliceToString(stat.Mntonname[:])) != wanted {
				continue
			}
			filesystem := strings.ToLower(unix.ByteSliceToString(stat.Fstypename[:]))
			owned := filesystem == "codexfold"
			return NativeFSKitMountState{Mounted: true, Owned: owned, Healthy: owned}
		}
		return NativeFSKitMountState{}
	}

	wanted := filepath.Clean(mountPoint)
	if state := lookup(wanted); state.Mounted {
		return state
	}
	if len(stats) == 0 {
		return NativeFSKitMountState{}
	}
	// Preserve aliases such as /tmp -> /private/tmp without resolving any
	// entry in the mount table or touching an already matched mount root.
	if resolved := filepath.Clean(resolvePath(mountPoint)); resolved != wanted {
		return lookup(resolved)
	}
	return NativeFSKitMountState{}
}

func (operations *nativeFSKitOperations) Mount(ctx context.Context, resourcePath string, mountPoint string) error {
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		return err
	}
	output, err := exec.CommandContext(ctx, "/sbin/mount", nativeFSKitMountArguments(operations.filesystemType, resourcePath, mountPoint)...).CombinedOutput()
	if err != nil {
		return nativeFSKitMountError(err, output)
	}
	return nil
}

func nativeFSKitMountError(commandErr error, output []byte) error {
	detail := strings.TrimSpace(string(output))
	if strings.Contains(detail, "Module ") && strings.Contains(detail, " is disabled") {
		return fmt.Errorf("%w: FSKit module is disabled by macOS: %s; enable CodexFoldFSKit FSKit Modules in System Settings > General > Login Items & Extensions, then retry", commandErr, detail)
	}
	return fmt.Errorf("%w: %s", commandErr, detail)
}

func nativeFSKitMountArguments(filesystemType string, resourcePath string, mountPoint string) []string {
	// The mount type is part of the signed FSKit module's Info.plist. An
	// environment-only suffix does not register a second personality and makes
	// fskitd reject the module as disabled.
	return []string{"-F", "-t", filesystemType, resourcePath, mountPoint}
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

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

	"github.com/jstar0/codexfold/internal/fskitproto"
	"github.com/jstar0/codexfold/internal/mountid"
	"golang.org/x/sys/unix"
)

type nativeFSKitOperations struct{}

func defaultNativeFSKitOperations() (NativeFSKitOperations, error) {
	return nativeFSKitOperations{}, nil
}

func (nativeFSKitOperations) DaemonHealthy(ctx context.Context, resourcePath string) error {
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

func (nativeFSKitOperations) MountState(ctx context.Context, mountPoint string, timeout time.Duration) (NativeFSKitMountState, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(mountPoint, &stat); err != nil {
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
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, directory := range []string{"sessions", "archived_sessions"} {
		command := exec.CommandContext(probeCtx, "/usr/bin/stat", "-f", "%HT", filepath.Join(mountPoint, directory))
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

func (nativeFSKitOperations) Mount(ctx context.Context, resourcePath string, mountPoint string) error {
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		return err
	}
	output, err := exec.CommandContext(ctx, "/sbin/mount", "-t", "codexfoldnative", resourcePath, mountPoint).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (nativeFSKitOperations) Unmount(ctx context.Context, mountPoint string, force bool) error {
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

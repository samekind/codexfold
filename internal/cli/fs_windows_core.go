//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/samekind/codexfold/internal/enroll"
	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/service"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type windowsCoreBootKey struct{}

func prepareWindowsStorageEngine(command *cobra.Command) (func(), error) {
	boot, err := mountfs.ReadWindowsCoreBoot(os.Stdin)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(command.Context())
	ctx = context.WithValue(ctx, windowsCoreBootKey{}, boot)
	command.SetContext(ctx)
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); cancel() }()
	return cancel, nil
}

func serveWindowsStorageEngine(ctx context.Context, command *cobra.Command, filesystem *mountfs.Filesystem, onReady func()) error {
	boot, ok := command.Context().Value(windowsCoreBootKey{}).(mountfs.WindowsCoreBoot)
	if !ok {
		return errors.New("storage engine has no private parent binding")
	}
	return mountfs.ServeWindowsCore(ctx, filesystem, boot, onReady)
}

func serveWindowsResidentHost(command *cobra.Command, home, store, mount string, foreground bool) error {
	lock, err := service.AcquireProcessLock(filepath.Join(store, "fs", "frontend.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	root := filepath.Join(store, "fs", "windows-core")
	if os.Getenv("CODEXFOLD_WINDOWS_GLOBAL_MOUNT") == "1" {
		// SYSTEM never trusts an executable pointer from the user's store.
		root = filepath.Join(filepath.Dir(binary), "Core")
	}
	arguments := []string{"fs", "serve"}
	command.Flags().Visit(func(flag *pflag.Flag) {
		if flag.Name != "frontend" {
			arguments = append(arguments, "--"+flag.Name+"="+flag.Value.String())
		}
	})
	arguments = append(arguments, "--frontend=windows-engine")
	host, err := mountfs.NewWindowsCoreHost(command.Context(), mountfs.WindowsCoreOptions{
		Binary: binary, Root: root, Store: store, Arguments: arguments,
		Stdout: command.OutOrStdout(), Stderr: command.ErrOrStderr(),
		BeforeUpdate: func() error { return requireWindowsEnrollmentPaused(store) },
	})
	if err != nil {
		return err
	}
	defer host.Close()
	stopControl, err := host.ServeControl(command.Context())
	if err != nil {
		return err
	}
	defer stopControl()
	activity := &mountfs.IOActivityCounter{}
	var recorder func(string)
	if trace, _ := command.Flags().GetString("operation-trace"); trace != "" {
		var closer io.Closer
		recorder, closer, err = newOperationRecorder(trace)
		if err != nil {
			return err
		}
		defer closer.Close()
	}
	stopStatus := startPlatformDaemonStatus(command.Context(), store, mount, activity, command.ErrOrStderr())
	defer stopStatus()
	return mountfs.Mount(command.Context(), mountfs.HostOptions{
		MountPoint: mount, StorageRoot: store, NamespaceRoot: home, Backend: host,
		BackendBuild: host.BuildSHA256, BackendHealthy: host.Healthy,
		BuildSHA256: host.BuildSHA256(), Foreground: foreground, Activity: activity,
		OperationRecorder: recorder,
	})
}

func requireWindowsEnrollmentPaused(store string) error {
	for _, paths := range [][2]string{{enroll.ControlPath(store), enroll.ProgressPath(store)}, {enroll.WorkerControlPath(store), enroll.WorkerProgressPath(store)}} {
		policy, err := enroll.LoadControl(paths[0])
		if err != nil {
			return err
		}
		if policy.Present && policy.Enabled {
			return errors.New("pause automatic folding before a live engine update")
		}
		if _, err := os.Stat(paths[1]); errors.Is(err, os.ErrNotExist) && !policy.Present {
			continue
		} else if err != nil {
			return fmt.Errorf("cannot inspect the paused folding acknowledgement: %w", err)
		}
		progress, err := enroll.LoadProgress(paths[1])
		if errors.Is(err, os.ErrNotExist) && !policy.Present {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot verify the paused folding acknowledgement: %w", err)
		}
		if progress.Phase != enroll.PhaseDisabled || progress.UpdatedAt.IsZero() || time.Since(progress.UpdatedAt) > 30*time.Second || time.Until(progress.UpdatedAt) > 5*time.Second {
			return errors.New("wait for a fresh paused folding acknowledgement before a live engine update")
		}
	}
	return nil
}

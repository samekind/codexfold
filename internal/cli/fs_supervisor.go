package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"time"

	"github.com/jstar0/codexfold/internal/service"
	"github.com/spf13/cobra"
)

type FSNativeSupervisorResult struct {
	ResourcePath string        `json:"resource_path"`
	MountPoint   string        `json:"mount_point"`
	Interval     time.Duration `json:"interval"`
	ProbeTimeout time.Duration `json:"probe_timeout"`
	Recovery     time.Duration `json:"recovery_timeout"`
	DryRun       bool          `json:"dry_run"`
}

func newFSNativeSupervisorCommand() *cobra.Command {
	var resourcePath, mountPoint string
	var interval, probeTimeout, recoveryTimeout time.Duration
	var apply, jsonOutput bool
	command := &cobra.Command{
		Use:   "supervise",
		Short: "Keep the native FSKit mount healthy and remount it after daemon or extension failure",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if !filepath.IsAbs(resourcePath) || !filepath.IsAbs(mountPoint) {
				return errors.New("absolute FSKit resource and mount paths are required")
			}
			if interval <= 0 || probeTimeout <= 0 || recoveryTimeout <= 0 {
				return errors.New("supervisor timing values must be positive")
			}
			result := FSNativeSupervisorResult{
				ResourcePath: filepath.Clean(resourcePath), MountPoint: filepath.Clean(mountPoint),
				Interval: interval, ProbeTimeout: probeTimeout, Recovery: recoveryTimeout, DryRun: !apply,
			}
			if !apply {
				if jsonOutput {
					return writeJSON(command, result)
				}
				_, err := fmt.Fprintf(command.OutOrStdout(), "dry_run=true resource=%s mount=%s interval=%s probe_timeout=%s recovery_timeout=%s\n", result.ResourcePath, result.MountPoint, result.Interval, result.ProbeTimeout, result.Recovery)
				return err
			}
			if runtime.GOOS != "darwin" {
				return errors.New("native FSKit supervision is available only on macOS")
			}
			processLock, err := service.AcquireProcessLock(filepath.Join(result.ResourcePath, service.NativeFSKitSupervisorLockName))
			if err != nil {
				return err
			}
			defer processLock.Close()
			return service.RunNativeFSKitSupervisor(command.Context(), service.NativeFSKitSupervisorOptions{
				ResourcePath: result.ResourcePath, MountPoint: result.MountPoint,
				Interval: result.Interval, ProbeTimeout: result.ProbeTimeout, RecoveryTimeout: result.Recovery,
				Event: func(message string) {
					_, _ = fmt.Fprintf(command.ErrOrStderr(), "native-fskit supervisor: %s\n", message)
				},
			})
		},
	}
	command.Flags().StringVar(&resourcePath, "resource", "", "Absolute native FSKit resource descriptor path")
	command.Flags().StringVar(&mountPoint, "mount", "", "Absolute native FSKit mount point")
	command.Flags().DurationVar(&interval, "interval", time.Second, "Health reconciliation interval")
	command.Flags().DurationVar(&probeTimeout, "probe-timeout", 2*time.Second, "Maximum duration of one mount health probe")
	command.Flags().DurationVar(&recoveryTimeout, "recovery-timeout", 15*time.Second, "Maximum duration of one mount or unmount recovery")
	command.Flags().BoolVar(&apply, "apply", false, "Run the native FSKit mount supervisor")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output for dry-run")
	return command
}

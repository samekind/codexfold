//go:build darwin

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/buildid"
	"github.com/samekind/codexfold/internal/enroll"
	"github.com/samekind/codexfold/internal/fskitstatus"
	"github.com/samekind/codexfold/internal/mountid"
	"github.com/samekind/codexfold/internal/service"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

type FSServiceLiveUpdateResult struct {
	FSServiceBinaryUpdateResult
	PreviousDaemonPID    int    `json:"previous_daemon_pid,omitempty"`
	ReplacementDaemonPID int    `json:"replacement_daemon_pid,omitempty"`
	MountPreserved       bool   `json:"mount_preserved"`
	RecoveryBinaryPath   string `json:"recovery_binary_path,omitempty"`
}

// The frontend retries a missing backend for ten seconds. Give a verified
// candidate almost that full window, since a premature rollback of the old
// binary takes much longer at production scale. Rollback has its own deadline;
// if needed, the user-facing outage alert still fires while it recovers.
const (
	liveDaemonTakeoverTimeout = 9 * time.Second
	liveDaemonRollbackTimeout = 2 * time.Minute
)

func addLiveDaemonUpdateCommand(parent *cobra.Command) {
	parent.AddCommand(newFSServiceUpdateDaemonLiveCommand())
}

// update-daemon-live intentionally does not call stopPlatformService: that
// command also stops supervision and unmounts the canonical namespace.
func newFSServiceUpdateDaemonLiveCommand() *cobra.Command {
	var definitionPath, mountPoint string
	var apply, jsonOutput bool
	command := &cobra.Command{
		Use:   "update-daemon-live <candidate>",
		Short: "Replace only the native FSKit daemon while keeping Codex and its mount running",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			candidate, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			definition, err := resolveServiceDefinitionPath(definitionPath)
			if err != nil {
				return err
			}
			platform, err := service.CurrentPlatform()
			if err != nil {
				return err
			}
			if platform != service.PlatformLaunchd {
				return errors.New("live daemon update requires macOS launchd")
			}
			frontend, err := service.DefinitionFrontend(platform, definition)
			if err != nil {
				return err
			}
			if frontend != "native-fskit" {
				return errors.New("live daemon update requires the native FSKit frontend")
			}
			target, err := service.DefinitionBinary(platform, definition)
			if err != nil {
				return err
			}
			mount := mountPoint
			if mount == "" {
				mount, err = service.DefinitionMountPoint(platform, definition)
				if err != nil {
					return err
				}
			}
			if !filepath.IsAbs(mount) {
				return errors.New("absolute native FSKit mount path is required")
			}
			mount = filepath.Clean(mount)
			oldSHA, err := buildid.FileSHA256(target)
			if err != nil {
				return err
			}
			newSHA, err := buildid.FileSHA256(candidate)
			if err != nil {
				return err
			}
			result := FSServiceLiveUpdateResult{FSServiceBinaryUpdateResult: FSServiceBinaryUpdateResult{
				Candidate: candidate, Target: target, CurrentSHA256: oldSHA,
				CandidateSHA256: newSHA, Changed: oldSHA != newSHA, DryRun: !apply,
			}}
			if result.Changed {
				launcher, err := service.DefinitionLauncher(platform, definition)
				if err != nil {
					return err
				}
				if err := requireLaunchableServiceBinary(command.Context(), launcher, candidate); err != nil {
					return err
				}
				if err := requireLiveDaemonCandidateProtocol(command.Context(), candidate); err != nil {
					return err
				}
			}
			if apply && result.Changed {
				store, err := service.DefinitionStore(platform, definition)
				if err != nil {
					return err
				}
				if err := requireIdleEnrollmentForLiveUpdate(store); err != nil {
					return err
				}
				before, err := platformServiceStatus(command.Context(), platform, mount, definition)
				if err != nil {
					return err
				}
				if !before.DaemonRunning || !before.SupervisorRunning || !before.MountHealthy || !before.Build.Healthy || before.DaemonPID <= 0 || before.SupervisorPID <= 0 || before.Build.RunningBuildSHA256 != oldSHA {
					return fmt.Errorf("refusing live update without a healthy, identified daemon, supervisor and mount: %+v", before)
				}
				mountID, err := liveMountFSID(mount)
				if err != nil {
					return fmt.Errorf("read live mount identity: %w", err)
				}
				result.PreviousDaemonPID = before.DaemonPID
				lockPaths, err := nativeFSKitLaunchdLockPaths(definition)
				if err != nil {
					return err
				}
				recoveryDirectory := filepath.Join(filepath.Dir(target), "Recovery", "live-update-"+time.Now().UTC().Format("20060102-150405"))
				result.RecoveryBinaryPath, err = service.PreserveBinaryForRecovery(target, recoveryDirectory)
				if err != nil {
					return fmt.Errorf("preserve previous daemon binary: %w", err)
				}
				var replacement service.Status
				oldDaemonSignaled := false
				activate := func(expectedSHA string) error {
					// Staging may take time. If a cycle began meanwhile, roll the
					// binary path back without interrupting the old daemon.
					if err := requireIdleEnrollmentForLiveUpdate(store); err != nil {
						return err
					}
					current, err := service.InspectProcessLock(lockPaths.daemon)
					if err != nil {
						return err
					}
					if current.Held && current.PID == before.DaemonPID {
						if err := signalFSKitProcess(before.DaemonPID, unix.SIGTERM); err != nil && !errors.Is(err, unix.ESRCH) {
							return fmt.Errorf("stop old FSKit daemon: %w", err)
						}
						oldDaemonSignaled = true
					}
					replacement, err = waitForLiveDaemon(command.Context(), definition, mount, expectedSHA, before.DaemonPID, before.SupervisorPID, mountID, liveDaemonTakeoverTimeout)
					return err
				}
				restore := func(expectedSHA string) error {
					current, err := service.InspectProcessLock(lockPaths.daemon)
					if err != nil {
						return err
					}
					if current.Held && current.PID == before.DaemonPID && !oldDaemonSignaled {
						return nil // promotion failed before the old daemon was stopped
					}
					if current.Held && current.PID > 0 && current.PID != before.DaemonPID {
						if signalErr := signalFSKitProcess(current.PID, unix.SIGTERM); signalErr != nil && !errors.Is(signalErr, unix.ESRCH) {
							return fmt.Errorf("stop failed replacement daemon: %w", signalErr)
						}
					}
					_, err = waitForLiveDaemon(command.Context(), definition, mount, expectedSHA, before.DaemonPID, before.SupervisorPID, mountID, liveDaemonRollbackTimeout)
					return err
				}
				if err := replaceDaemonBinaryLive(candidate, target, activate, restore); err != nil {
					return fmt.Errorf("%w (previous binary preserved at %s)", err, result.RecoveryBinaryPath)
				}
				result.ReplacementDaemonPID = replacement.DaemonPID
				result.MountPreserved = true
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "dry_run=%t changed=%t old_daemon=%d new_daemon=%d mount_preserved=%t current=%s candidate=%s recovery=%s\n", result.DryRun, result.Changed, result.PreviousDaemonPID, result.ReplacementDaemonPID, result.MountPreserved, oldSHA, newSHA, result.RecoveryBinaryPath)
			return err
		},
	}
	addServiceDefinitionFlags(command, &definitionPath)
	command.Flags().StringVar(&mountPoint, "mount", "", "Native FSKit mount path from the service definition")
	command.Flags().BoolVar(&apply, "apply", false, "Replace only the daemon while preserving the active mount")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

// A valid signing team only proves that launchd can exec the file. Refuse a
// candidate that is not even a runnable filesystem backend before signaling
// the current daemon; an immediate-exit executable would otherwise force a
// production-scale cold rollback while the mounted frontend waits.
func requireLiveDaemonCandidateProtocol(ctx context.Context, candidate string) error {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(checkCtx, candidate, "fs", "serve", "--help").CombinedOutput()
	if err != nil {
		return fmt.Errorf("candidate filesystem backend smoke check failed: %w", err)
	}
	help := string(output)
	for _, required := range []string{"--canonical-namespace", "--frontend", "--fskit-resource", "--fskit-socket"} {
		if !strings.Contains(help, required) {
			return fmt.Errorf("candidate filesystem backend smoke check is missing %s", required)
		}
	}
	return nil
}

func requireIdleEnrollmentForLiveUpdate(store string) error {
	progress, err := enroll.LoadProgress(enroll.ProgressPath(store))
	if err != nil {
		return fmt.Errorf("cannot prove automatic folding is idle: %w", err)
	}
	switch progress.Phase {
	case enroll.PhaseIdle, enroll.PhaseWaitingReclaim, enroll.PhaseDisabled:
		return nil
	default:
		return fmt.Errorf("automatic folding is %s; wait for it to become idle before replacing the daemon", progress.Phase)
	}
}

func replaceDaemonBinaryLive(candidate, target string, activate, restore func(string) error) error {
	update, err := service.StageBinaryUpdate(candidate, target)
	if err != nil {
		return err
	}
	if err := update.Promote(); err != nil {
		rollbackErr := update.Rollback()
		if rollbackErr != nil {
			return errors.Join(err, rollbackErr) // keep the backup for recovery
		}
		return errors.Join(err, rollbackErr, update.Commit())
	}
	if err := activate(update.CandidateSHA256); err != nil {
		rollbackErr := update.Rollback()
		if rollbackErr != nil {
			return errors.Join(fmt.Errorf("replacement daemon did not take over: %w", err), rollbackErr) // keep the backup
		}
		var restoreErr error
		restoreErr = restore(update.CurrentSHA256)
		return errors.Join(fmt.Errorf("replacement daemon did not take over: %w", err), rollbackErr, restoreErr, update.Commit())
	}
	return update.Commit()
}

func liveMountFSID(mount string) (unix.Fsid, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(mount, &stat); err != nil {
		return unix.Fsid{}, err
	}
	return stat.Fsid, nil
}

func waitForLiveDaemon(ctx context.Context, definition, mount, expectedSHA string, previousPID, supervisorPID int, mountID unix.Fsid, timeout time.Duration) (service.Status, error) {
	lockPaths, err := nativeFSKitLaunchdLockPaths(definition)
	if err != nil {
		return service.Status{}, err
	}
	resource, err := service.DefinitionFSKitResource(service.PlatformLaunchd, definition)
	if err != nil {
		return service.Status{}, err
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last service.Status
	var lastErr error
	type buildProbeResult struct {
		sha string
		err error
	}
	var buildProbe <-chan buildProbeResult
	for {
		if buildProbe == nil {
			daemon, daemonErr := service.InspectProcessLock(lockPaths.daemon)
			supervisor, supervisorErr := service.InspectProcessLock(lockPaths.supervisor)
			if daemonErr != nil || supervisorErr != nil {
				lastErr = errors.Join(daemonErr, supervisorErr)
			} else {
				last.DaemonRunning, last.DaemonPID = daemon.Held, daemon.PID
				last.SupervisorRunning, last.SupervisorPID = supervisor.Held, supervisor.PID
				if supervisor.Held && supervisor.PID != supervisorPID {
					return last, errors.New("FSKit supervisor changed during daemon-only update")
				}
				if daemon.Held && daemon.PID > 0 && daemon.PID != previousPID && supervisor.Held {
					snapshot, readErr := fskitstatus.Read(service.FSKitStatusPath(resource, "daemon"))
					observedAt, timeErr := time.Parse(time.RFC3339Nano, snapshot.UpdatedAt)
					if readErr == nil && timeErr == nil && snapshot.PID == daemon.PID && snapshot.State == "healthy" && time.Since(observedAt) < 3*time.Second {
						probe := make(chan buildProbeResult, 1)
						buildProbe = probe
						go func() {
							bytes, err := os.ReadFile(filepath.Join(mount, mountid.Path))
							if err != nil {
								probe <- buildProbeResult{err: err}
								return
							}
							identity, err := mountid.Parse(bytes)
							probe <- buildProbeResult{sha: identity.BuildSHA256, err: err}
						}()
					} else {
						lastErr = errors.Join(readErr, timeErr)
					}
				}
			}
		}
		select {
		case result := <-buildProbe:
			buildProbe = nil
			if result.err != nil {
				lastErr = result.err
				break
			}
			if result.sha != expectedSHA {
				lastErr = fmt.Errorf("mounted daemon build %s differs from expected %s", result.sha, expectedSHA)
				break
			}
			mountNow, mountErr := liveMountFSID(mount)
			if mountErr != nil {
				lastErr = mountErr
				break
			}
			if mountNow != mountID {
				return last, errors.New("FSKit mount identity changed during daemon-only update")
			}
			last.MountHealthy = true
			last.Build = service.BuildStatus{Healthy: true, RunningBuildSHA256: result.sha}
			return last, nil
		case <-ctx.Done():
			return last, ctx.Err()
		case <-deadline.C:
			return last, fmt.Errorf("daemon-only update did not become healthy within %s: daemon=%t pid=%d supervisor=%t mount=%t build=%s error=%v", timeout, last.DaemonRunning, last.DaemonPID, last.SupervisorRunning, last.MountHealthy, last.Build.RunningBuildSHA256, lastErr)
		case <-ticker.C:
		}
	}
}

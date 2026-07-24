package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/samekind/codexfold/internal/buildid"
	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/service"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
	"github.com/spf13/cobra"
)

const serviceLabel = "com.codexfold.fs"

// Re-registering a signed FSKit extension after an in-place app Contents swap
// can take noticeably longer than an ordinary LaunchAgent restart. Keep the
// update transaction bounded, but allow the system extension service enough
// time to replace its prior endpoint before declaring a healthy mount failed.
const nativeFSKitStartupTimeout = 120 * time.Second

type operationTrace struct {
	mu   sync.Mutex
	file *os.File
}

func newOperationRecorder(path string) (func(string), io.Closer, error) {
	if !filepath.IsAbs(path) {
		return nil, nil, errors.New("operation trace path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, err
	}
	file, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, err
	}
	trace := &operationTrace{file: file}
	return trace.record, trace, nil
}

func (t *operationTrace) record(operation string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = fmt.Fprintf(t.file, "%d %s\n", time.Now().UnixNano(), operation)
}

func (t *operationTrace) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.file.Sync(); err != nil {
		_ = t.file.Close()
		return err
	}
	return t.file.Close()
}

type FSServiceActionResult struct {
	Action         string `json:"action"`
	Path           string `json:"path,omitempty"`
	SupervisorPath string `json:"supervisor_path,omitempty"`
	DryRun         bool   `json:"dry_run"`
}

type FSServiceInstallResult struct {
	Path                  string `json:"path"`
	DryRun                bool   `json:"dry_run"`
	Bytes                 int    `json:"bytes"`
	SupervisorPath        string `json:"supervisor_path,omitempty"`
	SupervisorBytes       int    `json:"supervisor_bytes,omitempty"`
	FSKitAppPath          string `json:"fskit_app_path,omitempty"`
	FSKitLauncherPath     string `json:"fskit_launcher_path,omitempty"`
	FSKitResourcePath     string `json:"fskit_resource_path,omitempty"`
	FSKitAppChanged       bool   `json:"fskit_app_changed,omitempty"`
	BinarySourcePath      string `json:"binary_source_path,omitempty"`
	BinaryCurrentSHA256   string `json:"binary_current_sha256,omitempty"`
	BinaryCandidateSHA256 string `json:"binary_candidate_sha256,omitempty"`
	BinaryChanged         bool   `json:"binary_changed,omitempty"`
}

type FSServiceBinaryUpdateResult struct {
	Candidate       string `json:"candidate"`
	Target          string `json:"target"`
	CurrentSHA256   string `json:"current_sha256"`
	CandidateSHA256 string `json:"candidate_sha256"`
	Changed         bool   `json:"changed"`
	DryRun          bool   `json:"dry_run"`
}

type FSUpdatePreflightResult struct {
	DoctorHealthy       bool                   `json:"doctor_healthy"`
	Compatibility       FSCompatibilityResult  `json:"compatibility"`
	Decision            service.UpdateDecision `json:"decision"`
	QuarantinedSessions int                    `json:"quarantined_sessions"`
}

func newFSServiceCommand() *cobra.Command {
	command := &cobra.Command{Use: "service", Short: "Manage the transparent filesystem service"}
	command.AddCommand(newFSServiceInstallCommand())
	command.AddCommand(newFSServiceStartCommand())
	command.AddCommand(newFSServiceStopCommand())
	command.AddCommand(newFSServiceRestartCommand())
	command.AddCommand(newFSServiceStatusCommand())
	command.AddCommand(newFSServiceUpdateBinaryCommand())
	command.AddCommand(newFSServiceUpdatePreflightCommand())
	addPlatformServiceCommands(command)
	return command
}

func newFSServiceUpdateBinaryCommand() *cobra.Command {
	var codexHome, mountPoint, definitionPath string
	var apply, jsonOutput bool
	command := &cobra.Command{
		Use:   "update-binary <candidate>",
		Short: "Atomically replace, restart, verify, and roll back the service binary",
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
			target, err := service.DefinitionBinary(platform, definition)
			if err != nil {
				return err
			}
			currentSHA256, err := buildid.FileSHA256(target)
			if err != nil {
				return err
			}
			candidateSHA256, err := buildid.FileSHA256(candidate)
			if err != nil {
				return err
			}
			result := FSServiceBinaryUpdateResult{
				Candidate: candidate, Target: target, CurrentSHA256: currentSHA256,
				CandidateSHA256: candidateSHA256, Changed: currentSHA256 != candidateSHA256, DryRun: !apply,
			}
			if apply && result.Changed {
				home, err := codex.ResolveHome(codexHome)
				if err != nil {
					return err
				}
				if err := requireFilesystemActivationAllowed(home); err != nil {
					return err
				}
				mount := defaultMountPoint(home, mountPoint)
				update, err := service.StageBinaryUpdate(candidate, target)
				if err != nil {
					return err
				}
				if err := stopPlatformService(command.Context(), platform, definition); err != nil {
					_ = update.Commit()
					return fmt.Errorf("stop filesystem service before binary update: %w", err)
				}
				if err := update.Promote(); err != nil {
					rollbackErr := update.Rollback()
					var cleanupErr error
					var restartErr error
					if rollbackErr == nil {
						cleanupErr = update.Commit()
						restartErr = startPlatformService(command.Context(), platform, definition, mount)
					}
					return errors.Join(fmt.Errorf("promote filesystem service binary: %w", err), rollbackErr, cleanupErr, restartErr)
				}
				if err := startPlatformService(command.Context(), platform, definition, mount); err != nil {
					_ = stopPlatformService(command.Context(), platform, definition)
					rollbackErr := update.Rollback()
					var restartErr error
					if rollbackErr == nil {
						restartErr = startPlatformService(command.Context(), platform, definition, mount)
						_ = update.Commit()
					}
					return errors.Join(fmt.Errorf("start verified filesystem service binary: %w", err), rollbackErr, restartErr)
				}
				if err := update.Commit(); err != nil {
					return fmt.Errorf("remove filesystem binary rollback artifact: %w", err)
				}
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "dry_run=%t changed=%t target=%s current=%s candidate=%s\n", result.DryRun, result.Changed, result.Target, result.CurrentSHA256, result.CandidateSHA256)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&mountPoint, "mount", "", "Mounted CodexFold filesystem path; defaults to <codex-home>/fold-fs")
	addServiceDefinitionFlags(command, &definitionPath)
	command.Flags().BoolVar(&apply, "apply", false, "Stop, replace, restart, and verify the service binary")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSServiceInstallCommand() *cobra.Command {
	var codexHome, storeDir, mountPoint, binaryPath, binarySource, plistPath, logDir, nativeRoot, operationTracePath string
	var frontend, fskitResource, fskitAppPath, fskitAppSource, label string
	var enrollmentInterval, enrollmentStableFor time.Duration
	var enrollmentBatchSize int
	var apply, canonicalNamespace, enrollmentCanary, jsonOutput bool
	command := &cobra.Command{
		Use:   "install",
		Short: "Render and optionally start the native platform service",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) (runErr error) {
			if label == "" {
				label = serviceLabel
			}
			home, store, mount, binary, plist, logs, err := resolveServicePaths(codexHome, storeDir, mountPoint, binaryPath, plistPath, logDir, label)
			if err != nil {
				return err
			}
			var binaryCurrentSHA256, binaryCandidateSHA256 string
			if binarySource != "" {
				binarySource, err = filepath.Abs(binarySource)
				if err != nil {
					return err
				}
				if filepath.Clean(binarySource) == filepath.Clean(binary) {
					return errors.New("candidate binary must be separate from the installed target")
				}
				binaryCurrentSHA256, err = buildid.FileSHA256(binary)
				if err != nil {
					return err
				}
				binaryCandidateSHA256, err = buildid.FileSHA256(binarySource)
				if err != nil {
					return err
				}
			}
			if apply {
				if err := requireFilesystemActivationAllowed(home); err != nil {
					return err
				}
			}
			platform, err := service.CurrentPlatform()
			if err != nil {
				return err
			}
			if frontend == "" {
				frontend = "fuse"
			}
			if label != serviceLabel && platform != service.PlatformLaunchd {
				return errors.New("custom service labels are supported only for macOS LaunchAgents")
			}
			hadExistingDefinition := false
			if apply {
				if info, statErr := os.Stat(plist); statErr == nil {
					if !info.Mode().IsRegular() {
						return errors.New("installed service definition is not a regular file")
					}
					hadExistingDefinition = true
				} else if !errors.Is(statErr, os.ErrNotExist) {
					return statErr
				}
			}
			if frontend == "native-fskit" {
				if platform != service.PlatformLaunchd {
					return errors.New("native-fskit service frontend is available only on macOS")
				}
				canonicalNamespace = true
				userHome, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				if fskitAppPath == "" {
					fskitAppPath = service.DefaultFSKitAppPath(userHome)
				}
				fskitAppPath, err = filepath.Abs(fskitAppPath)
				if err != nil {
					return err
				}
			}
			var appTransaction fsKitAppTransaction
			var binaryUpdate *service.BinaryUpdate
			var definitionUpdates []*service.DefinitionUpdate
			rollbackInstall := false
			restartPreviousService := false
			defer func() {
				if !rollbackInstall {
					return
				}
				rollbackContext, cancel := context.WithTimeout(context.Background(), 2*nativeFSKitStartupTimeout+15*time.Second)
				defer cancel()
				runErr = errors.Join(runErr, rollbackFailedServiceInstall(
					rollbackContext, platform, plist, mount, definitionUpdates, appTransaction, binaryUpdate,
					restartPreviousService, stopPlatformService, startPlatformService,
				))
			}()
			if apply && frontend == "native-fskit" && hadExistingDefinition {
				rollbackInstall = true
				restartPreviousService = true
				stopErr := stopPlatformService(command.Context(), platform, plist)
				if err := waitPlatformServiceInactive(command.Context(), platform, plist, mount, 30*time.Second); err != nil {
					return errors.Join(stopErr, err)
				}
			}
			if apply && binarySource != "" {
				binaryUpdate, err = service.StageBinaryUpdate(binarySource, binary)
				if err != nil {
					return err
				}
				rollbackInstall = true
			}
			if apply && frontend == "native-fskit" {
				if fskitAppSource != "" {
					fskitAppSource, err = filepath.Abs(fskitAppSource)
					if err != nil {
						return err
					}
				}
				appTransaction, err = prepareFSKitAppPlatform(command.Context(), fskitAppSource, fskitAppPath)
				if err != nil {
					return err
				}
				rollbackInstall = true
				restartPreviousService = hadExistingDefinition && appTransaction.Changed()
				if fskitResource == "" {
					fskitResource = filepath.Join(appTransaction.AppGroupPath(), service.FSKitResourceDirectoryName)
				}
			}
			if frontend == "native-fskit" && fskitResource == "" {
				userHome, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				fskitResource = service.DefaultFSKitResourcePath(userHome)
			}
			if frontend == "native-fskit" {
				if err := validateNativeFSKitSocketPath(defaultNativeFSKitSocket(home, fskitResource)); err != nil {
					return err
				}
			}
			if frontend == "native-fskit" && apply {
				within, err := filepath.Rel(appTransaction.AppGroupPath(), filepath.Clean(fskitResource))
				if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
					return errors.New("native FSKit resource must remain inside the configured App Group")
				}
			}
			launcherPath := ""
			if frontend == "native-fskit" {
				launcherPath, err = service.FSKitHostLauncherPath(fskitAppPath)
				if err != nil {
					return err
				}
			}
			if canonicalNamespace {
				if nativeRoot == "" {
					nativeRoot = filepath.Join(home, "fold-native")
				}
				if !filepath.IsAbs(nativeRoot) {
					return errors.New("canonical service native root must be absolute")
				}
				nativeRoot = filepath.Clean(nativeRoot)
			}
			if enrollmentCanary {
				userHome, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				if err := validateCompatibilityCanary(home, filepath.Join(userHome, ".codex"), store, canonicalNamespace, compatibilityFlags{cliPath: "none", desktopPath: "none"}); err != nil {
					return err
				}
			}
			options := service.Options{
				Label: label, BinaryPath: binary, CodexHome: home, StoreDir: store, MountPoint: mount,
				StdoutPath: filepath.Join(logs, "stdout.log"), StderrPath: filepath.Join(logs, "stderr.log"),
				CanonicalNamespace: canonicalNamespace, NativeRoot: nativeRoot, OperationTrace: operationTracePath,
				EnrollmentInterval: enrollmentInterval, EnrollmentStableFor: enrollmentStableFor,
				EnrollmentBatchSize: enrollmentBatchSize, EnrollmentCanary: enrollmentCanary,
				Frontend: frontend, FSKitResource: fskitResource, LauncherPath: launcherPath,
			}
			definition, err := service.RenderDefinition(platform, options)
			if err != nil {
				return err
			}
			if apply && frontend == "fuse" && !mountfs.Available() {
				return errors.New("service installation requires a platform FUSE build and an authorized host prerequisite")
			}
			if apply {
				if err := os.MkdirAll(logs, 0o700); err != nil {
					return err
				}
			}
			written := service.InstallResult{Path: plist, DryRun: !apply, Bytes: len(definition)}
			if apply {
				update, err := service.StageDefinitionUpdate(plist, definition)
				if err != nil {
					return err
				}
				definitionUpdates = append(definitionUpdates, update)
				rollbackInstall = true
			} else if _, err := service.WriteDefinition(plist, definition, false); err != nil {
				return err
			}
			result := FSServiceInstallResult{
				Path: written.Path, DryRun: written.DryRun, Bytes: written.Bytes,
				BinarySourcePath: binarySource, BinaryCurrentSHA256: binaryCurrentSHA256,
				BinaryCandidateSHA256: binaryCandidateSHA256,
				BinaryChanged:         binarySource != "" && binaryCurrentSHA256 != binaryCandidateSHA256,
			}
			if frontend == "native-fskit" {
				result.FSKitAppPath = fskitAppPath
				result.FSKitLauncherPath = launcherPath
				result.FSKitResourcePath = fskitResource
				if appTransaction != nil {
					result.FSKitAppChanged = appTransaction.Changed()
				}
				supervisorDefinition, err := service.RenderLaunchdSupervisor(options)
				if err != nil {
					return err
				}
				supervisorPath := nativeFSKitSupervisorDefinitionPath(plist)
				if apply {
					update, err := service.StageDefinitionUpdate(supervisorPath, supervisorDefinition)
					if err != nil {
						return err
					}
					definitionUpdates = append(definitionUpdates, update)
				} else if _, err := service.WriteDefinition(supervisorPath, supervisorDefinition, false); err != nil {
					return err
				}
				result.SupervisorPath = supervisorPath
				result.SupervisorBytes = len(supervisorDefinition)
			}
			if apply {
				for _, update := range definitionUpdates {
					if err := update.Promote(); err != nil {
						return err
					}
				}
				if binaryUpdate != nil {
					if err := binaryUpdate.Promote(); err != nil {
						return err
					}
				}
			}
			if apply {
				restartPreviousService = restartPreviousService || hadExistingDefinition
				if err := installPlatformService(command.Context(), platform, plist, binary, mount); err != nil {
					return err
				}
			}
			rollbackInstall = false
			cleanupErr := commitDefinitionUpdates(definitionUpdates)
			if appTransaction != nil {
				if err := appTransaction.Commit(); err != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove FSKit app rollback artifacts: %w", err))
				}
			}
			if binaryUpdate != nil {
				if err := binaryUpdate.Commit(); err != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove filesystem binary rollback artifacts: %w", err))
				}
			}
			if cleanupErr != nil {
				return cleanupErr
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "dry_run=%t path=%s bytes=%d supervisor=%s\n", result.DryRun, result.Path, result.Bytes, result.SupervisorPath)
			return err
		},
	}
	addServicePathFlags(command, &codexHome, &storeDir, &mountPoint, &binaryPath, &plistPath, &logDir)
	command.Flags().StringVar(&binarySource, "binary-source", "", "Executable candidate to atomically install at --binary")
	command.Flags().BoolVar(&canonicalNamespace, "canonical-namespace", false, "Start the service with the canonical Codex session namespace")
	command.Flags().StringVar(&frontend, "frontend", "fuse", "Filesystem frontend: fuse or native-fskit")
	command.Flags().StringVar(&label, "label", serviceLabel, "Service label; non-default labels are intended for isolated macOS validation")
	command.Flags().StringVar(&fskitResource, "fskit-resource", "", "Native FSKit resource inside the App Group; defaults to <App Group>/native-fskit")
	command.Flags().StringVar(&fskitAppPath, "fskit-app", "", "Installed signed FSKit app; defaults to ~/Applications/CodexFoldFSKit.app")
	command.Flags().StringVar(&fskitAppSource, "fskit-app-source", "", "Signed FSKit app candidate to atomically install or update at --fskit-app")
	command.Flags().StringVar(&nativeRoot, "native-root", "", "Canonical native backing root; defaults to <codex-home>/fold-native")
	command.Flags().StringVar(&operationTracePath, "operation-trace", "", "Absolute path for sanitized filesystem operation names")
	command.Flags().DurationVar(&enrollmentInterval, "enrollment-interval", 0, "Periodic stable-session enrollment interval; zero disables the loop")
	command.Flags().DurationVar(&enrollmentStableFor, "enrollment-stable-for", time.Hour, "Required unchanged interval before periodic enrollment")
	command.Flags().IntVar(&enrollmentBatchSize, "enrollment-batch-size", 1, "Maximum sessions enrolled per periodic cycle")
	command.Flags().BoolVar(&enrollmentCanary, "enrollment-canary", false, "Enable periodic apply only for an explicitly isolated Codex home while capability remains preview")
	command.Flags().BoolVar(&apply, "apply", false, "Write, install, and start the native platform service")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func rollbackDefinitionUpdates(updates []*service.DefinitionUpdate) error {
	var result error
	for index := len(updates) - 1; index >= 0; index-- {
		result = errors.Join(result, updates[index].Rollback())
	}
	return errors.Join(result, commitDefinitionUpdates(updates))
}

func commitDefinitionUpdates(updates []*service.DefinitionUpdate) error {
	var result error
	for _, update := range updates {
		result = errors.Join(result, update.Commit())
	}
	return result
}

type stopServiceOperation func(context.Context, service.Platform, string) error
type startServiceOperation func(context.Context, service.Platform, string, string) error

func rollbackFailedServiceInstall(
	ctx context.Context,
	platform service.Platform,
	definitionPath string,
	mountPoint string,
	definitionUpdates []*service.DefinitionUpdate,
	appTransaction fsKitAppTransaction,
	binaryUpdate *service.BinaryUpdate,
	restartPreviousService bool,
	stop stopServiceOperation,
	start startServiceOperation,
) error {
	if restartPreviousService && stop != nil {
		// A failed start normally booted the jobs out already. This extra stop is
		// best effort so rollback can also recover failures before the health gate.
		_ = stop(ctx, platform, definitionPath)
	}
	result := rollbackDefinitionUpdates(definitionUpdates)
	if appTransaction != nil {
		result = errors.Join(result, appTransaction.Rollback(ctx))
	}
	if binaryUpdate != nil {
		result = errors.Join(result, binaryUpdate.Rollback(), binaryUpdate.Commit())
	}
	if restartPreviousService {
		if start == nil {
			result = errors.Join(result, errors.New("service rollback restart operation is unavailable"))
		} else {
			result = errors.Join(result, start(ctx, platform, definitionPath, mountPoint))
		}
	}
	return result
}

func waitPlatformServiceInactive(ctx context.Context, platform service.Platform, definitionPath string, mountPoint string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	var lockPaths nativeFSKitProcessLockPaths
	checkLocks := false
	if platform == service.PlatformLaunchd {
		frontend, err := service.DefinitionFrontend(platform, definitionPath)
		if err != nil {
			return err
		}
		if frontend == "native-fskit" {
			lockPaths, err = nativeFSKitLaunchdLockPaths(definitionPath)
			if err != nil {
				return err
			}
			checkLocks = true
		}
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var daemonLock, supervisorLock service.ProcessLockStatus
	var mountPresent bool
	var mountPresenceErr error
	for {
		status, err := platformServiceStatus(ctx, platform, mountPoint, definitionPath)
		if err != nil {
			return err
		}
		if checkLocks {
			daemonLock, err = service.InspectProcessLock(lockPaths.daemon)
			if err != nil {
				return fmt.Errorf("inspect daemon process lock: %w", err)
			}
			supervisorLock, err = service.InspectProcessLock(lockPaths.supervisor)
			if err != nil {
				return fmt.Errorf("inspect supervisor process lock: %w", err)
			}
			mountPresent, mountPresenceErr = service.MountPresent(mountPoint)
		}
		if nativeFSKitServiceInactive(status, daemonLock, supervisorLock, mountPresent, mountPresenceErr) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf(
				"previous filesystem service did not stop cleanly: daemon=%t supervisor=%t mount_healthy=%t mount_present=%t mount_presence_error=%q daemon_lock=%t daemon_lock_pid=%d supervisor_lock=%t supervisor_lock_pid=%d",
				status.DaemonRunning, status.SupervisorRunning, status.MountHealthy,
				mountPresent, errorString(mountPresenceErr),
				daemonLock.Held, daemonLock.PID, supervisorLock.Held, supervisorLock.PID,
			)
		case <-ticker.C:
		}
	}
}

func nativeFSKitServiceInactive(status service.Status, daemonLock service.ProcessLockStatus, supervisorLock service.ProcessLockStatus, mountPresent bool, mountPresenceErr error) bool {
	return !status.DaemonRunning && !status.SupervisorRunning && !status.MountHealthy &&
		!mountPresent && mountPresenceErr == nil && !daemonLock.Held && !supervisorLock.Held
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func newFSServiceStartCommand() *cobra.Command {
	return newFSServiceLifecycleCommand("start", true)
}

func newFSServiceStopCommand() *cobra.Command {
	return newFSServiceLifecycleCommand("stop", false)
}

func newFSServiceRestartCommand() *cobra.Command {
	return newFSServiceLifecycleCommand("restart", true)
}

func newFSServiceLifecycleCommand(action string, waitForMount bool) *cobra.Command {
	var plistPath, codexHome, mountPoint string
	var apply, jsonOutput bool
	command := &cobra.Command{
		Use:   action,
		Short: action + " the filesystem service",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			definition, err := resolveServiceDefinitionPath(plistPath)
			if err != nil {
				return err
			}
			var home string
			if apply && (action == "start" || action == "restart") {
				home, err = codex.ResolveHome(codexHome)
				if err != nil {
					return err
				}
				if err := requireFilesystemActivationAllowed(home); err != nil {
					return err
				}
			}
			platform, err := service.CurrentPlatform()
			if err != nil {
				return err
			}
			result := FSServiceActionResult{Action: action, Path: definition, DryRun: !apply}
			if apply {
				frontend, err := service.DefinitionFrontend(platform, definition)
				if err != nil {
					return err
				}
				if frontend == "native-fskit" {
					result.SupervisorPath = nativeFSKitSupervisorDefinitionPath(definition)
				}
				if waitForMount && frontend == "fuse" && !mountfs.Available() {
					return errors.New("service start requires a platform FUSE build and an authorized host prerequisite")
				}
				if action == "start" {
					if err := startPlatformService(command.Context(), platform, definition, defaultMountPoint(home, mountPoint)); err != nil {
						return err
					}
				} else if action == "restart" {
					if err := stopPlatformService(command.Context(), platform, definition); err != nil {
						return err
					}
					if err := startPlatformService(command.Context(), platform, definition, defaultMountPoint(home, mountPoint)); err != nil {
						return err
					}
				} else {
					if err := stopPlatformService(command.Context(), platform, definition); err != nil {
						return err
					}
				}
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "action=%s dry_run=%t path=%s supervisor=%s\n", action, result.DryRun, definition, result.SupervisorPath)
			return err
		},
	}
	addServiceDefinitionFlags(command, &plistPath)
	if waitForMount {
		command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
		command.Flags().StringVar(&mountPoint, "mount", "", "Mounted CodexFold filesystem path; defaults to <codex-home>/fold-fs")
	}
	command.Flags().BoolVar(&apply, "apply", false, "Execute the native service-manager action")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSServiceStatusCommand() *cobra.Command {
	var codexHome, mountPoint, definitionPath string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "status",
		Short: "Report daemon and mount health separately",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			platform, err := service.CurrentPlatform()
			if err != nil {
				return err
			}
			definition, err := resolveServiceDefinitionPath(definitionPath)
			if err != nil {
				return err
			}
			status, err := platformServiceStatus(command.Context(), platform, defaultMountPoint(home, mountPoint), definition)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command, status)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "daemon=%t supervisor=%t mount=%t build=%t running_build=%s disk_build=%s binary=%s daemon_error=%q supervisor_error=%q mount_error=%q build_error=%q\n", status.DaemonRunning, status.SupervisorRunning, status.MountHealthy, status.Build.Healthy, status.Build.RunningBuildSHA256, status.Build.ConfiguredBuildSHA256, status.Build.ConfiguredBinaryPath, status.DaemonError, status.SupervisorError, status.MountError, status.Build.Error)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&mountPoint, "mount", "", "Mounted CodexFold filesystem path; defaults to <codex-home>/fold-fs")
	addServiceDefinitionFlags(command, &definitionPath)
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func installPlatformService(ctx context.Context, platform service.Platform, definitionPath string, binaryPath string, mountPoint string) error {
	label, err := service.DefinitionLabel(platform, definitionPath)
	if err != nil {
		return err
	}
	if platform == service.PlatformWindows {
		manager := service.WindowsManager{}
		_ = manager.Stop(ctx, label)
		if err := manager.Install(ctx, label, binaryPath, definitionPath); err != nil {
			return err
		}
	}
	return startPlatformService(ctx, platform, definitionPath, mountPoint)
}

func startPlatformService(ctx context.Context, platform service.Platform, definitionPath string, mountPoint string) error {
	frontend, err := service.DefinitionFrontend(platform, definitionPath)
	if err != nil {
		return err
	}
	label, err := service.DefinitionLabel(platform, definitionPath)
	if err != nil {
		return err
	}
	switch platform {
	case service.PlatformLaunchd:
		manager := service.Manager{}
		if frontend == "native-fskit" {
			supervisorLabel := nativeFSKitSupervisorLabel(label)
			supervisorDefinition := nativeFSKitSupervisorDefinitionPath(definitionPath)
			_ = manager.Bootout(ctx, supervisorDefinition)
			_ = manager.Bootout(ctx, definitionPath)
			if err := manager.Enable(ctx, label); err != nil {
				return err
			}
			if err := manager.Enable(ctx, supervisorLabel); err != nil {
				return err
			}
			if err := manager.Bootstrap(ctx, definitionPath); err != nil {
				return err
			}
			if err := manager.Kickstart(ctx, label); err != nil {
				_ = manager.Bootout(ctx, definitionPath)
				return err
			}
			if err := manager.Bootstrap(ctx, supervisorDefinition); err != nil {
				_ = manager.Bootout(ctx, definitionPath)
				return err
			}
			if err := manager.Kickstart(ctx, supervisorLabel); err != nil {
				_ = manager.Bootout(ctx, supervisorDefinition)
				_ = manager.Bootout(ctx, definitionPath)
				return err
			}
			if err := waitLaunchdNativeFSKitHealthy(ctx, manager, label, definitionPath, mountPoint, nativeFSKitStartupTimeout); err != nil {
				_ = manager.Bootout(ctx, supervisorDefinition)
				_ = manager.Bootout(ctx, definitionPath)
				return err
			}
			if err := verifyServiceBuild(service.PlatformLaunchd, definitionPath, mountPoint); err != nil {
				_ = manager.Bootout(ctx, supervisorDefinition)
				_ = manager.Bootout(ctx, definitionPath)
				return err
			}
			return nil
		}
		_ = manager.Bootout(ctx, definitionPath)
		if err := manager.Enable(ctx, label); err != nil {
			return err
		}
		if err := manager.Bootstrap(ctx, definitionPath); err != nil {
			return err
		}
		if err := manager.Kickstart(ctx, label); err != nil {
			_ = manager.Bootout(ctx, definitionPath)
			return err
		}
		if _, err := manager.WaitHealthy(ctx, label, mountPoint, 15*time.Second); err != nil {
			_ = manager.Bootout(ctx, definitionPath)
			return err
		}
		if err := verifyServiceBuild(service.PlatformLaunchd, definitionPath, mountPoint); err != nil {
			_ = manager.Bootout(ctx, definitionPath)
			return err
		}
		return nil
	case service.PlatformSystemd:
		unit, err := systemdServiceUnit(definitionPath, label)
		if err != nil {
			return err
		}
		manager := service.SystemdManager{}
		_ = manager.Stop(ctx, unit)
		if err := manager.Start(ctx, unit); err != nil {
			return err
		}
		if _, err := manager.WaitHealthy(ctx, unit, mountPoint, 15*time.Second); err != nil {
			_ = manager.Stop(ctx, unit)
			return err
		}
		if err := verifyServiceBuild(service.PlatformSystemd, definitionPath, mountPoint); err != nil {
			_ = manager.Stop(ctx, unit)
			return err
		}
		return nil
	case service.PlatformWindows:
		manager := service.WindowsManager{}
		_ = manager.Stop(ctx, label)
		if err := manager.Start(ctx, label); err != nil {
			return err
		}
		if _, err := manager.WaitHealthy(ctx, label, mountPoint, 15*time.Second); err != nil {
			_ = manager.Stop(ctx, label)
			return err
		}
		if err := verifyServiceBuild(service.PlatformWindows, definitionPath, mountPoint); err != nil {
			_ = manager.Stop(ctx, label)
			return err
		}
		return nil
	default:
		return errors.New("unknown service platform")
	}
}

func stopPlatformService(ctx context.Context, platform service.Platform, definitionPath string) error {
	frontend, err := service.DefinitionFrontend(platform, definitionPath)
	if err != nil {
		return err
	}
	label, err := service.DefinitionLabel(platform, definitionPath)
	if err != nil {
		return err
	}
	switch platform {
	case service.PlatformLaunchd:
		if frontend == "native-fskit" {
			manager := service.Manager{}
			supervisorErr := manager.Bootout(ctx, nativeFSKitSupervisorDefinitionPath(definitionPath))
			daemonErr := manager.Bootout(ctx, definitionPath)
			return errors.Join(supervisorErr, daemonErr)
		}
		return (service.Manager{}).Bootout(ctx, definitionPath)
	case service.PlatformSystemd:
		unit, err := systemdServiceUnit(definitionPath, label)
		if err != nil {
			return err
		}
		return (service.SystemdManager{}).Stop(ctx, unit)
	case service.PlatformWindows:
		return (service.WindowsManager{}).Stop(ctx, label)
	default:
		return errors.New("unknown service platform")
	}
}

func verifyServiceBuild(platform service.Platform, definitionPath string, mountPoint string) error {
	status := service.InspectBuild(platform, definitionPath, mountPoint)
	if !status.Healthy {
		return fmt.Errorf("filesystem service build verification failed: %s", status.Error)
	}
	return nil
}

func platformServiceStatus(ctx context.Context, platform service.Platform, mountPoint string, definitionPath string) (service.Status, error) {
	frontend, err := service.DefinitionFrontend(platform, definitionPath)
	if err != nil {
		return service.Status{}, err
	}
	label, err := service.DefinitionLabel(platform, definitionPath)
	if err != nil {
		return service.Status{}, err
	}
	var status service.Status
	switch platform {
	case service.PlatformLaunchd:
		manager := service.Manager{}
		status = manager.Status(ctx, label, mountPoint)
		if frontend == "native-fskit" {
			lockPaths, err := nativeFSKitLaunchdLockPaths(definitionPath)
			if err != nil {
				return service.Status{}, err
			}
			status = validateLaunchdChildProcess(status, lockPaths.daemon, "daemon")
			supervisor := validateLaunchdChildProcess(
				manager.Status(ctx, nativeFSKitSupervisorLabel(label), mountPoint),
				lockPaths.supervisor,
				"supervisor",
			)
			status.SupervisorRunning = supervisor.DaemonRunning
			status.SupervisorPID = supervisor.DaemonPID
			status.SupervisorError = supervisor.DaemonError
		}
	case service.PlatformSystemd:
		unit, err := service.SystemdUnitName(label)
		if err != nil {
			return service.Status{}, err
		}
		status = (service.SystemdManager{}).Status(ctx, unit, mountPoint)
	case service.PlatformWindows:
		status = (service.WindowsManager{}).Status(ctx, label, mountPoint)
	default:
		return service.Status{}, errors.New("unknown service platform")
	}
	status.Build = service.InspectBuild(platform, definitionPath, mountPoint)
	return status, nil
}

type nativeFSKitProcessLockPaths struct {
	daemon     string
	supervisor string
}

func nativeFSKitLaunchdLockPaths(definitionPath string) (nativeFSKitProcessLockPaths, error) {
	store, err := service.DefinitionStore(service.PlatformLaunchd, definitionPath)
	if err != nil {
		return nativeFSKitProcessLockPaths{}, err
	}
	resource, err := service.DefinitionFSKitResource(service.PlatformLaunchd, definitionPath)
	if err != nil {
		return nativeFSKitProcessLockPaths{}, err
	}
	return nativeFSKitProcessLockPaths{
		daemon:     filepath.Join(store, "fs", "service.lock"),
		supervisor: filepath.Join(resource, service.NativeFSKitSupervisorLockName),
	}, nil
}

func validateLaunchdChildProcess(status service.Status, lockPath string, role string) service.Status {
	if !status.DaemonRunning {
		return status
	}
	lockStatus, err := service.InspectProcessLock(lockPath)
	if err != nil {
		status.DaemonRunning = false
		status.DaemonError = fmt.Sprintf("inspect %s process lock: %v", role, err)
		return status
	}
	if !lockStatus.Held {
		status.DaemonRunning = false
		status.DaemonError = fmt.Sprintf("%s host is running without an active child process lock", role)
		return status
	}
	parentPID, err := service.ProcessParentPID(lockStatus.PID)
	if err != nil {
		status.DaemonRunning = false
		status.DaemonError = fmt.Sprintf("inspect %s child process %d: %v", role, lockStatus.PID, err)
		return status
	}
	if parentPID != status.DaemonPID {
		status.DaemonRunning = false
		status.DaemonError = fmt.Sprintf("%s process lock owner %d belongs to host %d, not launchd host %d", role, lockStatus.PID, parentPID, status.DaemonPID)
	}
	return status
}

func waitLaunchdNativeFSKitHealthy(ctx context.Context, manager service.Manager, label string, definitionPath string, mountPoint string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	lockPaths, err := nativeFSKitLaunchdLockPaths(definitionPath)
	if err != nil {
		return err
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var daemon, supervisor service.Status
	supervisorLabel := nativeFSKitSupervisorLabel(label)
	for {
		daemon = validateLaunchdChildProcess(manager.Status(ctx, label, mountPoint), lockPaths.daemon, "daemon")
		supervisor = validateLaunchdChildProcess(manager.Status(ctx, supervisorLabel, mountPoint), lockPaths.supervisor, "supervisor")
		if daemon.DaemonRunning && supervisor.DaemonRunning && daemon.MountHealthy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("native FSKit service did not become healthy: daemon=%t supervisor=%t mount=%t daemon_error=%q supervisor_error=%q mount_error=%q", daemon.DaemonRunning, supervisor.DaemonRunning, daemon.MountHealthy, daemon.DaemonError, supervisor.DaemonError, daemon.MountError)
		case <-ticker.C:
		}
	}
}

func systemdServiceUnit(definitionPath string, label string) (string, error) {
	unit, err := service.SystemdUnitName(label)
	if err != nil {
		return "", err
	}
	if filepath.Base(definitionPath) != unit {
		return "", fmt.Errorf("systemd definition filename must be %s", unit)
	}
	return unit, nil
}

func newFSServiceUpdatePreflightCommand() *cobra.Command {
	var codexHome, storeDir string
	var compatibility compatibilityFlags
	var automatic, promote, applyQuarantine, jsonOutput bool
	command := &cobra.Command{
		Use:   "update-preflight",
		Short: "Gate service updates and optionally route unknown-version sessions to current native bytes",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			home, err := codex.ResolveHome(codexHome)
			if err != nil {
				return err
			}
			store := resolveFoldStore(home, storeDir)
			doctorErr := requireStorageHealth(command.Context(), store)
			compatibilityResult, err := evaluateCompatibility(command.Context(), store, compatibility)
			if err != nil {
				return err
			}
			fallbackReady, err := managedRoutesMatchCurrentBytes(command.Context(), home, store)
			if err != nil {
				fallbackReady = false
			}
			decision := service.EvaluateUpdate(service.UpdateInput{Capability: verifiedCapability(), DoctorHealthy: doctorErr == nil, Compatibility: compatibilityResult.Evaluation, NativeFallbackReady: fallbackReady, Automatic: automatic, ExplicitPromotion: promote})
			result := FSUpdatePreflightResult{DoctorHealthy: doctorErr == nil, Compatibility: compatibilityResult, Decision: decision}
			if decision.Quarantine && decision.RequiresNativeFallback && applyQuarantine {
				count, err := quarantineManagedRoutes(command.Context(), home, store)
				if err != nil {
					return err
				}
				result.QuarantinedSessions = count
				result.Decision = service.EvaluateUpdate(service.UpdateInput{Capability: verifiedCapability(), DoctorHealthy: doctorErr == nil, Compatibility: compatibilityResult.Evaluation, NativeFallbackReady: true, Automatic: automatic, ExplicitPromotion: promote})
			}
			if jsonOutput {
				return writeJSON(command, result)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "allowed=%t quarantine=%t requires_fallback=%t doctor=%t quarantined=%d reason=%q\n", result.Decision.Allowed, result.Decision.Quarantine, result.Decision.RequiresNativeFallback, result.DoctorHealthy, result.QuarantinedSessions, result.Decision.Reason)
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	addCompatibilityFlags(command, &compatibility)
	command.Flags().BoolVar(&automatic, "automatic", false, "Evaluate an unattended update")
	command.Flags().BoolVar(&promote, "promote", false, "Explicitly approve preview or canary promotion")
	command.Flags().BoolVar(&applyQuarantine, "apply-quarantine", false, "Route managed sessions to verified current native bytes when clients are unknown")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func managedRoutesMatchCurrentBytes(ctx context.Context, home string, store string) (bool, error) {
	states, err := vfs.DiscoverSessionStates(store)
	if err != nil {
		return false, err
	}
	sessions, err := codex.LoadSessions(home)
	if err != nil {
		return false, err
	}
	byID := make(map[string]codex.Session, len(sessions))
	for _, session := range sessions {
		byID[session.ID] = session
	}
	for _, state := range states {
		current, ok := byID[state.SessionID]
		if !ok {
			return false, fmt.Errorf("Codex route missing for managed session %s", state.SessionID)
		}
		if !isGeneratedNativeFallbackPath(current.RolloutPath, store, state.SessionID) {
			return false, nil
		}
		if _, err := hashPath(current.RolloutPath); err != nil {
			return false, err
		}
	}
	return true, nil
}

func quarantineManagedRoutes(ctx context.Context, home string, store string) (int, error) {
	states, err := vfs.DiscoverSessionStates(store)
	if err != nil {
		return 0, err
	}
	sessions, err := codex.LoadSessions(home)
	if err != nil {
		return 0, err
	}
	byID := make(map[string]codex.Session, len(sessions))
	for _, session := range sessions {
		byID[session.ID] = session
	}
	count := 0
	for _, state := range states {
		current, ok := byID[state.SessionID]
		if !ok {
			return count, fmt.Errorf("Codex route missing for managed session %s", state.SessionID)
		}
		if isGeneratedNativeFallbackPath(current.RolloutPath, store, state.SessionID) {
			if _, err := hashPath(current.RolloutPath); err != nil {
				return count, err
			}
			continue
		}
		managed, resolver, err := openManagedSession(ctx, store, state)
		if err != nil {
			return count, err
		}
		targetDirectory := filepath.Join(store, "fs", "fallbacks", state.SessionID)
		if err := os.MkdirAll(targetDirectory, 0o700); err != nil {
			return count, err
		}
		if err := os.Chmod(targetDirectory, 0o700); err != nil {
			return count, err
		}
		targetPath := filepath.Join(targetDirectory, "quarantine-current.jsonl")
		target, err := managed.MaterializeCurrent(ctx, targetPath, true)
		_ = resolver.Close()
		if err != nil {
			return count, err
		}
		if filepath.Clean(current.RolloutPath) == filepath.Clean(target.Path) {
			continue
		}
		if _, err := codex.RouteSession(ctx, codex.RouteOptions{CodexHome: home, SessionID: state.SessionID, ExpectedPath: current.RolloutPath, Target: codex.RouteTarget{Path: target.Path, Bytes: target.Bytes, SHA256: target.SHA256}}); err != nil {
			return count, err
		}
		if _, err := retireManagedState(store, state.SessionID); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func isGeneratedNativeFallbackPath(path string, store string, sessionID string) bool {
	if path == "" || store == "" || sessionID == "" {
		return false
	}
	directory := filepath.Clean(filepath.Dir(path))
	legacyDirectory := filepath.Join(filepath.Clean(store), "fs", "sessions", sessionID)
	fallbackDirectory := filepath.Join(filepath.Clean(store), "fs", "fallbacks", sessionID)
	if directory != legacyDirectory && directory != fallbackDirectory {
		return false
	}
	switch filepath.Base(path) {
	case "fallback-current.jsonl", "quarantine-current.jsonl":
		return true
	default:
		return false
	}
}

func retireManagedState(store string, sessionID string) (string, error) {
	if store == "" || sessionID == "" {
		return "", errors.New("store and session ID are required")
	}
	source := filepath.Join(filepath.Clean(store), "fs", "sessions", sessionID)
	retiredRoot := filepath.Join(filepath.Clean(store), "fs", "retired")
	if err := os.MkdirAll(retiredRoot, 0o700); err != nil {
		return "", err
	}
	target := filepath.Join(retiredRoot, fmt.Sprintf("%s-%d", sessionID, time.Now().UnixNano()))
	if err := os.Rename(source, target); err != nil {
		return "", err
	}
	return target, nil
}

func restoreManagedState(store string, sessionID string, retiredPath string) error {
	if store == "" || sessionID == "" || retiredPath == "" {
		return errors.New("store, session ID, and retired state path are required")
	}
	target := filepath.Join(filepath.Clean(store), "fs", "sessions", sessionID)
	if err := os.Rename(filepath.Clean(retiredPath), target); err != nil {
		return err
	}
	if _, err := vfs.RepublishSessionState(filepath.Join(target, "state.json")); err != nil {
		return fmt.Errorf("republish restored managed state: %w", err)
	}
	return nil
}

func retainCanonicalSnapshot(ctx context.Context, store string, sessionID string, source vfs.NativeFile, budget storage.Checker) (vfs.NativeFile, error) {
	if store == "" || !validSessionID(sessionID) || source.Path == "" {
		return vfs.NativeFile{}, errors.New("store, session ID, and source snapshot are required")
	}
	sourcePath := filepath.Clean(source.Path)
	verified, err := hashPath(sourcePath)
	if err != nil {
		return vfs.NativeFile{}, fmt.Errorf("verify canonical native snapshot: %w", err)
	}
	if verified.Bytes != source.Bytes || verified.SHA256 != source.SHA256 {
		return vfs.NativeFile{}, errors.New("canonical native snapshot changed during migration")
	}
	if budget == nil {
		guard, err := storage.DefaultGuard(store)
		if err != nil {
			return vfs.NativeFile{}, err
		}
		budget = guard
	}
	if _, err := budget.Check(ctx, storage.Projection{Operation: "retain-migration-snapshot", AdditionalPersistentBytes: source.Bytes}); err != nil {
		return vfs.NativeFile{}, err
	}
	retainedDir := filepath.Join(filepath.Clean(store), "fs", "snapshots", sessionID)
	retainedPath := filepath.Join(retainedDir, "native.jsonl")
	if err := os.MkdirAll(retainedDir, 0o700); err != nil {
		return vfs.NativeFile{}, err
	}
	if _, err := os.Lstat(retainedPath); err == nil {
		return vfs.NativeFile{}, errors.New("retained canonical snapshot already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return vfs.NativeFile{}, err
	}
	if err := os.Link(sourcePath, retainedPath); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return vfs.NativeFile{}, fmt.Errorf("stage canonical native snapshot: %w", err)
		}
		if err := copyCanonicalSnapshot(sourcePath, retainedPath); err != nil {
			return vfs.NativeFile{}, fmt.Errorf("copy canonical native snapshot: %w", err)
		}
	}
	retained, err := hashPath(retainedPath)
	if err == nil && (retained.Bytes != source.Bytes || retained.SHA256 != source.SHA256) {
		err = errors.New("retained canonical snapshot does not match source")
	}
	if err != nil {
		_ = os.Remove(retainedPath)
		return vfs.NativeFile{}, err
	}
	retained.Path = retainedPath
	return retained, nil
}

func finalizeCanonicalSnapshotSource(sourcePath string, retained vfs.NativeFile) error {
	sourcePath = filepath.Clean(sourcePath)
	retained.Path = filepath.Clean(retained.Path)
	source, err := hashPath(sourcePath)
	if err != nil {
		return fmt.Errorf("verify canonical source before cutover: %w", err)
	}
	hidden, err := hashPath(retained.Path)
	if err != nil {
		return fmt.Errorf("verify retained canonical snapshot before cutover: %w", err)
	}
	if source.Bytes != retained.Bytes || source.SHA256 != retained.SHA256 || hidden.Bytes != retained.Bytes || hidden.SHA256 != retained.SHA256 {
		return errors.New("canonical source changed before cutover")
	}
	if err := os.Remove(sourcePath); err != nil {
		return fmt.Errorf("hide canonical source after mount acknowledgement: %w", err)
	}
	return nil
}

func copyCanonicalSnapshot(sourcePath string, retainedPath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(retainedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		_ = os.Remove(retainedPath)
		return err
	}
	if err := target.Sync(); err != nil {
		_ = target.Close()
		_ = os.Remove(retainedPath)
		return err
	}
	return target.Close()
}

func restoreCanonicalSnapshotSource(originalPath string, retainedPath string) error {
	originalPath = filepath.Clean(originalPath)
	retainedPath = filepath.Clean(retainedPath)
	if originalPath == "" || retainedPath == "" {
		return errors.New("original and retained snapshot paths are required")
	}
	if _, err := os.Lstat(originalPath); err == nil {
		original, originalErr := hashPath(originalPath)
		retained, retainedErr := hashPath(retainedPath)
		if originalErr != nil || retainedErr != nil || original.Bytes != retained.Bytes || original.SHA256 != retained.SHA256 {
			return errors.New("cannot discard retained snapshot while canonical source differs")
		}
		if err := os.Remove(retainedPath); err != nil {
			return err
		}
		_ = os.Remove(filepath.Dir(retainedPath))
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(originalPath), 0o700); err != nil {
		return err
	}
	if err := os.Rename(retainedPath, originalPath); err != nil {
		return fmt.Errorf("restore canonical native snapshot: %w", err)
	}
	return nil
}

type mountAcknowledgement struct {
	Generation uint64 `json:"generation"`
	Route      string `json:"route"`
}

const (
	retirementRequestFilename         = "retire.request.json"
	retirementAcknowledgementFilename = "retire.ack.json"
)

type retirementControl struct {
	Token      string `json:"token"`
	Generation uint64 `json:"generation"`
	Route      string `json:"route"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	Error      string `json:"error,omitempty"`
}

func createRetirementRequest(store string, sessionID string, generation uint64, route string, target vfs.NativeFile) (retirementControl, error) {
	if store == "" || !validSessionID(sessionID) || generation == 0 || route == "" || target.Bytes < 0 || len(target.SHA256) != 64 {
		return retirementControl{}, errors.New("complete retirement request metadata is required")
	}
	directory := filepath.Join(filepath.Clean(store), "fs", "sessions", sessionID)
	if pending, exists, err := readRetirementRequest(store, sessionID); err != nil {
		return retirementControl{}, err
	} else if exists {
		if pending.Generation != generation || pending.Route != route || pending.Bytes != target.Bytes || pending.SHA256 != target.SHA256 {
			return retirementControl{}, errors.New("pending session retirement does not match the requested generation and target")
		}
		return pending, nil
	}
	if err := removeIfExists(filepath.Join(directory, retirementAcknowledgementFilename)); err != nil {
		return retirementControl{}, err
	}
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return retirementControl{}, err
	}
	request := retirementControl{Token: hex.EncodeToString(tokenBytes), Generation: generation, Route: route, Bytes: target.Bytes, SHA256: target.SHA256}
	if err := writeSessionControlFile(directory, retirementRequestFilename, request); err != nil {
		return retirementControl{}, err
	}
	return request, nil
}

func readRetirementRequest(store string, sessionID string) (retirementControl, bool, error) {
	path := filepath.Join(filepath.Clean(store), "fs", "sessions", sessionID, retirementRequestFilename)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return retirementControl{}, false, nil
	}
	if err != nil {
		return retirementControl{}, false, err
	}
	var request retirementControl
	if err := json.Unmarshal(data, &request); err != nil {
		return retirementControl{}, false, fmt.Errorf("decode retirement request: %w", err)
	}
	if len(request.Token) != 32 || request.Generation == 0 || request.Route == "" || request.Bytes < 0 || len(request.SHA256) != 64 || request.Error != "" {
		return retirementControl{}, false, errors.New("invalid retirement request")
	}
	return request, true, nil
}

func writeRetirementAcknowledgement(store string, sessionID string, acknowledgement retirementControl) error {
	if len(acknowledgement.Token) != 32 || acknowledgement.Generation == 0 || acknowledgement.Route == "" || acknowledgement.Bytes < 0 || len(acknowledgement.SHA256) != 64 {
		return errors.New("complete retirement acknowledgement metadata is required")
	}
	directory := filepath.Join(filepath.Clean(store), "fs", "sessions", sessionID)
	return writeSessionControlFile(directory, retirementAcknowledgementFilename, acknowledgement)
}

func waitForRetirementAcknowledgement(ctx context.Context, store string, sessionID string, request retirementControl, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	path := filepath.Join(filepath.Clean(store), "fs", "sessions", sessionID, retirementAcknowledgementFilename)
	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			var acknowledgement retirementControl
			if json.Unmarshal(data, &acknowledgement) == nil &&
				acknowledgement.Token == request.Token && acknowledgement.Generation == request.Generation &&
				acknowledgement.Route == request.Route && acknowledgement.Bytes == request.Bytes && acknowledgement.SHA256 == request.SHA256 {
				if acknowledgement.Error != "" {
					return fmt.Errorf("retirement rejected: %s", acknowledgement.Error)
				}
				return nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for retirement acknowledgement")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func clearRetirementControl(directory string) error {
	var result error
	for _, name := range []string{retirementRequestFilename, retirementAcknowledgementFilename} {
		if err := removeIfExists(filepath.Join(filepath.Clean(directory), name)); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func removeIfExists(path string) error {
	if err := os.Remove(filepath.Clean(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func writeMountAcknowledgement(store string, sessionID string, generation uint64, route string) error {
	if store == "" || !validSessionID(sessionID) || generation == 0 || route == "" {
		return errors.New("complete mount acknowledgement metadata is required")
	}
	directory := filepath.Join(filepath.Clean(store), "fs", "sessions", sessionID)
	return writeSessionControlFile(directory, "mounted.json", mountAcknowledgement{Generation: generation, Route: route})
}

func writeSessionControlFile(directory string, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".mounted-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, filepath.Join(directory, name))
}

func waitForMountAcknowledgement(ctx context.Context, store string, sessionID string, generation uint64, route string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	path := filepath.Join(filepath.Clean(store), "fs", "sessions", sessionID, "mounted.json")
	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			var acknowledgement mountAcknowledgement
			if json.Unmarshal(data, &acknowledgement) == nil && acknowledgement.Generation == generation && acknowledgement.Route == route {
				return nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for the filesystem daemon")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func retireCanonicalNativeSnapshot(store string, nativeRoot string, sessionID string, snapshotPath string, currentPath string, retiredState string) (string, error) {
	snapshotPath = filepath.Clean(snapshotPath)
	if snapshotPath == filepath.Clean(currentPath) {
		return "", nil
	}
	var relative string
	legacyRelative, legacyErr := relativeWithin(filepath.Clean(nativeRoot), snapshotPath)
	hiddenRoot := filepath.Join(filepath.Clean(store), "fs", "snapshots", sessionID)
	_, hiddenErr := relativeWithin(hiddenRoot, snapshotPath)
	switch {
	case legacyErr == nil:
		relative = legacyRelative
	case hiddenErr == nil && filepath.Base(snapshotPath) == "native.jsonl":
		relative = filepath.Join("store-snapshot", "native.jsonl")
	default:
		return "", errors.New("canonical native snapshot is outside the retained snapshot roots")
	}
	target := filepath.Join(filepath.Clean(retiredState), "retained-native", relative)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(snapshotPath, target); err != nil {
		return "", err
	}
	oldSidecar := filepath.Join(filepath.Dir(snapshotPath), "._"+filepath.Base(snapshotPath))
	newSidecar := filepath.Join(filepath.Dir(target), "._"+filepath.Base(target))
	if _, err := os.Lstat(oldSidecar); err == nil {
		if err := os.Rename(oldSidecar, newSidecar); err != nil {
			_ = os.Rename(target, snapshotPath)
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.Rename(target, snapshotPath)
		return "", err
	}
	if hiddenErr == nil {
		_ = os.Remove(hiddenRoot)
	}
	return target, nil
}

func validSessionID(sessionID string) bool {
	return sessionID != "" && sessionID != "." && sessionID != ".." && !strings.ContainsAny(sessionID, "/\\\x00")
}

func relativeWithin(root string, target string) (string, error) {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("path is outside root")
	}
	return relative, nil
}

func restoreCanonicalNativeSnapshot(snapshotPath string, retiredSnapshot string) error {
	if retiredSnapshot == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o700); err != nil {
		return err
	}
	if err := os.Rename(retiredSnapshot, snapshotPath); err != nil {
		return err
	}
	retiredSidecar := filepath.Join(filepath.Dir(retiredSnapshot), "._"+filepath.Base(retiredSnapshot))
	originalSidecar := filepath.Join(filepath.Dir(snapshotPath), "._"+filepath.Base(snapshotPath))
	if _, err := os.Lstat(retiredSidecar); err == nil {
		return os.Rename(retiredSidecar, originalSidecar)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func canonicalMountRoute(home string, mount string, route string) (string, error) {
	relative, err := canonicalRelativeRoute(home, route)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Clean(mount), relative), nil
}

func canonicalNativeRoute(home string, nativeRoot string, route string) (string, error) {
	relative, err := canonicalRelativeRoute(home, route)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(nativeRoot) {
		return "", errors.New("canonical native root must be absolute")
	}
	return filepath.Join(filepath.Clean(nativeRoot), relative), nil
}

func canonicalNamespaceRoute(home string, mount string, route string) (string, error) {
	relative, homeErr := canonicalRelativeRoute(home, route)
	if homeErr != nil {
		var mountErr error
		relative, mountErr = canonicalRelativeRoute(mount, route)
		if mountErr != nil {
			return "", homeErr
		}
	}
	return "/" + filepath.ToSlash(relative), nil
}

func canonicalSessionRoutes(home string, mount string, store string, states []vfs.SessionState, sessions []codex.Session) (map[string]string, error) {
	managed := make(map[string]struct{}, len(states))
	for _, state := range states {
		managed[state.SessionID] = struct{}{}
	}
	routes := make(map[string]string, len(states))
	for _, session := range sessions {
		if _, exists := managed[session.ID]; !exists {
			continue
		}
		if isGeneratedNativeFallbackPath(session.RolloutPath, store, session.ID) {
			continue
		}
		route, err := canonicalNamespaceRoute(home, mount, session.RolloutPath)
		if err != nil {
			return nil, err
		}
		routes[session.ID] = route
	}
	return routes, nil
}

func discoverCanonicalRoutes(home string, mount string, store string, states []vfs.SessionState, load func(string) ([]codex.Session, error)) (map[string]string, error) {
	if len(states) == 0 {
		return map[string]string{}, nil
	}
	sessions, err := load(home)
	if err != nil {
		return nil, err
	}
	return canonicalSessionRoutes(home, mount, store, states, sessions)
}

func canonicalRelativeRoute(home string, route string) (string, error) {
	if !filepath.IsAbs(home) || !filepath.IsAbs(route) {
		return "", errors.New("canonical Codex and route paths must be absolute")
	}
	home = filepath.Clean(home)
	relative, err := filepath.Rel(home, filepath.Clean(route))
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("Codex route is outside its home directory")
	}
	first, remainder := relative, ""
	if separator := strings.IndexByte(relative, byte(filepath.Separator)); separator >= 0 {
		first, remainder = relative[:separator], relative[separator+1:]
	}
	if (first != "sessions" && first != "archived_sessions") || remainder == "" || !strings.HasSuffix(remainder, ".jsonl") {
		return "", errors.New("Codex route is not inside sessions or archived_sessions")
	}
	return relative, nil
}

func addServicePathFlags(command *cobra.Command, codexHome, storeDir, mountPoint, binaryPath, plistPath, logDir *string) {
	command.Flags().StringVar(codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().StringVar(mountPoint, "mount", "", "Mounted CodexFold filesystem path; defaults to <codex-home>/fold-fs")
	command.Flags().StringVar(binaryPath, "binary", "", "Absolute CodexFold binary path; defaults to the current executable")
	addServiceDefinitionFlags(command, plistPath)
	command.Flags().StringVar(logDir, "log-dir", "", "Service log directory; defaults to <store>/service/logs")
}

func addServiceDefinitionFlags(command *cobra.Command, definitionPath *string) {
	command.Flags().StringVar(definitionPath, "definition", "", "Native service definition path")
	command.Flags().StringVar(definitionPath, "plist", "", "LaunchAgent plist path (macOS compatibility alias)")
}

func resolveServicePaths(codexHome, storeDir, mountPoint, binaryPath, plistPath, logDir, label string) (string, string, string, string, string, string, error) {
	home, err := codex.ResolveHome(codexHome)
	if err != nil {
		return "", "", "", "", "", "", err
	}
	store := resolveFoldStore(home, storeDir)
	mount := defaultMountPoint(home, mountPoint)
	binary := binaryPath
	if binary == "" {
		binary, err = os.Executable()
		if err != nil {
			return "", "", "", "", "", "", err
		}
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return "", "", "", "", "", "", err
	}
	plist, err := resolveServiceDefinitionPathForLabel(plistPath, label)
	if err != nil {
		return "", "", "", "", "", "", err
	}
	logs := logDir
	if logs == "" {
		logs = filepath.Join(store, "service", "logs")
	}
	logs, err = filepath.Abs(logs)
	if err != nil {
		return "", "", "", "", "", "", err
	}
	return home, store, mount, binary, plist, logs, nil
}

func resolvePlistPath(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"), nil
}

func nativeFSKitSupervisorDefinitionPath(definitionPath string) string {
	definitionPath = filepath.Clean(definitionPath)
	extension := filepath.Ext(definitionPath)
	base := strings.TrimSuffix(filepath.Base(definitionPath), extension)
	if extension == "" {
		extension = ".plist"
	}
	return filepath.Join(filepath.Dir(definitionPath), base+".supervisor"+extension)
}

func nativeFSKitSupervisorLabel(label string) string {
	return label + ".supervisor"
}

func resolveServiceDefinitionPath(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	platform, err := service.CurrentPlatform()
	if err != nil {
		return "", err
	}
	switch platform {
	case service.PlatformLaunchd:
		return resolvePlistPath("")
	case service.PlatformSystemd:
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if !filepath.IsAbs(configHome) {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			configHome = filepath.Join(home, ".config")
		}
		unit, err := service.SystemdUnitName(serviceLabel)
		if err != nil {
			return "", err
		}
		return filepath.Join(configHome, "systemd", "user", unit), nil
	case service.PlatformWindows:
		programData := os.Getenv("ProgramData")
		if programData == "" {
			return "", errors.New("ProgramData is not set")
		}
		return filepath.Join(programData, "CodexFold", "service.json"), nil
	default:
		return "", errors.New("unknown service platform")
	}
}

func resolveServiceDefinitionPathForLabel(explicit, label string) (string, error) {
	if explicit != "" || label == "" || label == serviceLabel {
		return resolveServiceDefinitionPath(explicit)
	}
	for index, character := range label {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '.' || character == '-' {
			if index == 0 && character == '.' {
				return "", errors.New("custom service label is not safe for a LaunchAgent path")
			}
			continue
		}
		return "", errors.New("custom service label is not safe for a LaunchAgent path")
	}
	if strings.HasSuffix(label, ".") {
		return "", errors.New("custom service label is not safe for a LaunchAgent path")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

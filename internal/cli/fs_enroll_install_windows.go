//go:build windows

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/enroll"
	"github.com/samekind/codexfold/internal/service"
	"github.com/samekind/codexfold/internal/sessionns"
	"github.com/spf13/cobra"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

type enrollmentInstallResult struct {
	Applied       bool                            `json:"applied"`
	Binding       service.WindowsEnrollmentConfig `json:"binding"`
	FilesystemPID uint32                          `json:"filesystem_pid"`
	WorkerPID     uint32                          `json:"worker_pid"`
	Policy        enroll.Control                  `json:"policy"`
	Error         string                          `json:"error,omitempty"`
}

func newEnrollmentServiceInstallCommand() *cobra.Command {
	var home, mount, resultPath string
	var apply bool
	command := &cobra.Command{Use: "install", Short: "Install or update only the persistent Windows enrollment service", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			result, err := installEnrollmentService(command.Context(), home, mount, apply)
			if err != nil {
				result.Error = err.Error()
			}
			if resultPath != "" {
				if !filepath.IsAbs(resultPath) {
					return errors.New("absolute installation result path is required")
				}
				data, marshalErr := json.MarshalIndent(result, "", "  ")
				if marshalErr != nil {
					return marshalErr
				}
				if _, writeErr := service.WriteDefinition(resultPath, append(data, '\n'), true); writeErr != nil {
					return errors.Join(err, writeErr)
				}
				// GUI-subsystem/elevated processes may have no usable stdout. The
				// result file is the output channel in that invocation.
				return err
			}
			if err != nil {
				return err
			}
			return writeJSON(command, result)
		}}
	command.Flags().StringVar(&home, "codex-home", "", "Explicit absolute user Codex home")
	command.Flags().StringVar(&mount, "mount", "V:/", "Mounted canonical namespace drive")
	command.Flags().StringVar(&resultPath, "result", "", "Write an installation result for an elevated invocation")
	command.Flags().BoolVar(&apply, "apply", false, "Install the automatic Windows enrollment service without remounting")
	return command
}

// Use native tools by absolute system path, including when the installer was
// elevated from a user shell whose PATH could resolve a different executable.
type enrollmentSystemRunner struct{}

func (enrollmentSystemRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, filepath.Join(system, name), args...)
	configureEnrollmentChild(command)
	return command.CombinedOutput()
}

func installEnrollmentService(ctx context.Context, home, mount string, apply bool) (result enrollmentInstallResult, runErr error) {
	if !filepath.IsAbs(home) || !filepath.IsAbs(mount) {
		return result, errors.New("absolute home and mount paths are required")
	}
	home, mount = filepath.Clean(home), filepath.Clean(mount)
	programRoot := filepath.Join(os.Getenv("ProgramFiles"), "CodexFold", "Enrollment")
	dataRoot := filepath.Join(os.Getenv("ProgramData"), "CodexFold", "Enrollment")
	if !filepath.IsAbs(programRoot) || !filepath.IsAbs(dataRoot) {
		return result, errors.New("Windows installation roots are not absolute")
	}
	owner, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return result, err
	}
	config := service.WindowsEnrollmentConfig{Version: 1, ServiceName: service.WindowsEnrollmentServiceName, OwnerSID: owner.User.Sid.String(),
		BinaryPath: filepath.Join(programRoot, "codexfold-enroll.exe"), CodexHome: home, Store: filepath.Join(home, "fold-store"), NativeRoot: filepath.Join(home, "fold-native"), Mount: mount,
		StdoutPath: filepath.Join(dataRoot, "stdout.log"), StderrPath: filepath.Join(dataRoot, "stderr.log")}
	definition := filepath.Join(dataRoot, "service.json")
	result.Binding = config
	policy, err := enroll.LoadControl(enroll.WorkerControlPath(config.Store))
	if err != nil {
		return result, err
	}
	if !policy.Present {
		policy = enroll.Control{Present: true, Enabled: true, Interval: time.Minute, StableFor: time.Hour, BatchSize: 5}
	}
	result.Policy = policy
	if !apply {
		return result, nil
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		return result, errors.New("administrator elevation is required to install the enrollment service")
	}
	if err := service.ProbeMount(mount); err != nil {
		return result, err
	}
	namespace, err := sessionns.Inspect(sessionns.Options{Home: home, Mount: mount, NativeRoot: config.NativeRoot})
	if err != nil || !namespace.Active {
		return result, errors.Join(errors.New("the bound canonical namespace must be active"), err)
	}
	manager, err := mgr.Connect()
	if err != nil {
		return result, err
	}
	defer manager.Disconnect()
	fsService, err := manager.OpenService(serviceLabel)
	if err != nil {
		return result, err
	}
	defer fsService.Close()
	fsStatus, err := fsService.Query()
	if err != nil || fsStatus.State != svc.Running {
		return result, errors.New("the filesystem service must remain running")
	}
	fsConfig, err := fsService.Config()
	if err != nil {
		return result, err
	}
	fsDefinition := filepath.Join(os.Getenv("ProgramData"), "CodexFold", "service.json")
	fsBytes, err := os.ReadFile(fsDefinition)
	if err != nil {
		return result, err
	}
	boundFS, err := service.ParseWindowsConfig(fsBytes)
	if err != nil || boundFS.ServiceName != serviceLabel || !strings.Contains(fsConfig.BinaryPathName, fsDefinition) || !serviceArgumentEquals(boundFS.Arguments, "--codex-home", home) || !serviceArgumentEquals(boundFS.Arguments, "--store", config.Store) {
		return result, errors.New("the running filesystem service belongs to another installation")
	}
	result.FilesystemPID = fsStatus.ProcessId
	runner := enrollmentSystemRunner{}
	for _, directory := range []string{programRoot, dataRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return result, err
		}
		if info, err := os.Lstat(directory); err != nil || info.Mode()&os.ModeSymlink != 0 {
			return result, errors.New("enrollment installation directory must not be a link")
		}
		if output, err := runner.Run(ctx, "icacls.exe", directory, "/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F", "*S-1-5-32-545:(OI)(CI)RX"); err != nil {
			return result, fmt.Errorf("protect enrollment installation: %w: %s", err, output)
		}
	}
	installLock, err := service.AcquireProcessLock(filepath.Join(dataRoot, "install.lock"))
	if err != nil {
		return result, err
	}
	defer installLock.Close()
	// Re-read after taking the installer lock, before pausing the worker.
	if current, err := enroll.LoadControl(enroll.WorkerControlPath(config.Store)); err != nil {
		return result, err
	} else if current.Present {
		policy = current
		result.Policy = current
	}
	var previousDefinition []byte
	previousServiceRunning := false
	// Stop only an existing, explicitly owned worker service. A raw CLI worker
	// is stopped via its hot policy so no Codex or filesystem process is touched.
	if existing, err := manager.OpenService(config.ServiceName); err == nil {
		defer existing.Close()
		previous, err := existing.Config()
		if err != nil || !strings.Contains(previous.BinaryPathName, config.BinaryPath) || !strings.Contains(previous.BinaryPathName, definition) {
			return result, errors.New("an unrelated service uses the enrollment service name")
		}
		oldBinding, err := os.ReadFile(definition)
		if err != nil {
			return result, err
		}
		binding, err := service.ParseWindowsEnrollmentConfig(oldBinding)
		if err != nil || !strings.EqualFold(binding.CodexHome, home) || binding.OwnerSID != config.OwnerSID {
			return result, errors.New("the enrollment service is bound to another user or home")
		}
		previousStatus, err := existing.Query()
		if err != nil {
			return result, err
		}
		if err := stopEnrollmentSCM(ctx, existing); err != nil {
			return result, err
		}
		previousDefinition = oldBinding
		// Reinstalling restores this owned service if the replacement fails.
		previousServiceRunning = previousStatus.State == svc.Running
	}
	paused := policy
	paused.Enabled = false
	if err := enroll.SaveControl(enroll.WorkerControlPath(config.Store), paused); err != nil {
		return result, err
	}
	// Always restore the captured user intent, including when a later install
	// step fails. Restoring a paused setting must never silently enable folding.
	defer func() {
		if !result.Applied {
			runErr = errors.Join(runErr, enroll.SaveControl(enroll.WorkerControlPath(config.Store), policy))
		}
	}()
	if err := waitEnrollmentLockReleased(ctx, config.Store); err != nil {
		return result, err
	}
	builtin, err := enroll.LoadControl(enroll.ControlPath(config.Store))
	if err != nil {
		return result, err
	}
	if !builtin.Present {
		builtin = policy
	}
	builtin.Enabled = false
	if err := enroll.SaveControl(enroll.ControlPath(config.Store), builtin); err != nil {
		return result, err
	}
	candidate, err := os.Executable()
	if err != nil {
		return result, err
	}
	if strings.EqualFold(candidate, config.BinaryPath) {
		return result, errors.New("install from a separate candidate binary")
	}
	update, err := service.StageBinaryUpdate(candidate, config.BinaryPath)
	if err != nil {
		return result, err
	}
	if err := update.Promote(); err != nil {
		_ = update.Rollback()
		_ = update.Commit()
		return result, err
	}
	defer func() {
		if runErr != nil && !result.Applied {
			// Keep a failed replacement from leaving an enabled but broken service.
			if installed, err := manager.OpenService(config.ServiceName); err == nil {
				stopErr := stopEnrollmentSCM(context.Background(), installed)
				if stopErr != nil {
					installed.Close()
					runErr = errors.Join(runErr, stopErr)
					return
				}
				if len(previousDefinition) == 0 {
					runErr = errors.Join(runErr, installed.Delete())
				}
				installed.Close()
			}
			rollbackErr := update.Rollback()
			runErr = errors.Join(runErr, rollbackErr)
			if rollbackErr != nil {
				return
			}
			if len(previousDefinition) > 0 {
				_, restoreErr := service.WriteDefinition(definition, previousDefinition, true)
				runErr = errors.Join(runErr, restoreErr, enroll.SaveControl(enroll.WorkerControlPath(config.Store), policy))
				if restoreErr == nil && previousServiceRunning {
					runErr = errors.Join(runErr, (service.WindowsManager{Runner: runner}).Start(context.Background(), config.ServiceName))
				}
			}
		}
		runErr = errors.Join(runErr, update.Commit())
	}()
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return result, err
	}
	if _, err := service.WriteDefinition(definition, append(data, '\n'), true); err != nil {
		return result, err
	}
	if _, err := service.ParseWindowsEnrollmentConfig(data); err != nil {
		return result, err
	}
	platform := service.WindowsManager{Runner: runner}
	if err := platform.InstallEnrollment(ctx, config.ServiceName, config.BinaryPath, definition); err != nil {
		return result, err
	}
	if err := enroll.SaveControl(enroll.WorkerControlPath(config.Store), policy); err != nil {
		return result, err
	}
	startAttempt := time.Now()
	if err := platform.Start(ctx, config.ServiceName); err != nil {
		return result, err
	}
	workerService, err := manager.OpenService(config.ServiceName)
	if err != nil {
		return result, err
	}
	defer workerService.Close()
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, err := workerService.Query()
		if err != nil {
			return result, err
		}
		progress, _ := enroll.LoadProgress(enroll.WorkerProgressPath(config.Store))
		if status.State == svc.Running && progress.UpdatedAt.After(startAttempt) && progress.Phase != enroll.PhaseStopped && progress.Phase != enroll.PhaseConfigInvalid && progress.Enabled == policy.Enabled {
			result.WorkerPID = status.ProcessId
			break
		}
		if time.Now().After(deadline) {
			return result, errors.New("enrollment service did not publish a fresh status; inspect its stderr log")
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	fsAfter, err := fsService.Query()
	if err != nil || fsAfter.State != svc.Running || fsAfter.ProcessId != result.FilesystemPID {
		return result, errors.New("filesystem service identity changed during enrollment installation")
	}
	// Correct the final filesystem recovery action without restarting it. Its
	// existing two-retry limit could otherwise strand the mount after a crash.
	if output, err := runner.Run(ctx, "sc.exe", "failure", serviceLabel, "reset=", "86400", "actions=", "restart/5000/restart/15000/restart/60000"); err != nil {
		return result, fmt.Errorf("filesystem recovery configuration: %w: %s", err, output)
	}
	result.Applied = true
	return result, nil
}

func serviceArgumentEquals(args []string, flag, value string) bool {
	for index, arg := range args {
		if arg == flag && index+1 < len(args) {
			return strings.EqualFold(filepath.Clean(args[index+1]), filepath.Clean(value))
		}
	}
	return false
}

func stopEnrollmentSCM(ctx context.Context, owned *mgr.Service) error {
	status, err := owned.Query()
	if err != nil {
		return err
	}
	if status.State == svc.Stopped {
		return nil
	}
	if status.State != svc.StopPending {
		if _, err := owned.Control(svc.Stop); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(40 * time.Second)
	for {
		status, err = owned.Query()
		if err != nil {
			return err
		}
		if status.State == svc.Stopped {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("enrollment service did not finish stopping")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func waitEnrollmentLockReleased(ctx context.Context, store string) error {
	deadline := time.Now().Add(40 * time.Second)
	for {
		status, err := service.InspectProcessLock(filepath.Join(store, "enrollment", "worker.lock"))
		if !status.Held {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("the previous enrollment worker did not release its lock")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

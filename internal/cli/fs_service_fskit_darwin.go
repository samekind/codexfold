//go:build darwin

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/service"
	"golang.org/x/sys/unix"
)

const launchServicesRegister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"

var runFSKitLaunchServicesCommand = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, launchServicesRegister, args...).CombinedOutput()
}

var runFSKitRegisteredModulesCommand = func(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(
		ctx,
		"/usr/bin/pluginkit",
		"-m", "-A", "-D", "-v", "-i", service.FSKitModuleIdentifier,
	).CombinedOutput()
}

var runFSKitResidencyCommand = func(ctx context.Context, launcher string) ([]byte, error) {
	return exec.CommandContext(ctx, launcher, "--configure-residency").CombinedOutput()
}

var runFSKitMenuBarOpenCommand = func(ctx context.Context, appPath string) ([]byte, error) {
	return exec.CommandContext(ctx, "/usr/bin/open", "-g", appPath).CombinedOutput()
}

var inspectFSKitMenuBarProcess = fsKitMenuBarProcessRunning

var (
	listFSKitUserProcessIDs       = userProcessIDs
	inspectFSKitProcessExecutable = fsKitProcessExecutablePath
	inspectFSKitProcessCommand    = fsKitProcessCommandLine
	signalFSKitProcess            = unix.Kill
)

const (
	codexFoldFSKitHostProcessName   = "CodexFoldFSKit"
	codexFoldFSKitModuleProcessName = "CodexFoldFSKitModule"
	fsKitRegistrationWait           = 15 * time.Second
	fsKitModuleShutdownWait         = 10 * time.Second
	fsKitMenuBarLaunchWait          = 5 * time.Second
)

type darwinFSKitAppTransaction struct {
	target          string
	source          string
	stageRoot       string
	stagePath       string
	appGroupPath    string
	residency       FSKitResidencyOutcome
	registration    fsKitTargetRegistrationSnapshot
	changed         bool
	hadTarget       bool
	contentsSwapped bool
	appInstalled    bool
	manageResidency bool
}

func prepareFSKitAppPlatform(ctx context.Context, source string, target string, manageResidency bool) (fsKitAppTransaction, error) {
	if !filepath.IsAbs(target) {
		return nil, errors.New("installed FSKit app path must be absolute")
	}
	target = filepath.Clean(target)
	registration, err := captureFSKitTargetRegistration(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("capture FSKit target registration: %w", err)
	}
	if source == "" {
		appGroup, residency, err := refreshFSKitApp(ctx, target, manageResidency)
		if err != nil {
			return nil, errors.Join(err, registration.restore(ctx, target))
		}
		return &darwinFSKitAppTransaction{
			target: target, appGroupPath: appGroup, residency: residency, manageResidency: manageResidency,
			registration: registration,
		}, nil
	}
	if !filepath.IsAbs(source) {
		return nil, errors.New("FSKit app source path must be absolute")
	}
	source = filepath.Clean(source)
	if source == target {
		appGroup, residency, err := refreshFSKitApp(ctx, target, manageResidency)
		if err != nil {
			return nil, errors.Join(err, registration.restore(ctx, target))
		}
		return &darwinFSKitAppTransaction{
			target: target, appGroupPath: appGroup, residency: residency, manageResidency: manageResidency,
			registration: registration,
		}, nil
	}
	if err := validateFSKitApp(ctx, source); err != nil {
		return nil, fmt.Errorf("validate FSKit app source: %w", err)
	}
	sourceDigest, err := hashAppBundle(source)
	if err != nil {
		return nil, err
	}
	targetDigest, targetErr := hashAppBundle(target)
	if targetErr == nil {
		if targetDigest == sourceDigest {
			appGroup, residency, err := refreshFSKitApp(ctx, target, manageResidency)
			if err != nil {
				return nil, errors.Join(err, registration.restore(ctx, target))
			}
			return &darwinFSKitAppTransaction{
				target: target, appGroupPath: appGroup, residency: residency, manageResidency: manageResidency,
				registration: registration,
			}, nil
		}
		if err := requireNewerFSKitAppVersion(ctx, source, target); err != nil {
			return nil, err
		}
	} else if !errors.Is(targetErr, os.ErrNotExist) {
		return nil, targetErr
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return nil, err
	}
	stageRoot, err := os.MkdirTemp(filepath.Dir(target), ".codexfold-fskit-stage-*")
	if err != nil {
		return nil, err
	}
	transaction := &darwinFSKitAppTransaction{
		target: target, source: source, stageRoot: stageRoot, changed: true,
		registration: registration, manageResidency: manageResidency,
	}
	stagePath := filepath.Join(stageRoot, filepath.Base(target))
	transaction.stagePath = stagePath
	if output, err := exec.CommandContext(ctx, "/usr/bin/ditto", source, stagePath).CombinedOutput(); err != nil {
		_ = transaction.Commit()
		return nil, commandOutputError("stage FSKit app", output, err)
	}
	if err := validateFSKitApp(ctx, stagePath); err != nil {
		_ = transaction.Commit()
		return nil, fmt.Errorf("validate staged FSKit app: %w", err)
	}
	if stagedDigest, err := hashAppBundle(stagePath); err != nil || stagedDigest != sourceDigest {
		_ = transaction.Commit()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("staged FSKit app does not match the source bundle")
	}
	if targetErr == nil {
		if err := quiesceFSKitAppForUpdate(ctx, target); err != nil {
			_, _, restoreErr := refreshFSKitApp(ctx, target, manageResidency)
			_ = transaction.Commit()
			return nil, errors.Join(fmt.Errorf("quiesce existing FSKit app: %w", err), restoreErr)
		}
	}
	if err := transaction.promoteStagedApp(); err != nil {
		rollbackErr := transaction.Rollback(ctx)
		return nil, errors.Join(err, rollbackErr)
	}
	installedDigest, err := hashAppBundle(target)
	if err != nil {
		rollbackErr := transaction.Rollback(ctx)
		return nil, errors.Join(err, rollbackErr)
	}
	if installedDigest != sourceDigest {
		rollbackErr := transaction.Rollback(ctx)
		return nil, errors.Join(errors.New("installed FSKit app does not match the source bundle"), rollbackErr)
	}
	appGroup, residency, err := refreshFSKitApp(ctx, target, manageResidency)
	if err != nil {
		rollbackErr := transaction.Rollback(ctx)
		return nil, errors.Join(err, rollbackErr)
	}
	transaction.appGroupPath = appGroup
	transaction.residency = residency
	return transaction, nil
}

func refreshFSKitApp(ctx context.Context, appPath string, manageResidency bool) (string, FSKitResidencyOutcome, error) {
	return validateAndEnableFSKitApp(ctx, appPath, manageResidency)
}

func quiesceFSKitAppForUpdate(ctx context.Context, appPath string) error {
	return stopCodexFoldFSKitModuleProcesses(ctx, appPath)
}

func unregisterFSKitAppRegistration(ctx context.Context, appPath string) error {
	output, err := runFSKitLaunchServicesCommand(ctx, "-u", appPath)
	if err != nil {
		return commandOutputError("unregister FSKit target app registration", output, err)
	}
	return nil
}

type fsKitTargetRegistrationSnapshot struct {
	modulePath string
	registered bool
}

func captureFSKitTargetRegistration(ctx context.Context, appPath string) (fsKitTargetRegistrationSnapshot, error) {
	modulePath, err := service.FSKitModulePath(appPath)
	if err != nil {
		return fsKitTargetRegistrationSnapshot{}, err
	}
	paths, err := registeredFSKitModulePaths(ctx)
	if err != nil {
		return fsKitTargetRegistrationSnapshot{}, err
	}
	return fsKitTargetRegistrationSnapshot{
		modulePath: filepath.Clean(modulePath),
		registered: containsFSKitModulePath(paths, modulePath),
	}, nil
}

func (snapshot fsKitTargetRegistrationSnapshot) restore(ctx context.Context, appPath string) error {
	modulePath, err := service.FSKitModulePath(appPath)
	if err != nil {
		return err
	}
	modulePath = filepath.Clean(modulePath)
	if snapshot.modulePath == "" || filepath.Clean(snapshot.modulePath) != modulePath {
		return errors.New("FSKit target registration snapshot does not match the app")
	}
	paths, err := registeredFSKitModulePaths(ctx)
	if err != nil {
		return err
	}
	registered := containsFSKitModulePath(paths, modulePath)
	if registered == snapshot.registered {
		return nil
	}
	if snapshot.registered {
		output, err := runFSKitLaunchServicesCommand(ctx, "-f", "-R", "-trusted", appPath)
		if err != nil {
			return commandOutputError("restore FSKit target app registration", output, err)
		}
		return waitForFSKitModulePath(ctx, modulePath, fsKitRegistrationWait)
	}
	if err := unregisterFSKitAppRegistration(ctx, appPath); err != nil {
		return err
	}
	return waitForFSKitModulePathAbsent(ctx, modulePath, fsKitRegistrationWait)
}

func containsFSKitModulePath(paths []string, target string) bool {
	target = filepath.Clean(target)
	for _, path := range paths {
		if filepath.Clean(path) == target {
			return true
		}
	}
	return false
}

func registeredFSKitModulePaths(ctx context.Context) ([]string, error) {
	output, err := runFSKitRegisteredModulesCommand(ctx)
	if err != nil {
		return nil, commandOutputError("list FSKit module registrations", output, err)
	}
	return parseFSKitModulePaths(output), nil
}

func parseFSKitModulePaths(output []byte) []string {
	seen := make(map[string]struct{})
	var paths []string
	for _, line := range strings.Split(string(output), "\n") {
		separator := strings.LastIndexByte(line, '\t')
		if separator < 0 {
			continue
		}
		candidate := filepath.Clean(strings.TrimSpace(line[separator+1:]))
		if !filepath.IsAbs(candidate) || filepath.Ext(candidate) != ".appex" {
			continue
		}
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		paths = append(paths, candidate)
	}
	return paths
}

func normalizedFSKitModulePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = filepath.Clean(path)
		if path == "." || path == "" {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func sameFSKitModulePaths(left []string, right []string) bool {
	return strings.Join(normalizedFSKitModulePaths(left), "\x00") == strings.Join(normalizedFSKitModulePaths(right), "\x00")
}

func staleFSKitModulePaths(paths []string, target string) []string {
	target = filepath.Clean(target)
	var stale []string
	for _, path := range normalizedFSKitModulePaths(paths) {
		if path != target {
			stale = append(stale, path)
		}
	}
	return stale
}

func waitForFSKitModulePath(ctx context.Context, target string, timeout time.Duration) error {
	target = filepath.Clean(target)
	if timeout <= 0 {
		timeout = fsKitRegistrationWait
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last []string
	var lastErr error
	for {
		paths, err := registeredFSKitModulePaths(ctx)
		if err == nil {
			last = paths
			lastErr = nil
			for _, path := range paths {
				if filepath.Clean(path) == target {
					return nil
				}
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if lastErr != nil {
				return fmt.Errorf("FSKit module registration query failed: %w", lastErr)
			}
			return fmt.Errorf("FSKit module registration did not include %s: got %v", target, normalizedFSKitModulePaths(last))
		case <-ticker.C:
		}
	}
}

func waitForFSKitModulePathAbsent(ctx context.Context, target string, timeout time.Duration) error {
	target = filepath.Clean(target)
	if timeout <= 0 {
		timeout = fsKitRegistrationWait
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last []string
	var lastErr error
	for {
		paths, err := registeredFSKitModulePaths(ctx)
		if err == nil {
			last = paths
			lastErr = nil
			if !containsFSKitModulePath(paths, target) {
				return nil
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if lastErr != nil {
				return fmt.Errorf("FSKit module registration query failed: %w", lastErr)
			}
			return fmt.Errorf("FSKit module registration still includes %s: got %v", target, normalizedFSKitModulePaths(last))
		case <-ticker.C:
		}
	}
}

func waitForFSKitModulePaths(ctx context.Context, want []string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = fsKitRegistrationWait
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last []string
	var lastErr error
	for {
		paths, err := registeredFSKitModulePaths(ctx)
		if err == nil {
			last = paths
			lastErr = nil
			if sameFSKitModulePaths(paths, want) {
				return nil
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if lastErr != nil {
				return fmt.Errorf("FSKit module registration query failed: %w", lastErr)
			}
			return fmt.Errorf("FSKit module registration did not converge: got %v, want %v", normalizedFSKitModulePaths(last), normalizedFSKitModulePaths(want))
		case <-ticker.C:
		}
	}
}

func fsKitParentAppPath(modulePath string) (string, bool) {
	extensions := filepath.Dir(filepath.Clean(modulePath))
	if filepath.Base(extensions) != "Extensions" {
		return "", false
	}
	contents := filepath.Dir(extensions)
	if filepath.Base(contents) != "Contents" {
		return "", false
	}
	app := filepath.Dir(contents)
	if filepath.Ext(app) != ".app" {
		return "", false
	}
	return app, true
}

func (t *darwinFSKitAppTransaction) AppGroupPath() string { return t.appGroupPath }
func (t *darwinFSKitAppTransaction) Changed() bool        { return t.changed }
func (t *darwinFSKitAppTransaction) Residency() FSKitResidencyOutcome {
	return t.residency
}

// promoteStagedApp preserves an existing app bundle root because macOS can
// attach launch authorization to that directory's inode. Swapping Contents is
// atomic on APFS and keeps the previous version in stagePath for rollback.
func (t *darwinFSKitAppTransaction) promoteStagedApp() error {
	if t == nil || t.target == "" || t.stagePath == "" {
		return errors.New("FSKit app transaction is incomplete")
	}
	if info, err := os.Stat(t.target); err == nil {
		if !info.IsDir() {
			return errors.New("installed FSKit app path is not a directory")
		}
		t.hadTarget = true
		targetContents := filepath.Join(t.target, "Contents")
		stagedContents := filepath.Join(t.stagePath, "Contents")
		if info, err := os.Stat(targetContents); err != nil {
			return fmt.Errorf("inspect installed FSKit app Contents: %w", err)
		} else if !info.IsDir() {
			return errors.New("installed FSKit app Contents is not a directory")
		}
		if info, err := os.Stat(stagedContents); err != nil {
			return fmt.Errorf("inspect staged FSKit app Contents: %w", err)
		} else if !info.IsDir() {
			return errors.New("staged FSKit app Contents is not a directory")
		}
		if err := unix.RenamexNp(stagedContents, targetContents, unix.RENAME_SWAP); err != nil {
			return fmt.Errorf("atomically replace FSKit app Contents: %w", err)
		}
		t.contentsSwapped = true
		return syncDirectories(t.target, t.stagePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(t.stagePath, t.target); err != nil {
		return err
	}
	t.appInstalled = true
	return syncDirectory(filepath.Dir(t.target))
}

func (t *darwinFSKitAppTransaction) rollbackStagedApp() error {
	if t == nil {
		return nil
	}
	if t.contentsSwapped {
		targetContents := filepath.Join(t.target, "Contents")
		stagedContents := filepath.Join(t.stagePath, "Contents")
		if err := unix.RenamexNp(stagedContents, targetContents, unix.RENAME_SWAP); err != nil {
			return fmt.Errorf("restore FSKit app Contents: %w", err)
		}
		t.contentsSwapped = false
		return syncDirectories(t.target, t.stagePath)
	}
	if t.appInstalled {
		if err := os.RemoveAll(t.target); err != nil {
			return err
		}
		t.appInstalled = false
		return syncDirectory(filepath.Dir(t.target))
	}
	return nil
}

func (t *darwinFSKitAppTransaction) Rollback(ctx context.Context) error {
	if t == nil {
		return nil
	}
	if !t.changed {
		return t.registration.restore(ctx, t.target)
	}
	var result error
	if err := quiesceFSKitAppForUpdate(ctx, t.target); err != nil {
		return err
	}
	if t.appInstalled && !t.registration.registered {
		result = errors.Join(result, t.registration.restore(ctx, t.target))
	}
	if err := t.rollbackStagedApp(); err != nil {
		// Preserve the staged previous app for an operator-visible recovery rather
		// than deleting the only rollback material after a failed restore.
		return errors.Join(result, err)
	}
	if t.hadTarget && t.registration.registered {
		if _, _, err := refreshFSKitApp(ctx, t.target, t.manageResidency); err != nil {
			result = errors.Join(result, err)
		}
	}
	result = errors.Join(result, t.registration.restore(ctx, t.target))
	t.changed = false
	return errors.Join(result, t.Commit())
}

func (t *darwinFSKitAppTransaction) Commit() error {
	if t == nil {
		return nil
	}
	var result error
	for _, path := range []string{t.stageRoot} {
		if path == "" {
			continue
		}
		if err := os.RemoveAll(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	t.stageRoot = ""
	t.stagePath = ""
	return errors.Join(result, syncDirectory(filepath.Dir(t.target)))
}

func validateAndEnableFSKitApp(ctx context.Context, appPath string, manageResidency bool) (string, FSKitResidencyOutcome, error) {
	if err := validateFSKitApp(ctx, appPath); err != nil {
		return "", FSKitResidencyOutcome{}, err
	}
	targetModule, err := service.FSKitModulePath(appPath)
	if err != nil {
		return "", FSKitResidencyOutcome{}, err
	}
	if output, err := runFSKitLaunchServicesCommand(ctx, "-f", "-R", "-trusted", appPath); err != nil {
		return "", FSKitResidencyOutcome{}, commandOutputError("register FSKit app", output, err)
	}
	if err := waitForFSKitModulePath(ctx, targetModule, fsKitRegistrationWait); err != nil {
		return "", FSKitResidencyOutcome{}, err
	}
	if _, err := ensureFSKitModuleEnabled(service.FSKitModuleIdentifier); err != nil {
		return "", FSKitResidencyOutcome{}, err
	}
	if output, err := exec.CommandContext(ctx, "/usr/bin/pluginkit", "-e", "use", "-p", "com.apple.fskit.fsmodule", "-i", service.FSKitModuleIdentifier).CombinedOutput(); err != nil {
		return "", FSKitResidencyOutcome{}, commandOutputError("enable FSKit extension election", output, err)
	}
	launcher, err := service.FSKitHostLauncherPath(appPath)
	if err != nil {
		return "", FSKitResidencyOutcome{}, err
	}
	output, err := exec.CommandContext(ctx, launcher, "--app-group-path").CombinedOutput()
	if err != nil {
		return "", FSKitResidencyOutcome{}, commandOutputError("resolve FSKit App Group path", output, err)
	}
	appGroup := filepath.Clean(strings.TrimSpace(string(output)))
	if !filepath.IsAbs(appGroup) || filepath.Base(appGroup) != service.FSKitAppGroupIdentifier {
		return "", FSKitResidencyOutcome{}, fmt.Errorf("FSKit host returned invalid App Group path %q", appGroup)
	}
	if !manageResidency {
		return appGroup, FSKitResidencyOutcome{}, nil
	}
	residency, err := configureFSKitResidency(ctx, launcher)
	if err != nil {
		return "", FSKitResidencyOutcome{}, err
	}
	return appGroup, residency, nil
}

func configureFSKitResidency(ctx context.Context, launcher string) (FSKitResidencyOutcome, error) {
	output, err := runFSKitResidencyCommand(ctx, launcher)
	if err != nil {
		return FSKitResidencyOutcome{}, commandOutputError("configure FSKit residency", output, err)
	}
	var outcome FSKitResidencyOutcome
	if err := json.Unmarshal(output, &outcome); err != nil {
		return FSKitResidencyOutcome{}, fmt.Errorf("decode FSKit residency outcome: %w", err)
	}
	if outcome.SchemaVersion != 1 {
		return FSKitResidencyOutcome{}, fmt.Errorf("unsupported FSKit residency outcome schema %d", outcome.SchemaVersion)
	}
	validState := func(state string) bool {
		switch state {
		case "enabled", "requires_approval", "not_found", "unavailable":
			return true
		default:
			return false
		}
	}
	if !validState(outcome.IncidentMonitor.State) || !validState(outcome.LaunchAtLogin.State) {
		return FSKitResidencyOutcome{}, errors.New("FSKit residency outcome contains an invalid service state")
	}
	ready := outcome.IncidentMonitor.State == "enabled" && outcome.LaunchAtLogin.State == "enabled"
	requiresApproval := outcome.IncidentMonitor.State == "requires_approval" || outcome.LaunchAtLogin.State == "requires_approval"
	if outcome.Ready != ready || outcome.RequiresApproval != requiresApproval {
		return FSKitResidencyOutcome{}, errors.New("FSKit residency outcome is internally inconsistent")
	}
	return outcome, nil
}

func ensureFSKitMenuBarResidency(ctx context.Context, appPath string) FSKitResidencyServiceOutcome {
	launcher, err := service.FSKitHostLauncherPath(appPath)
	if err != nil {
		return FSKitResidencyServiceOutcome{State: "unavailable", Detail: err.Error()}
	}
	running, err := inspectFSKitMenuBarProcess(ctx, launcher)
	if err != nil {
		return FSKitResidencyServiceOutcome{State: "unavailable", Detail: err.Error()}
	}
	if running {
		return FSKitResidencyServiceOutcome{State: "enabled"}
	}
	output, err := runFSKitMenuBarOpenCommand(ctx, appPath)
	if err != nil {
		return FSKitResidencyServiceOutcome{
			State:  "unavailable",
			Detail: commandOutputError("start CodexFold menu-bar app", output, err).Error(),
		}
	}

	deadline := time.NewTimer(fsKitMenuBarLaunchWait)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		running, inspectErr := inspectFSKitMenuBarProcess(ctx, launcher)
		if inspectErr != nil {
			return FSKitResidencyServiceOutcome{State: "unavailable", Detail: inspectErr.Error()}
		}
		if running {
			return FSKitResidencyServiceOutcome{State: "enabled"}
		}
		select {
		case <-ctx.Done():
			return FSKitResidencyServiceOutcome{State: "unavailable", Detail: ctx.Err().Error()}
		case <-deadline.C:
			return FSKitResidencyServiceOutcome{
				State:  "unavailable",
				Detail: "CodexFold menu-bar app did not remain running after launch",
			}
		case <-ticker.C:
		}
	}
}

func fsKitMenuBarProcessRunning(ctx context.Context, launcher string) (bool, error) {
	pids, err := listFSKitUserProcessIDs(ctx, codexFoldFSKitHostProcessName)
	if err != nil {
		return false, fmt.Errorf("inspect CodexFold menu-bar processes: %w", err)
	}
	launcher = filepath.Clean(launcher)
	for _, pid := range pids {
		executable, commandErr := inspectFSKitProcessExecutable(ctx, pid)
		if commandErr != nil {
			return false, fmt.Errorf("inspect CodexFold menu-bar process %d: %w", pid, commandErr)
		}
		if executable != launcher {
			continue
		}
		command, commandErr := inspectFSKitProcessCommand(ctx, pid)
		if commandErr != nil {
			return false, fmt.Errorf("inspect CodexFold menu-bar process %d arguments: %w", pid, commandErr)
		}
		if fsKitMenuBarCommandHasNoArguments(command, launcher) {
			return true, nil
		}
	}
	return false, nil
}

func fsKitMenuBarCommandHasNoArguments(command, launcher string) bool {
	command = strings.TrimSpace(command)
	launcher = filepath.Clean(launcher)
	return command == launcher || command == filepath.Base(launcher)
}

func validateFSKitApp(ctx context.Context, appPath string) error {
	launcher, err := service.FSKitHostLauncherPath(appPath)
	if err != nil {
		return err
	}
	module, err := service.FSKitModulePath(appPath)
	if err != nil {
		return err
	}
	if info, err := os.Stat(launcher); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		if err != nil {
			return err
		}
		return errors.New("FSKit host launcher is not executable")
	}
	if info, err := os.Stat(module); err != nil || !info.IsDir() {
		if err != nil {
			return err
		}
		return errors.New("FSKit module bundle is missing")
	}
	checks := []struct {
		path string
		key  string
		want string
	}{
		{filepath.Join(appPath, "Contents", "Info.plist"), "CFBundleIdentifier", service.FSKitHostBundleIdentifier},
		{filepath.Join(module, "Contents", "Info.plist"), "CFBundleIdentifier", service.FSKitModuleIdentifier},
	}
	for _, check := range checks {
		output, err := exec.CommandContext(ctx, "/usr/bin/plutil", "-extract", check.key, "raw", check.path).CombinedOutput()
		if err != nil {
			return commandOutputError("read FSKit bundle identity", output, err)
		}
		if strings.TrimSpace(string(output)) != check.want {
			return fmt.Errorf("FSKit bundle identifier=%q expected=%q", strings.TrimSpace(string(output)), check.want)
		}
	}
	if output, err := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", "--verbose=2", appPath).CombinedOutput(); err != nil {
		return commandOutputError("verify FSKit app signature", output, err)
	}
	entitlements, err := exec.CommandContext(ctx, "/usr/bin/codesign", "-d", "--entitlements", ":-", "--xml", module).CombinedOutput()
	if err != nil {
		return commandOutputError("read FSKit module entitlements", entitlements, err)
	}
	for _, required := range []string{"com.apple.developer.fskit.fsmodule", "com.apple.security.app-sandbox", "com.apple.security.application-groups", service.FSKitAppGroupIdentifier} {
		if !strings.Contains(string(entitlements), required) {
			return fmt.Errorf("FSKit module signature is missing %s", required)
		}
	}
	profile := filepath.Join(module, "Contents", "embedded.provisionprofile")
	profileData, err := exec.CommandContext(ctx, "/usr/bin/security", "cms", "-D", "-i", profile).CombinedOutput()
	if err != nil {
		return commandOutputError("read FSKit module provisioning profile", profileData, err)
	}
	if !strings.Contains(string(profileData), service.FSKitAppGroupIdentifier) {
		return errors.New("FSKit module provisioning profile does not authorize the App Group")
	}
	return nil
}

func requireNewerFSKitAppVersion(ctx context.Context, source string, target string) error {
	sourceVersion, err := readFSKitAppVersion(ctx, source)
	if err != nil {
		return err
	}
	targetVersion, err := readFSKitAppVersion(ctx, target)
	if err != nil {
		return err
	}
	comparison, err := compareFSKitBundleVersions(sourceVersion, targetVersion)
	if err != nil {
		return err
	}
	if comparison <= 0 {
		return fmt.Errorf("FSKit app candidate CFBundleVersion %s must exceed installed version %s when bundle contents differ", sourceVersion, targetVersion)
	}
	return nil
}

func readFSKitAppVersion(ctx context.Context, appPath string) (string, error) {
	infoPath := filepath.Join(appPath, "Contents", "Info.plist")
	output, err := exec.CommandContext(ctx, "/usr/bin/plutil", "-extract", "CFBundleVersion", "raw", infoPath).CombinedOutput()
	if err != nil {
		return "", commandOutputError("read FSKit app version", output, err)
	}
	version := strings.TrimSpace(string(output))
	if _, err := parseFSKitBundleVersion(version); err != nil {
		return "", err
	}
	return version, nil
}

func compareFSKitBundleVersions(left string, right string) (int, error) {
	leftParts, err := parseFSKitBundleVersion(left)
	if err != nil {
		return 0, err
	}
	rightParts, err := parseFSKitBundleVersion(right)
	if err != nil {
		return 0, err
	}
	for index := 0; index < max(len(leftParts), len(rightParts)); index++ {
		var leftPart, rightPart uint64
		if index < len(leftParts) {
			leftPart = leftParts[index]
		}
		if index < len(rightParts) {
			rightPart = rightParts[index]
		}
		if leftPart < rightPart {
			return -1, nil
		}
		if leftPart > rightPart {
			return 1, nil
		}
	}
	return 0, nil
}

func parseFSKitBundleVersion(version string) ([]uint64, error) {
	parts := strings.Split(strings.TrimSpace(version), ".")
	if len(parts) == 0 || len(parts) > 3 {
		return nil, fmt.Errorf("invalid FSKit CFBundleVersion %q", version)
	}
	parsed := make([]uint64, len(parts))
	for index, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("invalid FSKit CFBundleVersion %q", version)
		}
		value, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid FSKit CFBundleVersion %q: %w", version, err)
		}
		parsed[index] = value
	}
	return parsed, nil
}

func ensureFSKitModuleEnabled(moduleID string) (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	path := filepath.Join(home, "Library", "Group Containers", "group.com.apple.fskit.settings", "enabledModules.plist")
	output, err := exec.Command("/usr/bin/plutil", "-convert", "json", "-o", "-", path).CombinedOutput()
	if err != nil {
		// macOS may TCC-deny direct reads of Apple's FSKit settings container
		// from ordinary developer tools. The caller still elects the module with
		// pluginkit(1); treat this as already-handled rather than hard-failing
		// acceptance installs that cannot rewrite enabledModules.plist.
		if fsKitSettingsPermissionDenied(output, err) {
			return false, nil
		}
		return false, commandOutputError("read enabled FSKit modules", output, err)
	}
	var modules []string
	if err := json.Unmarshal(output, &modules); err != nil {
		return false, err
	}
	for _, current := range modules {
		if current == moduleID {
			return false, nil
		}
	}
	command := fmt.Sprintf("Add :%d string %s", len(modules), moduleID)
	if output, err := exec.Command("/usr/libexec/PlistBuddy", "-c", command, path).CombinedOutput(); err != nil {
		if fsKitSettingsPermissionDenied(output, err) {
			return false, nil
		}
		return false, commandOutputError("enable FSKit module", output, err)
	}
	if output, err := exec.Command("/usr/bin/plutil", "-lint", path).CombinedOutput(); err != nil {
		return false, commandOutputError("validate enabled FSKit modules", output, err)
	}
	return true, nil
}

func fsKitSettingsPermissionDenied(output []byte, err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(string(output) + "\n" + err.Error())
	return strings.Contains(text, "operation not permitted") ||
		strings.Contains(text, "don't have permission") ||
		strings.Contains(text, "couldn’t be opened because you don’t have permission") ||
		strings.Contains(text, "couldn't be opened because you don't have permission")
}

func userProcessIDs(ctx context.Context, name string) ([]int, error) {
	output, err := exec.CommandContext(ctx, "/usr/bin/pgrep", "-u", strconv.Itoa(os.Getuid()), "-x", name).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var result []int
	for _, field := range strings.Fields(string(output)) {
		pid, parseErr := strconv.Atoi(field)
		if parseErr == nil && pid > 1 {
			result = append(result, pid)
		}
	}
	return result, nil
}

func fsKitProcessExecutablePath(ctx context.Context, pid int) (string, error) {
	output, err := exec.CommandContext(
		ctx,
		"/bin/ps",
		"-p", strconv.Itoa(pid),
		"-o", "comm=",
	).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", nil
		}
		return "", err
	}
	path := strings.TrimSpace(string(output))
	if path == "" {
		return "", nil
	}
	return filepath.Clean(path), nil
}

func fsKitProcessCommandLine(ctx context.Context, pid int) (string, error) {
	output, err := exec.CommandContext(
		ctx,
		"/bin/ps",
		"-ww",
		"-p", strconv.Itoa(pid),
		"-o", "command=",
	).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func fsKitModuleExecutablePath(appPath string) (string, error) {
	modulePath, err := service.FSKitModulePath(appPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(modulePath, "Contents", "MacOS", codexFoldFSKitModuleProcessName), nil
}

func fsKitModuleProcessIDs(ctx context.Context, appPath string) ([]int, error) {
	executable, err := fsKitModuleExecutablePath(appPath)
	if err != nil {
		return nil, err
	}
	pids, err := listFSKitUserProcessIDs(ctx, codexFoldFSKitModuleProcessName)
	if err != nil {
		return nil, fmt.Errorf("inspect CodexFold FSKit module processes: %w", err)
	}
	matched := make([]int, 0, len(pids))
	for _, pid := range pids {
		current, inspectErr := inspectFSKitProcessExecutable(ctx, pid)
		if inspectErr != nil {
			return nil, fmt.Errorf("inspect CodexFold FSKit module process %d: %w", pid, inspectErr)
		}
		if current == filepath.Clean(executable) {
			matched = append(matched, pid)
		}
	}
	return matched, nil
}

func stopCodexFoldFSKitModuleProcesses(ctx context.Context, appPath string) error {
	pids, err := fsKitModuleProcessIDs(ctx, appPath)
	if err != nil {
		return err
	}
	for _, pid := range pids {
		if err := signalFSKitProcess(pid, unix.SIGTERM); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("stop CodexFold FSKit module process %d: %w", pid, err)
		}
	}
	if err := waitForNoCodexFoldFSKitModuleProcesses(ctx, appPath, fsKitModuleShutdownWait); err != nil {
		remaining, inspectErr := fsKitModuleProcessIDs(ctx, appPath)
		if inspectErr != nil {
			return errors.Join(err, inspectErr)
		}
		for _, pid := range remaining {
			if killErr := signalFSKitProcess(pid, unix.SIGKILL); killErr != nil && !errors.Is(killErr, unix.ESRCH) {
				return errors.Join(err, fmt.Errorf("force-stop CodexFold FSKit module process %d: %w", pid, killErr))
			}
		}
		return waitForNoCodexFoldFSKitModuleProcesses(ctx, appPath, fsKitModuleShutdownWait)
	}
	return nil
}

func waitForNoCodexFoldFSKitModuleProcesses(ctx context.Context, appPath string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = fsKitModuleShutdownWait
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		pids, err := fsKitModuleProcessIDs(ctx, appPath)
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("CodexFold FSKit module processes remain: %v", pids)
		case <-ticker.C:
		}
	}
}

func hashAppBundle(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", errors.New("app bundle path must be absolute")
	}
	hash := sha256.New()
	err := filepath.WalkDir(filepath.Clean(root), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		_, _ = io.WriteString(hash, relative)
		_, _ = io.WriteString(hash, "\x00"+info.Mode().String()+"\x00")
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_, _ = io.WriteString(hash, target)
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		return errors.Join(copyErr, closeErr)
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(filepath.Clean(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func syncDirectories(paths ...string) error {
	seen := make(map[string]struct{}, len(paths))
	var result error
	for _, path := range paths {
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		result = errors.Join(result, syncDirectory(path))
	}
	return result
}

func commandOutputError(action string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, detail)
}

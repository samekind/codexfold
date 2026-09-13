//go:build darwin

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/samekind/codexfold/internal/service"
	"golang.org/x/sys/unix"
)

func TestFSKitSettingsPermissionDenied(t *testing.T) {
	if !fsKitSettingsPermissionDenied([]byte("Operation not permitted"), errors.New("exit status 1")) {
		t.Fatal("expected Operation not permitted to be treated as TCC denial")
	}
	if fsKitSettingsPermissionDenied([]byte("file does not exist"), errors.New("exit status 1")) {
		t.Fatal("did not expect missing-file errors to be treated as TCC denial")
	}
}

func TestConfigureFSKitResidencyReturnsExplicitReadyAndApprovalOutcomes(t *testing.T) {
	original := runFSKitResidencyCommand
	defer func() { runFSKitResidencyCommand = original }()

	for _, test := range []struct {
		name             string
		payload          string
		wantReady        bool
		wantApproval     bool
		wantMonitorState string
	}{
		{
			name:             "ready",
			payload:          `{"schema_version":1,"incident_monitor":{"state":"enabled"},"launch_at_login":{"state":"enabled"},"ready":true,"requires_approval":false}`,
			wantReady:        true,
			wantMonitorState: "enabled",
		},
		{
			name:             "requires approval",
			payload:          `{"schema_version":1,"incident_monitor":{"state":"requires_approval"},"launch_at_login":{"state":"enabled"},"ready":false,"requires_approval":true}`,
			wantApproval:     true,
			wantMonitorState: "requires_approval",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runFSKitResidencyCommand = func(context.Context, string) ([]byte, error) {
				return []byte(test.payload), nil
			}
			outcome, err := configureFSKitResidency(context.Background(), "/tmp/CodexFoldFSKit")
			if err != nil {
				t.Fatalf("configure residency: %v", err)
			}
			if outcome.Ready != test.wantReady || outcome.RequiresApproval != test.wantApproval || outcome.IncidentMonitor.State != test.wantMonitorState {
				t.Fatalf("residency outcome = %#v", outcome)
			}
		})
	}
}

func TestConfigureFSKitResidencyFailsClosedOnInconsistentOutcome(t *testing.T) {
	original := runFSKitResidencyCommand
	defer func() { runFSKitResidencyCommand = original }()
	runFSKitResidencyCommand = func(context.Context, string) ([]byte, error) {
		return []byte(`{"schema_version":1,"incident_monitor":{"state":"requires_approval"},"launch_at_login":{"state":"enabled"},"ready":true,"requires_approval":false}`), nil
	}
	if _, err := configureFSKitResidency(context.Background(), "/tmp/CodexFoldFSKit"); err == nil {
		t.Fatal("inconsistent residency outcome was accepted")
	}
}

func TestCompleteFSKitResidencyRequiresCurrentMenuBar(t *testing.T) {
	outcome := FSKitResidencyOutcome{
		SchemaVersion:   1,
		IncidentMonitor: FSKitResidencyServiceOutcome{State: "enabled"},
		LaunchAtLogin:   FSKitResidencyServiceOutcome{State: "enabled"},
		Ready:           true,
	}
	completeFSKitResidency(&outcome, FSKitResidencyServiceOutcome{State: "unavailable"})
	if outcome.Ready || outcome.MenuBar.State != "unavailable" {
		t.Fatalf("unavailable current menu bar was reported ready: %#v", outcome)
	}
	completeFSKitResidency(&outcome, FSKitResidencyServiceOutcome{State: "enabled"})
	if !outcome.Ready || outcome.MenuBar.State != "enabled" {
		t.Fatalf("enabled current menu bar was not reported ready: %#v", outcome)
	}
}

func stubNoFSKitHostProcesses(t *testing.T) {
	t.Helper()
	originalList := listFSKitUserProcessIDs
	originalSignal := signalFSKitProcess
	t.Cleanup(func() {
		listFSKitUserProcessIDs = originalList
		signalFSKitProcess = originalSignal
	})
	listFSKitUserProcessIDs = func(context.Context, string) ([]int, error) { return nil, nil }
	signalFSKitProcess = func(pid int, sig unix.Signal) error {
		t.Fatalf("unexpected signal %s to pid %d", sig, pid)
		return nil
	}
}

func TestEnsureFSKitMenuBarResidencyDoesNotRelaunchRunningApp(t *testing.T) {
	stubNoFSKitHostProcesses(t)
	originalInspect := inspectFSKitMenuBarProcess
	originalOpen := runFSKitMenuBarOpenCommand
	t.Cleanup(func() {
		inspectFSKitMenuBarProcess = originalInspect
		runFSKitMenuBarOpenCommand = originalOpen
	})
	inspectFSKitMenuBarProcess = func(context.Context, string) (bool, error) { return true, nil }
	openCalls := 0
	runFSKitMenuBarOpenCommand = func(context.Context, string) ([]byte, error) {
		openCalls++
		return nil, nil
	}

	outcome := ensureFSKitMenuBarResidency(context.Background(), "/tmp/CodexFoldFSKit.app")
	if outcome.State != "enabled" || openCalls != 0 {
		t.Fatalf("running menu bar outcome=%#v open_calls=%d", outcome, openCalls)
	}
}

func TestEnsureFSKitMenuBarResidencyLaunchesOnlyCodexFoldApp(t *testing.T) {
	stubNoFSKitHostProcesses(t)
	originalInspect := inspectFSKitMenuBarProcess
	originalOpen := runFSKitMenuBarOpenCommand
	t.Cleanup(func() {
		inspectFSKitMenuBarProcess = originalInspect
		runFSKitMenuBarOpenCommand = originalOpen
	})
	inspectCalls := 0
	inspectFSKitMenuBarProcess = func(_ context.Context, launcher string) (bool, error) {
		inspectCalls++
		if launcher != "/tmp/CodexFoldFSKit.app/Contents/MacOS/CodexFoldFSKit" {
			t.Fatalf("inspected unexpected launcher %q", launcher)
		}
		return inspectCalls > 1, nil
	}
	openedPath := ""
	runFSKitMenuBarOpenCommand = func(_ context.Context, appPath string) ([]byte, error) {
		openedPath = appPath
		return nil, nil
	}

	outcome := ensureFSKitMenuBarResidency(context.Background(), "/tmp/CodexFoldFSKit.app")
	if outcome.State != "enabled" || openedPath != "/tmp/CodexFoldFSKit.app" {
		t.Fatalf("menu bar launch outcome=%#v opened=%q", outcome, openedPath)
	}
}

func TestEnsureFSKitMenuBarResidencyReportsLaunchFailureWithoutFallback(t *testing.T) {
	stubNoFSKitHostProcesses(t)
	originalInspect := inspectFSKitMenuBarProcess
	originalOpen := runFSKitMenuBarOpenCommand
	t.Cleanup(func() {
		inspectFSKitMenuBarProcess = originalInspect
		runFSKitMenuBarOpenCommand = originalOpen
	})
	inspectFSKitMenuBarProcess = func(context.Context, string) (bool, error) { return false, nil }
	runFSKitMenuBarOpenCommand = func(context.Context, string) ([]byte, error) {
		return []byte("launch denied"), errors.New("open failed")
	}

	outcome := ensureFSKitMenuBarResidency(context.Background(), "/tmp/CodexFoldFSKit.app")
	if outcome.State != "unavailable" || outcome.Detail == "" {
		t.Fatalf("failed menu bar launch outcome = %#v", outcome)
	}
}

func TestStopCodexFoldFSKitHostProcessesStopsStagedLeftoverMenuBar(t *testing.T) {
	originalList := listFSKitUserProcessIDs
	originalExecutable := inspectFSKitProcessExecutable
	originalCommand := inspectFSKitProcessCommand
	originalSignal := signalFSKitProcess
	t.Cleanup(func() {
		listFSKitUserProcessIDs = originalList
		inspectFSKitProcessExecutable = originalExecutable
		inspectFSKitProcessCommand = originalCommand
		signalFSKitProcess = originalSignal
	})

	current := "/tmp/CodexFoldFSKit.app/Contents/MacOS/CodexFoldFSKit"
	staged := "/tmp/.codexfold-fskit-stage-1/CodexFoldFSKit.app/Contents/MacOS/CodexFoldFSKit"
	alive := map[int]string{101: staged, 202: current + " --run-helper helper fs serve"}
	listFSKitUserProcessIDs = func(context.Context, string) ([]int, error) {
		var pids []int
		for _, pid := range []int{101, 202} {
			if _, ok := alive[pid]; ok {
				pids = append(pids, pid)
			}
		}
		return pids, nil
	}
	inspectFSKitProcessExecutable = func(_ context.Context, pid int) (string, error) {
		if pid == 101 {
			return staged, nil
		}
		return current, nil
	}
	inspectFSKitProcessCommand = func(_ context.Context, pid int) (string, error) {
		return alive[pid], nil
	}
	signaled := make([]int, 0, 1)
	signalFSKitProcess = func(pid int, sig unix.Signal) error {
		if sig == unix.SIGTERM {
			signaled = append(signaled, pid)
			delete(alive, pid)
		}
		if _, ok := alive[pid]; !ok {
			return unix.ESRCH
		}
		return nil
	}

	if err := stopCodexFoldFSKitHostProcesses(context.Background(), "/tmp/CodexFoldFSKit.app"); err != nil {
		t.Fatal(err)
	}
	if len(signaled) != 1 || signaled[0] != 101 {
		t.Fatalf("signaled=%v, want only staged leftover 101", signaled)
	}
	if _, ok := alive[202]; !ok {
		t.Fatal("helper process was stopped")
	}
}

func TestEnsureFSKitMenuBarResidencyStopsStaleStageHostBeforeLaunch(t *testing.T) {
	originalList := listFSKitUserProcessIDs
	originalExecutable := inspectFSKitProcessExecutable
	originalCommand := inspectFSKitProcessCommand
	originalSignal := signalFSKitProcess
	originalInspect := inspectFSKitMenuBarProcess
	originalOpen := runFSKitMenuBarOpenCommand
	t.Cleanup(func() {
		listFSKitUserProcessIDs = originalList
		inspectFSKitProcessExecutable = originalExecutable
		inspectFSKitProcessCommand = originalCommand
		signalFSKitProcess = originalSignal
		inspectFSKitMenuBarProcess = originalInspect
		runFSKitMenuBarOpenCommand = originalOpen
	})

	staged := "/tmp/.codexfold-fskit-stage-9/CodexFoldFSKit.app/Contents/MacOS/CodexFoldFSKit"
	alive := map[int]bool{77: true}
	listFSKitUserProcessIDs = func(context.Context, string) ([]int, error) {
		if alive[77] {
			return []int{77}, nil
		}
		return nil, nil
	}
	inspectFSKitProcessExecutable = func(context.Context, int) (string, error) { return staged, nil }
	inspectFSKitProcessCommand = func(context.Context, int) (string, error) { return "CodexFoldFSKit", nil }
	signalFSKitProcess = func(pid int, sig unix.Signal) error {
		if pid == 77 && sig == unix.SIGTERM {
			delete(alive, 77)
			return nil
		}
		return unix.ESRCH
	}
	inspectFSKitMenuBarProcess = func(context.Context, string) (bool, error) { return false, nil }
	opened := 0
	runFSKitMenuBarOpenCommand = func(context.Context, string) ([]byte, error) {
		opened++
		inspectFSKitMenuBarProcess = func(context.Context, string) (bool, error) { return true, nil }
		return nil, nil
	}

	outcome := ensureFSKitMenuBarResidency(context.Background(), "/tmp/CodexFoldFSKit.app")
	if outcome.State != "enabled" || opened != 1 || alive[77] {
		t.Fatalf("outcome=%#v opened=%d stale_alive=%t", outcome, opened, alive[77])
	}
}

func TestStaleFSKitMenuBarProcessIDsOnlyReturnsArgumentFreeStageHosts(t *testing.T) {
	originalList := listFSKitUserProcessIDs
	originalExecutable := inspectFSKitProcessExecutable
	originalCommand := inspectFSKitProcessCommand
	t.Cleanup(func() {
		listFSKitUserProcessIDs = originalList
		inspectFSKitProcessExecutable = originalExecutable
		inspectFSKitProcessCommand = originalCommand
	})

	current := "/Users/test/Applications/CodexFoldFSKit.app/Contents/MacOS/CodexFoldFSKit"
	stage := "/Users/test/Applications/.codexfold-fskit-stage-1/CodexFoldFSKit.app/Contents/MacOS/CodexFoldFSKit"
	otherInstalled := "/tmp/Other/CodexFoldFSKit.app/Contents/MacOS/CodexFoldFSKit"
	listFSKitUserProcessIDs = func(context.Context, string) ([]int, error) {
		return []int{101, 202, 303, 404}, nil
	}
	inspectFSKitProcessExecutable = func(_ context.Context, pid int) (string, error) {
		switch pid {
		case 101, 404:
			return stage, nil
		case 202:
			return otherInstalled, nil
		default:
			return "", nil
		}
	}
	inspectFSKitProcessCommand = func(_ context.Context, pid int) (string, error) {
		if pid == 404 {
			return stage + " --run-helper /tmp/helper fs serve", nil
		}
		return stage, nil
	}

	got, err := staleFSKitMenuBarProcessIDs(context.Background(), current)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []int{101}) {
		t.Fatalf("stale stage host pids = %#v, want [101]", got)
	}
}

func TestFSKitMenuBarProcessRunningRequiresArgumentFreeHostProcess(t *testing.T) {
	originalList := listFSKitUserProcessIDs
	originalExecutable := inspectFSKitProcessExecutable
	originalCommand := inspectFSKitProcessCommand
	t.Cleanup(func() {
		listFSKitUserProcessIDs = originalList
		inspectFSKitProcessExecutable = originalExecutable
		inspectFSKitProcessCommand = originalCommand
	})

	launcher := "/tmp/CodexFoldFSKit.app/Contents/MacOS/CodexFoldFSKit"
	listFSKitUserProcessIDs = func(context.Context, string) ([]int, error) {
		return []int{101}, nil
	}
	inspectFSKitProcessExecutable = func(context.Context, int) (string, error) {
		return launcher, nil
	}
	inspectFSKitProcessCommand = func(context.Context, int) (string, error) {
		return launcher + " --run-helper /tmp/codexfold fs service run", nil
	}

	running, err := fsKitMenuBarProcessRunning(context.Background(), launcher)
	if err != nil {
		t.Fatal(err)
	}
	if running {
		t.Fatal("host helper with the menu-bar executable was reported as the menu-bar UI")
	}

	listFSKitUserProcessIDs = func(context.Context, string) ([]int, error) {
		return []int{101, 202}, nil
	}
	inspectFSKitProcessCommand = func(_ context.Context, pid int) (string, error) {
		if pid == 101 {
			return launcher + " --run-helper /tmp/codexfold fs supervise", nil
		}
		return launcher, nil
	}
	running, err = fsKitMenuBarProcessRunning(context.Background(), launcher)
	if err != nil {
		t.Fatal(err)
	}
	if !running {
		t.Fatal("argument-free host process was not reported as the menu-bar UI")
	}
}

func TestCollapseFSKitMenuBarProcessesStopsOlderDuplicates(t *testing.T) {
	originalList := listFSKitUserProcessIDs
	originalExecutable := inspectFSKitProcessExecutable
	originalCommand := inspectFSKitProcessCommand
	originalSignal := signalFSKitProcess
	t.Cleanup(func() {
		listFSKitUserProcessIDs = originalList
		inspectFSKitProcessExecutable = originalExecutable
		inspectFSKitProcessCommand = originalCommand
		signalFSKitProcess = originalSignal
	})

	launcher := "/tmp/CodexFoldFSKit.app/Contents/MacOS/CodexFoldFSKit"
	alive := map[int]bool{101: true, 202: true, 303: true}
	listFSKitUserProcessIDs = func(context.Context, string) ([]int, error) {
		var pids []int
		for _, pid := range []int{101, 202, 303} {
			if alive[pid] {
				pids = append(pids, pid)
			}
		}
		return pids, nil
	}
	inspectFSKitProcessExecutable = func(context.Context, int) (string, error) {
		return launcher, nil
	}
	inspectFSKitProcessCommand = func(_ context.Context, pid int) (string, error) {
		if pid == 202 {
			return launcher + " --run-helper /tmp/helper fs serve", nil
		}
		return launcher, nil
	}
	var signals []int
	signalFSKitProcess = func(pid int, signal syscall.Signal) error {
		if pid == 303 {
			t.Fatal("newest menu-bar process was signalled")
		}
		if pid == 202 {
			t.Fatal("helper process was signalled as a menu-bar duplicate")
		}
		if signal != unix.SIGTERM {
			t.Fatalf("signal = %v, want SIGTERM", signal)
		}
		signals = append(signals, pid)
		delete(alive, pid)
		return nil
	}

	if err := collapseFSKitMenuBarProcesses(context.Background(), launcher); err != nil {
		t.Fatal(err)
	}
	if len(signals) != 1 || signals[0] != 101 {
		t.Fatalf("signals = %#v, want only older menu-bar pid 101", signals)
	}
	if !alive[202] || !alive[303] {
		t.Fatal("helper or newest menu-bar process did not remain alive")
	}
}

func TestFSKitMenuBarCommandHasNoArgumentsAcceptsLaunchdArgvZeroOnly(t *testing.T) {
	launcher := "/tmp/CodexFoldFSKit.app/Contents/MacOS/CodexFoldFSKit"
	for _, command := range []string{launcher, filepath.Base(launcher)} {
		if !fsKitMenuBarCommandHasNoArguments(command, launcher) {
			t.Fatalf("argument-free command %q was rejected", command)
		}
	}
	for _, command := range []string{
		"",
		launcher + " --run-helper /tmp/helper fs serve",
		launcher + " --run-helper /tmp/helper fs supervise",
		launcher + " --configure-residency",
		filepath.Base(launcher) + " --app-group-path",
	} {
		if fsKitMenuBarCommandHasNoArguments(command, launcher) {
			t.Fatalf("command with arguments %q was accepted as the menu-bar UI", command)
		}
	}
}

func TestStopFSKitModuleProcessesSignalsOnlyTheExactTargetApp(t *testing.T) {
	originalList := listFSKitUserProcessIDs
	originalInspect := inspectFSKitProcessExecutable
	originalSignal := signalFSKitProcess
	t.Cleanup(func() {
		listFSKitUserProcessIDs = originalList
		inspectFSKitProcessExecutable = originalInspect
		signalFSKitProcess = originalSignal
	})

	targetApp := "/tmp/isolated/CodexFoldFSKit.app"
	productionApp := "/Users/test/Applications/CodexFoldFSKit.app"
	targetExecutable, err := fsKitModuleExecutablePath(targetApp)
	if err != nil {
		t.Fatal(err)
	}
	productionExecutable, err := fsKitModuleExecutablePath(productionApp)
	if err != nil {
		t.Fatal(err)
	}
	alive := map[int]bool{101: true, 202: true}
	listFSKitUserProcessIDs = func(context.Context, string) ([]int, error) {
		var pids []int
		for _, pid := range []int{101, 202} {
			if alive[pid] {
				pids = append(pids, pid)
			}
		}
		return pids, nil
	}
	inspectFSKitProcessExecutable = func(_ context.Context, pid int) (string, error) {
		switch pid {
		case 101:
			return targetExecutable, nil
		case 202:
			return productionExecutable, nil
		default:
			return "", nil
		}
	}
	var signals []struct {
		pid    int
		signal syscall.Signal
	}
	signalFSKitProcess = func(pid int, signal syscall.Signal) error {
		signals = append(signals, struct {
			pid    int
			signal syscall.Signal
		}{pid: pid, signal: signal})
		if pid == 202 {
			t.Fatal("production FSKit module was signalled")
		}
		delete(alive, pid)
		return nil
	}

	if err := stopCodexFoldFSKitModuleProcesses(context.Background(), targetApp); err != nil {
		t.Fatal(err)
	}
	if len(signals) != 1 || signals[0].pid != 101 || signals[0].signal != unix.SIGTERM {
		t.Fatalf("signals = %#v, want one SIGTERM for target pid 101", signals)
	}
	if !alive[202] {
		t.Fatal("production FSKit module did not remain alive")
	}
}

func TestReapIdleFSKitModuleProcessesKillsOnlySocketlessDuplicates(t *testing.T) {
	originalList := listFSKitUserProcessIDs
	originalInspect := inspectFSKitProcessExecutable
	originalSocket := inspectFSKitModuleUnixSocket
	originalSignal := signalFSKitProcess
	t.Cleanup(func() {
		listFSKitUserProcessIDs = originalList
		inspectFSKitProcessExecutable = originalInspect
		inspectFSKitModuleUnixSocket = originalSocket
		signalFSKitProcess = originalSignal
	})

	app := "/tmp/CodexFoldFSKit.app"
	executable, err := fsKitModuleExecutablePath(app)
	if err != nil {
		t.Fatal(err)
	}
	alive := map[int]bool{101: true, 202: true}
	listFSKitUserProcessIDs = func(context.Context, string) ([]int, error) {
		var pids []int
		for _, pid := range []int{101, 202} {
			if alive[pid] {
				pids = append(pids, pid)
			}
		}
		return pids, nil
	}
	inspectFSKitProcessExecutable = func(context.Context, int) (string, error) {
		return executable, nil
	}
	inspectFSKitModuleUnixSocket = func(_ context.Context, pid int) (bool, error) {
		return pid == 202, nil
	}
	var signals []struct {
		pid    int
		signal syscall.Signal
	}
	signalFSKitProcess = func(pid int, signal syscall.Signal) error {
		signals = append(signals, struct {
			pid    int
			signal syscall.Signal
		}{pid: pid, signal: signal})
		if pid == 202 {
			t.Fatal("live FSKit module was signalled")
		}
		if signal != unix.SIGKILL {
			t.Fatalf("signal = %v, want SIGKILL", signal)
		}
		delete(alive, pid)
		return nil
	}

	if err := reapIdleCodexFoldFSKitModuleProcesses(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if len(signals) != 1 || signals[0].pid != 101 {
		t.Fatalf("signals = %#v, want SIGKILL for idle pid 101", signals)
	}
	if !alive[202] {
		t.Fatal("live FSKit module did not remain alive")
	}
}

func TestQuiesceNativeFSKitDefinitionSignalsOnlyTheDefinitionApp(t *testing.T) {
	originalList := listFSKitUserProcessIDs
	originalInspect := inspectFSKitProcessExecutable
	originalSignal := signalFSKitProcess
	t.Cleanup(func() {
		listFSKitUserProcessIDs = originalList
		inspectFSKitProcessExecutable = originalInspect
		signalFSKitProcess = originalSignal
	})

	root := t.TempDir()
	targetApp := filepath.Join(root, "isolated", "CodexFoldFSKit.app")
	productionApp := filepath.Join(root, "Applications", "CodexFoldFSKit.app")
	launcher, err := service.FSKitHostLauncherPath(targetApp)
	if err != nil {
		t.Fatal(err)
	}
	targetExecutable, err := fsKitModuleExecutablePath(targetApp)
	if err != nil {
		t.Fatal(err)
	}
	productionExecutable, err := fsKitModuleExecutablePath(productionApp)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := service.RenderLaunchd(service.Options{
		Label: "com.codexfold.fs", BinaryPath: filepath.Join(root, "bin", "codexfold"),
		LauncherPath: launcher, CodexHome: filepath.Join(root, "codex"),
		StoreDir: filepath.Join(root, "store"), MountPoint: filepath.Join(root, "mount"),
		StdoutPath: filepath.Join(root, "logs", "stdout.log"),
		StderrPath: filepath.Join(root, "logs", "stderr.log"), CanonicalNamespace: true,
		NativeRoot: filepath.Join(root, "native"), Frontend: "native-fskit",
		FSKitResource: filepath.Join(root, "store", "fs", "native-fskit.resource"),
	})
	if err != nil {
		t.Fatal(err)
	}
	definitionPath := filepath.Join(root, "com.codexfold.fs.plist")
	if err := os.WriteFile(definitionPath, definition, 0o600); err != nil {
		t.Fatal(err)
	}

	alive := map[int]bool{101: true, 202: true}
	listFSKitUserProcessIDs = func(context.Context, string) ([]int, error) {
		var pids []int
		for _, pid := range []int{101, 202} {
			if alive[pid] {
				pids = append(pids, pid)
			}
		}
		return pids, nil
	}
	inspectFSKitProcessExecutable = func(_ context.Context, pid int) (string, error) {
		switch pid {
		case 101:
			return targetExecutable, nil
		case 202:
			return productionExecutable, nil
		default:
			return "", nil
		}
	}
	var signals []struct {
		pid    int
		signal syscall.Signal
	}
	signalFSKitProcess = func(pid int, signal syscall.Signal) error {
		signals = append(signals, struct {
			pid    int
			signal syscall.Signal
		}{pid: pid, signal: signal})
		if pid == 202 {
			t.Fatal("production FSKit module was signalled")
		}
		delete(alive, pid)
		return nil
	}

	if err := quiesceNativeFSKitDefinition(context.Background(), definitionPath); err != nil {
		t.Fatal(err)
	}
	if len(signals) != 1 || signals[0].pid != 101 || signals[0].signal != unix.SIGTERM {
		t.Fatalf("signals = %#v, want one SIGTERM for target pid 101", signals)
	}
	if !alive[202] {
		t.Fatal("production FSKit module did not remain alive")
	}
}

func TestTargetRegistrationRollbackRemovesOnlyTheTransactionTarget(t *testing.T) {
	targetApp := "/tmp/isolated/CodexFoldFSKit.app"
	productionApp := "/Users/test/Applications/CodexFoldFSKit.app"
	targetModule, err := service.FSKitModulePath(targetApp)
	if err != nil {
		t.Fatal(err)
	}
	productionModule, err := service.FSKitModulePath(productionApp)
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{
		targetModule:     true,
		productionModule: true,
	}
	var registrationCommands [][]string
	installFakeFSKitRegistrationCommands(t, registered, &registrationCommands)

	snapshot := fsKitTargetRegistrationSnapshot{modulePath: targetModule, registered: false}
	if err := snapshot.restore(context.Background(), targetApp); err != nil {
		t.Fatal(err)
	}
	if registered[targetModule] {
		t.Fatal("transaction target registration remains after rollback")
	}
	if !registered[productionModule] {
		t.Fatal("rollback removed the pre-existing production registration")
	}
	want := [][]string{{"-u", targetApp}}
	if !reflect.DeepEqual(registrationCommands, want) {
		t.Fatalf("registration commands = %#v, want %#v", registrationCommands, want)
	}
}

func TestFailedFirstInstallRemovesCandidateRegistrationAndPreservesExistingRegistrations(t *testing.T) {
	originalList := listFSKitUserProcessIDs
	t.Cleanup(func() { listFSKitUserProcessIDs = originalList })
	listFSKitUserProcessIDs = func(context.Context, string) ([]int, error) { return nil, nil }

	root := t.TempDir()
	targetApp := filepath.Join(root, "CodexFoldFSKit.app")
	writeFSKitAppTestFile(t, filepath.Join(targetApp, "Contents", "candidate.txt"), "candidate")
	productionApp := "/Users/test/Applications/CodexFoldFSKit.app"
	targetModule, err := service.FSKitModulePath(targetApp)
	if err != nil {
		t.Fatal(err)
	}
	productionModule, err := service.FSKitModulePath(productionApp)
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{
		targetModule:     true,
		productionModule: true,
	}
	var registrationCommands [][]string
	installFakeFSKitRegistrationCommands(t, registered, &registrationCommands)

	transaction := &darwinFSKitAppTransaction{
		target: targetApp, changed: true, appInstalled: true,
		registration: fsKitTargetRegistrationSnapshot{modulePath: targetModule, registered: false},
	}
	if err := transaction.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(targetApp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed first-install candidate remains: %v", err)
	}
	if registered[targetModule] {
		t.Fatal("failed first-install registration remains")
	}
	if !registered[productionModule] {
		t.Fatal("failed first install removed the production registration")
	}
	want := [][]string{{"-u", targetApp}}
	if !reflect.DeepEqual(registrationCommands, want) {
		t.Fatalf("registration commands = %#v, want %#v", registrationCommands, want)
	}
}

func TestTargetRegistrationRollbackRestoresOnlyAnOriginallyRegisteredTarget(t *testing.T) {
	targetApp := "/tmp/isolated/CodexFoldFSKit.app"
	productionApp := "/Users/test/Applications/CodexFoldFSKit.app"
	targetModule, err := service.FSKitModulePath(targetApp)
	if err != nil {
		t.Fatal(err)
	}
	productionModule, err := service.FSKitModulePath(productionApp)
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{productionModule: true}
	var registrationCommands [][]string
	installFakeFSKitRegistrationCommands(t, registered, &registrationCommands)

	snapshot := fsKitTargetRegistrationSnapshot{modulePath: targetModule, registered: true}
	if err := snapshot.restore(context.Background(), targetApp); err != nil {
		t.Fatal(err)
	}
	if !registered[targetModule] || !registered[productionModule] {
		t.Fatalf("registrations after restore = %#v", registered)
	}
	want := [][]string{{"-f", "-R", "-trusted", targetApp}}
	if !reflect.DeepEqual(registrationCommands, want) {
		t.Fatalf("registration commands = %#v, want %#v", registrationCommands, want)
	}
}

func TestCompareFSKitBundleVersions(t *testing.T) {
	tests := []struct {
		left  string
		right string
		want  int
	}{
		{left: "2", right: "1", want: 1},
		{left: "1.0.1", right: "1", want: 1},
		{left: "1", right: "1.0", want: 0},
		{left: "1.9", right: "2", want: -1},
	}
	for _, test := range tests {
		got, err := compareFSKitBundleVersions(test.left, test.right)
		if err != nil {
			t.Fatalf("compare %q and %q: %v", test.left, test.right, err)
		}
		if got != test.want {
			t.Fatalf("compare %q and %q = %d, want %d", test.left, test.right, got, test.want)
		}
	}
}

func TestCompareFSKitBundleVersionsRejectsInvalidInput(t *testing.T) {
	for _, version := range []string{"", "1.2.3.4", "1.beta"} {
		if _, err := compareFSKitBundleVersions(version, "1"); err == nil {
			t.Fatalf("invalid version %q was accepted", version)
		}
	}
}

func TestParseFSKitModulePathsIncludesDuplicateRegistrations(t *testing.T) {
	output := []byte(`
+    vip.jstar.codexfold.fskitprofileprobe.module(0.1.1)	OLD	2026-07-22 08:23:53 +0000	/private/tmp/old/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex
+    vip.jstar.codexfold.fskitprofileprobe.module(0.1.1)	CURRENT	2026-07-22 09:24:23 +0000	/Users/test/Applications/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex
+    vip.jstar.codexfold.fskitprofileprobe.module(0.1.1)	DUPLICATE	2026-07-22 09:24:24 +0000	/private/tmp/old/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex
 (3 plug-ins)
`)
	want := []string{
		"/private/tmp/old/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex",
		"/Users/test/Applications/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex",
	}
	if got := parseFSKitModulePaths(output); !reflect.DeepEqual(got, want) {
		t.Fatalf("module paths = %#v, want %#v", got, want)
	}
}

func TestSameFSKitModulePathsIgnoresOrderAndDuplicateEntries(t *testing.T) {
	left := []string{"/tmp/current.appex", "/tmp/old.appex", "/tmp/current.appex"}
	right := []string{"/tmp/old.appex", "/tmp/current.appex"}
	if !sameFSKitModulePaths(left, right) {
		t.Fatalf("module path sets differ: left=%v right=%v", left, right)
	}
	if sameFSKitModulePaths(left, []string{"/tmp/current.appex"}) {
		t.Fatal("different module path sets were treated as equal")
	}
}

func TestStaleFSKitModulePathsKeepsInstalledModule(t *testing.T) {
	target := "/Users/test/Applications/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex"
	paths := []string{
		"/private/tmp/candidate/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex",
		target,
		"/private/tmp/old/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex",
		target,
	}
	want := []string{
		"/private/tmp/candidate/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex",
		"/private/tmp/old/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex",
	}
	if got := staleFSKitModulePaths(paths, target); !reflect.DeepEqual(got, want) {
		t.Fatalf("stale module paths = %#v, want %#v", got, want)
	}
}

func TestUnregisterFSKitAppRegistrationUsesLaunchServicesOnly(t *testing.T) {
	original := runFSKitLaunchServicesCommand
	t.Cleanup(func() { runFSKitLaunchServicesCommand = original })
	var got []string
	runFSKitLaunchServicesCommand = func(_ context.Context, args ...string) ([]byte, error) {
		got = append([]string(nil), args...)
		return nil, nil
	}
	app := "/private/tmp/candidate/CodexFoldFSKit.app"
	if err := unregisterFSKitAppRegistration(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if want := []string{"-u", app}; !reflect.DeepEqual(got, want) {
		t.Fatalf("LaunchServices cleanup args = %#v, want %#v", got, want)
	}
}

func TestFSKitParentAppPath(t *testing.T) {
	module := "/private/tmp/CodexFoldFSKit.app/Contents/Extensions/CodexFoldFSKitModule.appex"
	if got, ok := fsKitParentAppPath(module); !ok || got != "/private/tmp/CodexFoldFSKit.app" {
		t.Fatalf("parent app = %q ok=%t", got, ok)
	}
	if _, ok := fsKitParentAppPath("/private/tmp/CodexFoldFSKitModule.appex"); ok {
		t.Fatal("module outside an app bundle unexpectedly had a parent app")
	}
}

func TestFSKitAppContentsSwapPreservesBundleRootAndRollsBack(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "CodexFoldFSKit.app")
	stageRoot := filepath.Join(root, "stage")
	stagePath := filepath.Join(stageRoot, filepath.Base(target))
	writeFSKitAppTestFile(t, filepath.Join(target, "Contents", "old.txt"), "old")
	writeFSKitAppTestFile(t, filepath.Join(stagePath, "Contents", "new.txt"), "new")

	attribute := "com.codexfold.test-root"
	value := []byte("preserve-this-root-xattr")
	if err := unix.Setxattr(target, attribute, value, 0); err != nil {
		t.Fatalf("set root xattr: %v", err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	transaction := &darwinFSKitAppTransaction{
		target: target, stageRoot: stageRoot, stagePath: stagePath, changed: true,
	}
	if err := transaction.promoteStagedApp(); err != nil {
		t.Fatalf("promote staged app: %v", err)
	}
	assertFSKitAppRootUnchanged(t, target, before, attribute, value)
	assertFSKitAppTestFile(t, filepath.Join(target, "Contents", "new.txt"), "new")
	assertFSKitAppTestFile(t, filepath.Join(stagePath, "Contents", "old.txt"), "old")

	if err := transaction.rollbackStagedApp(); err != nil {
		t.Fatalf("rollback staged app: %v", err)
	}
	assertFSKitAppRootUnchanged(t, target, before, attribute, value)
	assertFSKitAppTestFile(t, filepath.Join(target, "Contents", "old.txt"), "old")
	if _, err := os.Stat(filepath.Join(target, "Contents", "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new contents remain after rollback: %v", err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("commit cleanup: %v", err)
	}
	if _, err := os.Stat(stageRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging root remains after commit: %v", err)
	}
}

func TestFSKitAppContentsSwapFirstInstallRemovesOnlyCandidateOnRollback(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "CodexFoldFSKit.app")
	stageRoot := filepath.Join(root, "stage")
	stagePath := filepath.Join(stageRoot, filepath.Base(target))
	writeFSKitAppTestFile(t, filepath.Join(stagePath, "Contents", "new.txt"), "new")
	transaction := &darwinFSKitAppTransaction{
		target: target, stageRoot: stageRoot, stagePath: stagePath, changed: true,
	}
	if err := transaction.promoteStagedApp(); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if !transaction.appInstalled || transaction.hadTarget {
		t.Fatalf("first-install state = %#v", transaction)
	}
	assertFSKitAppTestFile(t, filepath.Join(target, "Contents", "new.txt"), "new")
	if err := transaction.rollbackStagedApp(); err != nil {
		t.Fatalf("rollback first install: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate app remains after rollback: %v", err)
	}
}

func writeFSKitAppTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertFSKitAppTestFile(t *testing.T, path string, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("file %s = %q, want %q", path, got, want)
	}
}

func assertFSKitAppRootUnchanged(t *testing.T, path string, before os.FileInfo, attribute string, want []byte) {
	t.Helper()
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatalf("app bundle root inode changed: before=%#v after=%#v", before.Sys(), after.Sys())
	}
	got := make([]byte, len(want))
	n, err := unix.Getxattr(path, attribute, got)
	if err != nil {
		t.Fatalf("read root xattr: %v", err)
	}
	if string(got[:n]) != string(want) {
		t.Fatalf("root xattr = %q, want %q", got[:n], want)
	}
}

func installFakeFSKitRegistrationCommands(
	t *testing.T,
	registered map[string]bool,
	commands *[][]string,
) {
	t.Helper()
	originalList := runFSKitRegisteredModulesCommand
	originalLaunchServices := runFSKitLaunchServicesCommand
	t.Cleanup(func() {
		runFSKitRegisteredModulesCommand = originalList
		runFSKitLaunchServicesCommand = originalLaunchServices
	})
	runFSKitRegisteredModulesCommand = func(context.Context) ([]byte, error) {
		paths := make([]string, 0, len(registered))
		for path, present := range registered {
			if present {
				paths = append(paths, path)
			}
		}
		sort.Strings(paths)
		var output strings.Builder
		for _, path := range paths {
			fmt.Fprintf(&output, "+ %s\t%s\n", service.FSKitModuleIdentifier, path)
		}
		return []byte(output.String()), nil
	}
	runFSKitLaunchServicesCommand = func(_ context.Context, args ...string) ([]byte, error) {
		*commands = append(*commands, append([]string(nil), args...))
		switch {
		case len(args) == 2 && args[0] == "-u":
			module, err := service.FSKitModulePath(args[1])
			if err != nil {
				return nil, err
			}
			delete(registered, module)
		case len(args) == 4 && reflect.DeepEqual(args[:3], []string{"-f", "-R", "-trusted"}):
			module, err := service.FSKitModulePath(args[3])
			if err != nil {
				return nil, err
			}
			registered[module] = true
		default:
			return nil, fmt.Errorf("unexpected LaunchServices command: %v", args)
		}
		return nil, nil
	}
}

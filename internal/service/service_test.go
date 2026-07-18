package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jstar0/codexfold/internal/compat"
	"github.com/jstar0/codexfold/internal/fsctl"
)

func TestRenderLaunchdUsesAbsoluteArgumentsAndContainsNoSessionContent(t *testing.T) {
	root := t.TempDir()
	definition, err := RenderLaunchd(Options{
		Label: "com.codexfold.fs", BinaryPath: filepath.Join(root, "bin", "codexfold"),
		CodexHome: filepath.Join(root, "codex"), StoreDir: filepath.Join(root, "store"),
		MountPoint: filepath.Join(root, "mount"), StdoutPath: filepath.Join(root, "logs", "stdout.log"),
		StderrPath: filepath.Join(root, "logs", "stderr.log"), CanonicalNamespace: true,
		NativeRoot: filepath.Join(root, "native"), OperationTrace: filepath.Join(root, "logs", "operations.log"),
		EnrollmentInterval: 5 * time.Minute, EnrollmentStableFor: time.Hour, EnrollmentBatchSize: 2,
		EnrollmentCanary: true,
	})
	if err != nil {
		t.Fatalf("RenderLaunchd: %v", err)
	}
	text := string(definition)
	for _, required := range []string{
		"<string>fs</string>", "<string>serve</string>", "<string>--apply</string>",
		"<string>--canonical-namespace</string>", "<string>--native-root</string>",
		"<string>--operation-trace</string>", filepath.Join(root, "logs", "operations.log"),
		"<string>--enrollment-interval</string>", "<string>5m0s</string>",
		"<string>--enrollment-stable-for</string>", "<string>1h0m0s</string>",
		"<string>--enrollment-batch-size</string>", "<string>2</string>",
		"<string>--enrollment-canary</string>",
		filepath.Join(root, "store"), filepath.Join(root, "mount"), filepath.Join(root, "native"),
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("definition missing %q:\n%s", required, text)
		}
	}
	if strings.Contains(text, "session_meta") || strings.Contains(text, "rollout") {
		t.Fatalf("definition contains session content: %s", text)
	}
	if runtime.GOOS == "darwin" {
		path := filepath.Join(root, "service.plist")
		if err := os.WriteFile(path, definition, 0o600); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command("/usr/bin/plutil", "-lint", path).CombinedOutput(); err != nil {
			t.Fatalf("plutil rejected definition: %v\n%s", err, output)
		}
	}
	if _, err := RenderLaunchd(Options{Label: "com.codexfold.fs", BinaryPath: "codexfold", CodexHome: root, StoreDir: root, MountPoint: root, StdoutPath: filepath.Join(root, "out"), StderrPath: filepath.Join(root, "err")}); err == nil {
		t.Fatal("relative binary path should be rejected")
	}
}

func TestRenderLaunchdNativeFSKitSeparatesDaemonAndSupervisor(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "CodexFoldFSKit.app", "Contents", "MacOS", "CodexFoldFSKit")
	binary := filepath.Join(root, "bin", "codexfold")
	options := Options{
		Label: "com.codexfold.fs", BinaryPath: binary, LauncherPath: launcher,
		CodexHome: filepath.Join(root, "codex"), StoreDir: filepath.Join(root, "store"),
		MountPoint: filepath.Join(root, "mount"), StdoutPath: filepath.Join(root, "logs", "stdout.log"),
		StderrPath: filepath.Join(root, "logs", "stderr.log"), CanonicalNamespace: true,
		NativeRoot: filepath.Join(root, "native"), Frontend: "native-fskit",
		FSKitResource: filepath.Join(root, "store", "fs", "native-fskit.resource"),
	}
	daemon, err := RenderLaunchd(options)
	if err != nil {
		t.Fatalf("RenderLaunchd: %v", err)
	}
	for _, required := range []string{
		"<string>" + launcher + "</string>", "<string>--run-helper</string>", "<string>" + binary + "</string>",
		"<string>--frontend</string>", "<string>native-fskit</string>",
		"<string>--fskit-resource</string>", options.FSKitResource,
	} {
		if !strings.Contains(string(daemon), required) {
			t.Fatalf("native daemon definition missing %q:\n%s", required, daemon)
		}
	}

	supervisor, err := RenderLaunchdSupervisor(options)
	if err != nil {
		t.Fatalf("RenderLaunchdSupervisor: %v", err)
	}
	text := string(supervisor)
	for _, required := range []string{
		"<string>com.codexfold.fs.supervisor</string>",
		"<string>" + launcher + "</string>", "<string>--run-helper</string>", "<string>" + binary + "</string>",
		"<string>fs</string>", "<string>supervise</string>", "<string>--apply</string>",
		"<string>--resource</string>", options.FSKitResource,
		"<string>--mount</string>", options.MountPoint,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("native supervisor definition missing %q:\n%s", required, text)
		}
	}
}

func TestRenderLaunchdNativeFSKitRequiresHostLauncher(t *testing.T) {
	root := t.TempDir()
	_, err := RenderLaunchd(Options{
		Label: "com.codexfold.fs", BinaryPath: filepath.Join(root, "codexfold"),
		CodexHome: filepath.Join(root, "home"), StoreDir: filepath.Join(root, "store"),
		MountPoint: filepath.Join(root, "mount"), StdoutPath: filepath.Join(root, "stdout.log"),
		StderrPath: filepath.Join(root, "stderr.log"), CanonicalNamespace: true,
		NativeRoot: filepath.Join(root, "native"), Frontend: "native-fskit",
		FSKitResource: filepath.Join(root, "resource"),
	})
	if err == nil || !strings.Contains(err.Error(), "launcher") {
		t.Fatalf("native FSKit without launcher error = %v", err)
	}
}

func TestRenderSystemdUsesTheSameServeArgumentsAndRestartPolicy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "path with space % and $")
	options := Options{
		Label: "com.codexfold.fs", BinaryPath: filepath.Join(root, "bin", "codexfold"),
		CodexHome: filepath.Join(root, "codex"), StoreDir: filepath.Join(root, "store"),
		MountPoint: filepath.Join(root, "mount"), StdoutPath: filepath.Join(root, "logs", "stdout.log"),
		StderrPath: filepath.Join(root, "logs", "stderr.log"), CanonicalNamespace: true,
		NativeRoot: filepath.Join(root, "native"), OperationTrace: filepath.Join(root, "logs", "operations.log"),
		EnrollmentInterval: 5 * time.Minute, EnrollmentStableFor: time.Hour, EnrollmentBatchSize: 2,
		EnrollmentCanary: true,
	}
	definition, err := RenderSystemd(options)
	if err != nil {
		t.Fatalf("RenderSystemd: %v", err)
	}
	text := string(definition)
	for _, required := range []string{
		"[Service]", "Type=simple", "Restart=on-failure", "RestartSec=2s", "TimeoutStopSec=30s",
		"--canonical-namespace", "--native-root", "--operation-trace",
		"--enrollment-interval", "5m0s", "--enrollment-canary",
		"ExecStart=:\"", "StandardOutput=append:", "StandardError=append:", "\\x20", "%%", "$",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("systemd definition missing %q:\n%s", required, text)
		}
	}
	if strings.Contains(text, "session_meta") || strings.Contains(text, "rollout") {
		t.Fatalf("definition contains session content: %s", text)
	}
}

func TestRenderWindowsConfigUsesTheSameServeArguments(t *testing.T) {
	root := t.TempDir()
	definition, err := RenderWindowsConfig(Options{
		Label: "com.codexfold.fs", BinaryPath: filepath.Join(root, "codexfold.exe"),
		CodexHome: filepath.Join(root, "codex"), StoreDir: filepath.Join(root, "store"),
		MountPoint: filepath.Join(root, "mount"), StdoutPath: filepath.Join(root, "logs", "stdout.log"),
		StderrPath: filepath.Join(root, "logs", "stderr.log"), CanonicalNamespace: true,
		NativeRoot: filepath.Join(root, "native"),
	})
	if err != nil {
		t.Fatalf("RenderWindowsConfig: %v", err)
	}
	config, err := ParseWindowsConfig(definition)
	if err != nil {
		t.Fatalf("ParseWindowsConfig: %v", err)
	}
	if config.Version != 1 || config.ServiceName != "com.codexfold.fs" {
		t.Fatalf("unexpected Windows config: %#v", config)
	}
	joined := strings.Join(config.Arguments, " ")
	for _, required := range []string{"fs serve --apply --foreground=true", "--canonical-namespace", "--native-root"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("Windows config missing %q: %s", required, joined)
		}
	}
}

func TestManagerUsesOnlyPerUserLaunchctlAndSeparatesDaemonFromMount(t *testing.T) {
	root := t.TempDir()
	runner := &recordingRunner{outputs: map[string][]byte{"launchctl print gui/501/com.codexfold.fs": []byte("state = running\npid = 123\n")}}
	manager := Manager{UID: 501, Runner: runner, MountProbe: func(string) error { return errors.New("mount unavailable") }}
	plist := filepath.Join(root, "com.codexfold.fs.plist")
	if err := os.WriteFile(plist, []byte("plist"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Bootstrap(context.Background(), plist); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := manager.Kickstart(context.Background(), "com.codexfold.fs"); err != nil {
		t.Fatalf("Kickstart: %v", err)
	}
	status := manager.Status(context.Background(), "com.codexfold.fs", filepath.Join(root, "mount"))
	if !status.DaemonRunning || status.DaemonPID != 123 || status.MountHealthy {
		t.Fatalf("status did not separate daemon and mount: %#v", status)
	}
	joined := strings.Join(runner.calls, "\n")
	if strings.Contains(joined, "sudo") || !strings.Contains(joined, "launchctl bootstrap gui/501") || !strings.Contains(joined, "launchctl kickstart gui/501/com.codexfold.fs") {
		t.Fatalf("unexpected lifecycle commands:\n%s", joined)
	}
}

func TestSystemdManagerUsesOnlyTheUserManagerAndSeparatesMountHealth(t *testing.T) {
	runner := &recordingRunner{outputs: map[string][]byte{
		"systemctl --user show com.codexfold.fs.service --property=ActiveState --property=SubState --no-pager": []byte("ActiveState=active\nSubState=running\n"),
	}}
	manager := SystemdManager{Runner: runner, MountProbe: func(string) error { return errors.New("mount unavailable") }}
	if err := manager.Start(context.Background(), "com.codexfold.fs.service"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := manager.Stop(context.Background(), "com.codexfold.fs.service"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	status := manager.Status(context.Background(), "com.codexfold.fs.service", filepath.Join(t.TempDir(), "mount"))
	if !status.DaemonRunning || status.MountHealthy {
		t.Fatalf("status did not separate daemon and mount: %#v", status)
	}
	joined := strings.Join(runner.calls, "\n")
	for _, required := range []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable --now com.codexfold.fs.service",
		"systemctl --user stop com.codexfold.fs.service",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing systemd user command %q:\n%s", required, joined)
		}
	}
	if strings.Contains(joined, "sudo") || strings.Contains(joined, "systemctl enable") {
		t.Fatalf("system service command leaked into user manager:\n%s", joined)
	}
}

func TestWindowsManagerInstallsStartsStopsAndReportsSCMState(t *testing.T) {
	installRunner := &recordingRunner{errors: map[string]error{
		"sc.exe query com.codexfold.fs": errors.New("service does not exist"),
	}}
	manager := WindowsManager{Runner: installRunner}
	binary := `C:\Program Files\CodexFold\codexfold.exe`
	definition := `C:\ProgramData\CodexFold\service.json`
	if err := manager.Install(context.Background(), "com.codexfold.fs", binary, definition); err != nil {
		t.Fatalf("Install: %v", err)
	}
	joined := strings.Join(installRunner.calls, "\n")
	for _, required := range []string{
		"sc.exe create com.codexfold.fs",
		`"C:\Program Files\CodexFold\codexfold.exe" fs service run --definition C:\ProgramData\CodexFold\service.json`,
		"start= auto",
		"sc.exe failure com.codexfold.fs",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing Windows service command %q:\n%s", required, joined)
		}
	}

	statusRunner := &recordingRunner{outputs: map[string][]byte{
		"sc.exe queryex com.codexfold.fs": []byte("STATE              : 4  RUNNING\n"),
	}}
	manager = WindowsManager{Runner: statusRunner, MountProbe: func(string) error { return nil }}
	if err := manager.Start(context.Background(), "com.codexfold.fs"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := manager.Stop(context.Background(), "com.codexfold.fs"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	status := manager.Status(context.Background(), "com.codexfold.fs", filepath.Join(t.TempDir(), "mount"))
	if !status.DaemonRunning || !status.MountHealthy {
		t.Fatalf("Windows service status = %#v", status)
	}
}

func TestStatusDoesNotTreatLoadedExitedJobAsRunning(t *testing.T) {
	runner := &recordingRunner{outputs: map[string][]byte{
		"launchctl print gui/501/com.codexfold.fs": []byte("state = exited\nlast exit code = 1\n"),
	}}
	status := (Manager{UID: 501, Runner: runner, MountProbe: func(string) error { return errors.New("not mounted") }}).Status(
		context.Background(), "com.codexfold.fs", filepath.Join(t.TempDir(), "mount"),
	)
	if status.DaemonRunning || status.DaemonError == "" {
		t.Fatalf("loaded exited job was reported as running: %#v", status)
	}
}

func TestWaitHealthyRequiresRunningDaemonAndLiveMount(t *testing.T) {
	runner := &recordingRunner{outputs: map[string][]byte{
		"launchctl print gui/501/com.codexfold.fs": []byte("state = running\npid = 123\n"),
	}}
	probes := 0
	manager := Manager{UID: 501, Runner: runner, MountProbe: func(string) error {
		probes++
		if probes < 3 {
			return errors.New("mount starting")
		}
		return nil
	}}
	status, err := manager.WaitHealthy(context.Background(), "com.codexfold.fs", filepath.Join(t.TempDir(), "mount"), time.Second)
	if err != nil || !status.DaemonRunning || !status.MountHealthy || probes != 3 {
		t.Fatalf("WaitHealthy status=%#v probes=%d err=%v", status, probes, err)
	}
}

func TestEvaluateUpdateQuarantinesUnknownVersionsAndRejectsPreviewAutomation(t *testing.T) {
	unknown := EvaluateUpdate(UpdateInput{Capability: fsctl.StorageEngine, DoctorHealthy: true, Compatibility: compat.Evaluation{Quarantine: true}, NativeFallbackReady: false})
	if unknown.Allowed || !unknown.Quarantine || !unknown.RequiresNativeFallback {
		t.Fatalf("unknown version was not quarantined: %#v", unknown)
	}
	ready := EvaluateUpdate(UpdateInput{Capability: fsctl.StorageEngine, DoctorHealthy: true, Compatibility: compat.Evaluation{Quarantine: true}, NativeFallbackReady: true})
	if ready.Allowed || !ready.Quarantine || ready.RequiresNativeFallback {
		t.Fatalf("quarantine should remain blocked after fallback: %#v", ready)
	}
	automatic := EvaluateUpdate(UpdateInput{Capability: fsctl.FSEnginePreview, DoctorHealthy: true, Compatibility: compat.Evaluation{Approved: true}, Automatic: true})
	if automatic.Allowed {
		t.Fatalf("preview automatic update should be rejected: %#v", automatic)
	}
	manual := EvaluateUpdate(UpdateInput{Capability: fsctl.FSEnginePreview, DoctorHealthy: true, Compatibility: compat.Evaluation{Approved: true}, ExplicitPromotion: true})
	if !manual.Allowed {
		t.Fatalf("explicit preview promotion should pass: %#v", manual)
	}
}

func TestProcessLockAllowsOnlyOneFilesystemHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.lock")
	status, err := InspectProcessLock(path)
	if err != nil || status.Held {
		t.Fatalf("missing process lock status = %#v err=%v", status, err)
	}
	first, err := AcquireProcessLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	status, err = InspectProcessLock(path)
	if err != nil || !status.Held || status.PID != os.Getpid() {
		t.Fatalf("held process lock status = %#v err=%v", status, err)
	}

	if _, err := AcquireProcessLock(path); err == nil {
		t.Fatal("a second filesystem host acquired the same process lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	status, err = InspectProcessLock(path)
	if err != nil || status.Held {
		t.Fatalf("released process lock status = %#v err=%v", status, err)
	}
	second, err := AcquireProcessLock(path)
	if err != nil {
		t.Fatalf("lock was not released after the first host exited: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

type recordingRunner struct {
	calls   []string
	outputs map[string][]byte
	errors  map[string]error
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	if err, ok := r.errors[call]; ok {
		return r.outputs[call], err
	}
	if output, ok := r.outputs[call]; ok {
		return output, nil
	}
	return nil, nil
}

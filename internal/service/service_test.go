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
	})
	if err != nil {
		t.Fatalf("RenderLaunchd: %v", err)
	}
	text := string(definition)
	for _, required := range []string{
		"<string>fs</string>", "<string>serve</string>", "<string>--apply</string>",
		"<string>--canonical-namespace</string>", "<string>--native-root</string>",
		"<string>--operation-trace</string>", filepath.Join(root, "logs", "operations.log"),
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
	if !status.DaemonRunning || status.MountHealthy {
		t.Fatalf("status did not separate daemon and mount: %#v", status)
	}
	joined := strings.Join(runner.calls, "\n")
	if strings.Contains(joined, "sudo") || !strings.Contains(joined, "launchctl bootstrap gui/501") || !strings.Contains(joined, "launchctl kickstart gui/501/com.codexfold.fs") {
		t.Fatalf("unexpected lifecycle commands:\n%s", joined)
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
	first, err := AcquireProcessLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	if _, err := AcquireProcessLock(path); err == nil {
		t.Fatal("a second filesystem host acquired the same process lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
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
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	if output, ok := r.outputs[call]; ok {
		return output, nil
	}
	return nil, nil
}

//go:build linux

package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderSystemdPassesSystemdAnalyzeVerify(t *testing.T) {
	if os.Getenv("CODEXFOLD_RUN_SYSTEMD_USER_TEST") != "1" {
		t.Skip("set CODEXFOLD_RUN_SYSTEMD_USER_TEST=1 to run systemd verification")
	}
	root := filepath.Join(t.TempDir(), "path with space % and $")
	binary := filepath.Join(root, "bin", "codexfold")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	definition, err := RenderSystemd(Options{
		Label: "com.codexfold.fs", BinaryPath: binary,
		CodexHome: filepath.Join(root, "codex"), StoreDir: filepath.Join(root, "store"),
		MountPoint: filepath.Join(root, "mount"), StdoutPath: filepath.Join(root, "logs", "stdout.log"),
		StderrPath: filepath.Join(root, "logs", "stderr.log"), CanonicalNamespace: true,
		NativeRoot: filepath.Join(root, "native"),
	})
	if err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(root, "com.codexfold.fs.service")
	if err := os.WriteFile(unitPath, definition, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("systemd-analyze", "--user", "verify", unitPath).CombinedOutput(); err != nil {
		t.Fatalf("systemd-analyze rejected generated unit: %v\n%s", err, output)
	}
}

func TestRealSystemdUserManagerLifecycle(t *testing.T) {
	if os.Getenv("CODEXFOLD_RUN_SYSTEMD_USER_TEST") != "1" {
		t.Skip("set CODEXFOLD_RUN_SYSTEMD_USER_TEST=1 to run systemd lifecycle")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	unit := fmt.Sprintf("codexfold-validation-%d.service", os.Getpid())
	unitPath := filepath.Join(home, ".config", "systemd", "user", unit)
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o700); err != nil {
		t.Fatal(err)
	}
	definition := []byte("[Unit]\nDescription=CodexFold systemd user validation\n\n[Service]\nType=simple\nExecStart=/usr/bin/sleep infinity\n\n[Install]\nWantedBy=default.target\n")
	if err := os.WriteFile(unitPath, definition, 0o600); err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		_, _ = exec.Command("systemctl", "--user", "disable", "--now", unit).CombinedOutput()
		_ = os.Remove(unitPath)
		_, _ = exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput()
	}
	t.Cleanup(cleanup)

	manager := SystemdManager{MountProbe: func(string) error { return nil }}
	mountPoint := filepath.Join(t.TempDir(), "mount")
	if err := manager.Start(context.Background(), unit); err != nil {
		t.Fatal(err)
	}
	status, err := manager.WaitHealthy(context.Background(), unit, mountPoint, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !status.DaemonRunning || !status.MountHealthy {
		t.Fatalf("unexpected running status: %#v", status)
	}
	if err := manager.Stop(context.Background(), unit); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status = manager.Status(context.Background(), unit, mountPoint)
		if !status.DaemonRunning {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("systemd user unit remained running: %#v", status)
}

func TestLinuxMountProbeRejectsAnOrdinaryDirectory(t *testing.T) {
	err := ProbeMount(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not a CodexFold FUSE mount root") {
		t.Fatalf("ordinary directory probe error = %v", err)
	}
}

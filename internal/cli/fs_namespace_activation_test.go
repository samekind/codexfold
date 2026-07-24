package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWaitForCanonicalNativePassthroughRequiresMatchingFileSize(t *testing.T) {
	root := t.TempDir()
	mount := filepath.Join(root, "mount")
	nativeRoot := filepath.Join(root, "native")
	for _, namespace := range []string{"sessions", "archived_sessions"} {
		if err := os.MkdirAll(filepath.Join(mount, namespace), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(nativeRoot, namespace), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	nativePath := filepath.Join(nativeRoot, "sessions", "rollout.jsonl")
	mountedPath := filepath.Join(mount, "sessions", "rollout.jsonl")
	if err := os.WriteFile(nativePath, []byte("native-content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mountedPath, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitForCanonicalNativePassthrough(context.Background(), mount, nativeRoot, 5*time.Millisecond); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("size mismatch readiness error = %v", err)
	}
	if err := os.WriteFile(mountedPath, []byte("native-content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitForCanonicalNativePassthrough(context.Background(), mount, nativeRoot, time.Second); err != nil {
		t.Fatalf("matching passthrough did not become ready: %v", err)
	}
}

func TestFSNamespaceActivationRollsBackWhenPassthroughDoesNotBecomeReady(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mount := filepath.Join(root, "mount")
	nativeRoot := filepath.Join(home, "fold-native")
	rollout := filepath.Join(home, "sessions", "rollout.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "archived_sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollout, []byte("{\"safe\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeStateFixture(t, home, rollout)
	for _, namespace := range []string{"sessions", "archived_sessions"} {
		if err := os.MkdirAll(filepath.Join(mount, namespace), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	previousMountProbe := mountHealthProbe
	mountHealthProbe = func(string) error { return nil }
	t.Cleanup(func() { mountHealthProbe = previousMountProbe })
	previousReadiness := waitForCanonicalNamespaceActivation
	waitForCanonicalNamespaceActivation = func(context.Context, string, string, time.Duration) error {
		return errors.New("mounted tree incomplete")
	}
	t.Cleanup(func() { waitForCanonicalNamespaceActivation = previousReadiness })

	command := NewRootCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{
		"fs", "namespace", "activate", "--apply",
		"--codex-home", home, "--mount", mount, "--native-root", nativeRoot,
	})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "mounted tree incomplete") {
		t.Fatalf("activation readiness error = %v", err)
	}
	for _, namespace := range []string{"sessions", "archived_sessions"} {
		path := filepath.Join(home, namespace)
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("rollback namespace %s: info=%v err=%v", namespace, info, statErr)
		}
	}
	if data, readErr := os.ReadFile(rollout); readErr != nil || string(data) != "{\"safe\":true}\n" {
		t.Fatalf("rollback rollout: data=%q err=%v", data, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".codexfold-namespace.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("activation rollback left journal: %v", statErr)
	}
}

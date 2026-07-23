//go:build linux && fuse && fuse3 && cgo

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/service"
)

func TestRealLinuxFSServeRecoversAfterHostSIGKILL(t *testing.T) {
	if os.Getenv("CODEXFOLD_RUN_FUSE3_TEST") != "1" {
		t.Skip("set CODEXFOLD_RUN_FUSE3_TEST=1 to run the real Linux fs serve crash test")
	}
	if !mountfs.Available() {
		t.Fatal("FUSE3 host is unavailable")
	}
	root := t.TempDir()
	home := filepath.Join(root, "codex")
	store := filepath.Join(root, "store")
	mount := filepath.Join(root, "mount")
	native := filepath.Join(root, "native")
	for _, directory := range []string{home, store, native} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = exec.Command("fusermount3", "-uz", mount).Run()
		_ = os.Chmod(mount, 0o500)
	})

	first, firstDone, firstOutput := startLinuxFSServeHelper(t, home, store, mount, native)
	waitForLinuxFSServeMount(t, mount, firstDone, firstOutput)
	firstPID := first.Process.Pid
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-firstDone; err == nil {
		t.Fatal("SIGKILLed fs serve helper exited successfully")
	}

	second, secondDone, secondOutput := startLinuxFSServeHelper(t, home, store, mount, native)
	waitForLinuxFSServeMount(t, mount, secondDone, secondOutput)
	if second.Process.Pid == firstPID {
		t.Fatal("replacement fs serve reused the killed process ID")
	}
	if err := second.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("replacement fs serve shutdown: %v\n%s", err, secondOutput.String())
		}
	case <-time.After(15 * time.Second):
		_ = second.Process.Kill()
		t.Fatal("replacement fs serve did not stop after SIGTERM")
	}
	waitForLinuxFSServeUnmount(t, mount)
	info, err := os.Stat(mount)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o500 {
		t.Fatalf("unmounted fs serve backing mode=%#o", info.Mode().Perm())
	}
}

func TestRealLinuxFSServeCrashHelper(t *testing.T) {
	if os.Getenv("CODEXFOLD_FS_SERVE_CRASH_HELPER") != "1" {
		t.Skip("helper process")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	root := NewRootCommand()
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	root.SetArgs([]string{
		"fs", "serve", "--apply", "--foreground=true",
		"--codex-home", os.Getenv("CODEXFOLD_FS_SERVE_HOME"),
		"--store", os.Getenv("CODEXFOLD_FS_SERVE_STORE"),
		"--mount", os.Getenv("CODEXFOLD_FS_SERVE_MOUNT"),
		"--canonical-namespace", "--native-root", os.Getenv("CODEXFOLD_FS_SERVE_NATIVE"),
	})
	if err := root.ExecuteContext(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func startLinuxFSServeHelper(t *testing.T, home string, store string, mount string, native string) (*exec.Cmd, <-chan error, *lockedBuffer) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestRealLinuxFSServeCrashHelper$", "-test.v")
	command.Env = append(os.Environ(),
		"CODEXFOLD_FS_SERVE_CRASH_HELPER=1",
		"CODEXFOLD_FS_SERVE_HOME="+home,
		"CODEXFOLD_FS_SERVE_STORE="+store,
		"CODEXFOLD_FS_SERVE_MOUNT="+mount,
		"CODEXFOLD_FS_SERVE_NATIVE="+native,
	)
	output := &lockedBuffer{}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() { _ = command.Process.Kill() })
	return command, done, output
}

func waitForLinuxFSServeMount(t *testing.T, mount string, done <-chan error, output *lockedBuffer) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := service.ProbeMount(mount); err == nil {
			return
		} else {
			lastErr = err
		}
		select {
		case err := <-done:
			t.Fatalf("fs serve exited before mount health: %v\n%s", err, output.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("fs serve mount did not become healthy: %v\n%s", lastErr, output.String())
}

func waitForLinuxFSServeUnmount(t *testing.T, mount string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := service.ProbeMount(mount); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("fs serve mount remained healthy after shutdown")
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(value)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

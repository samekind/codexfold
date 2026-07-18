//go:build linux && fuse && fuse3 && cgo

package mountfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestRealFuse3ManagedReadWriteAndRestart(t *testing.T) {
	requireRealFuse3(t)
	root := t.TempDir()
	source := []byte("{\"record\":0}\n")
	managed := mountSessionFixture(t, "linux-managed", source)
	filesystem := New()
	if err := filesystem.AddSession("linux-managed", managed); err != nil {
		t.Fatal(err)
	}
	mountPoint := filepath.Join(root, "mount")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		t.Fatal(err)
	}
	stopMount := startRealFuse3Mount(t, mountPoint, filesystem)
	target := filepath.Join(mountPoint, "linux-managed.jsonl")
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, source) {
		t.Fatalf("initial managed read = %q err=%v", got, err)
	}

	file, err := os.OpenFile(target, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	tail := []byte("{\"record\":1}\n")
	if _, err := file.Write(tail); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), source...), tail...)

	file, err = os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("PATCH"), 2); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Truncate(int64(len(want) - 2)); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	copy(want[2:], []byte("PATCH"))
	want = want[:len(want)-2]
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("mutated managed read = %q err=%v", got, err)
	}
	if managed.State().BackingPath == "" {
		t.Fatal("random write did not enter copy-on-write backing")
	}

	stopMount()
	waitForRealFuse3Unmount(t, mountPoint)
	assertRealFuse3BackingSealed(t, mountPoint)
	stopRemount := startRealFuse3Mount(t, mountPoint, filesystem)
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("remounted managed read = %q err=%v", got, err)
	}
	stopRemount()
	waitForRealFuse3Unmount(t, mountPoint)
	assertRealFuse3BackingSealed(t, mountPoint)
}

func TestRealFuse3CanonicalArchiveUnarchiveRename(t *testing.T) {
	requireRealFuse3(t)
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	activeDirectory := filepath.Join(nativeRoot, "sessions", "2026", "07", "16")
	archivedDirectory := filepath.Join(nativeRoot, "archived_sessions")
	for _, directory := range []string{activeDirectory, archivedDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	source := []byte("{\"canonical\":true}\n")
	managed := mountSessionFixture(t, "linux-canonical", source)
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	filename := "rollout-linux-canonical.jsonl"
	if err := filesystem.AddSessionAt("linux-canonical", "/archived_sessions/"+filename, managed); err != nil {
		t.Fatal(err)
	}
	mountPoint := filepath.Join(root, "mount")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		t.Fatal(err)
	}
	stopMount := startRealFuse3Mount(t, mountPoint, filesystem)
	archivedPath := filepath.Join(mountPoint, "archived_sessions", filename)
	activePath := filepath.Join(mountPoint, "sessions", "2026", "07", "16", filename)
	if err := os.Rename(archivedPath, activePath); err != nil {
		t.Fatalf("unarchive rename: %v", err)
	}
	if got, err := os.ReadFile(activePath); err != nil || !bytes.Equal(got, source) {
		t.Fatalf("active managed read = %q err=%v", got, err)
	}
	if _, err := os.Stat(archivedPath); !os.IsNotExist(err) {
		t.Fatalf("archived route remained after unarchive: %v", err)
	}
	if err := os.Rename(activePath, archivedPath); err != nil {
		t.Fatalf("archive rename: %v", err)
	}
	if got, err := os.ReadFile(archivedPath); err != nil || !bytes.Equal(got, source) {
		t.Fatalf("restored archived read = %q err=%v", got, err)
	}
	nativePath := filepath.Join(nativeRoot, "archived_sessions", filename)
	nativeBytes := []byte("{\"native_fallback\":true}\n")
	if err := os.WriteFile(nativePath, nativeBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := filesystem.PreferNativeSession("linux-canonical"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(archivedPath); err != nil || !bytes.Equal(got, nativeBytes) {
		t.Fatalf("preferred native read = %q err=%v", got, err)
	}
	if err := os.Remove(nativePath); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(archivedPath); err != nil || !bytes.Equal(got, source) {
		t.Fatalf("managed fallback read = %q err=%v", got, err)
	}
	if err := os.WriteFile(nativePath, nativeBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := filesystem.RemoveSession("linux-canonical"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(archivedPath); err != nil || !bytes.Equal(got, nativeBytes) {
		t.Fatalf("native read after managed removal = %q err=%v", got, err)
	}
	stopMount()
	waitForRealFuse3Unmount(t, mountPoint)
	assertRealFuse3BackingSealed(t, mountPoint)
}

func TestRealFuse3HostCrashUnmountsAndRestarts(t *testing.T) {
	requireRealFuse3(t)
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	route := filepath.Join("sessions", "2026", "07", "16", "rollout-crash.jsonl")
	nativePath := filepath.Join(nativeRoot, route)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	source := []byte("{\"crash_recovery\":true}\n")
	if err := os.WriteFile(nativePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	mountPoint := filepath.Join(root, "mount")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		process, done, output := startRealFuse3Helper(t, mountPoint, nativeRoot)
		waitForRealFuse3ProcessMount(t, mountPoint, done, output)
		mountedPath := filepath.Join(mountPoint, route)
		if got, err := os.ReadFile(mountedPath); err != nil || !bytes.Equal(got, source) {
			t.Fatalf("attempt %d mounted read = %q err=%v", attempt, got, err)
		}
		if err := process.Kill(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err == nil {
			t.Fatalf("attempt %d killed helper exited successfully", attempt)
		}
		if attempt == 2 {
			if err := recoverStaleMount(mountPoint); err != nil {
				t.Fatal(err)
			}
			waitForRealFuse3Unmount(t, mountPoint)
			assertRealFuse3BackingSealed(t, mountPoint)
		}
	}
}

func TestRealFuse3ReadAndFsyncPerformance(t *testing.T) {
	requireRealFuse3(t)
	root := t.TempDir()
	line := []byte("{\"payload\":\"0123456789abcdef0123456789abcdef0123456789abcdef\"}\n")
	source := bytes.Repeat(line, (16<<20)/len(line)+1)
	source = source[:16<<20]
	managed := mountSessionFixture(t, "linux-performance", source)
	filesystem := New()
	if err := filesystem.AddSession("linux-performance", managed); err != nil {
		t.Fatal(err)
	}
	mountPoint := filepath.Join(root, "mount")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		t.Fatal(err)
	}
	stopMount := startRealFuse3Mount(t, mountPoint, filesystem)
	target := filepath.Join(mountPoint, "linux-performance.jsonl")

	readStart := time.Now()
	file, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	readBytes, copyErr := io.Copy(io.Discard, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || readBytes != int64(len(source)) {
		t.Fatalf("mounted performance read bytes=%d copy=%v close=%v", readBytes, copyErr, closeErr)
	}
	readDuration := time.Since(readStart)
	readMiBPerSecond := float64(readBytes) / (1024 * 1024) / readDuration.Seconds()
	if readMiBPerSecond < 25 {
		t.Fatalf("mounted read throughput %.2f MiB/s is below the 25 MiB/s safety floor", readMiBPerSecond)
	}

	file, err = os.OpenFile(target, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	durations := make([]time.Duration, 0, 50)
	for index := range 50 {
		started := time.Now()
		if _, err := fmt.Fprintf(file, "{\"append\":%d}\n", index); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		durations = append(durations, time.Since(started))
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95 := durations[(len(durations)*95+99)/100-1]
	if p95 > 250*time.Millisecond {
		t.Fatalf("mounted append+fsync p95 %s exceeds the 250ms safety ceiling", p95)
	}
	t.Logf("FUSE3 read=%.2f MiB/s append_fsync_p95=%s", readMiBPerSecond, p95)
	stopMount()
	waitForRealFuse3Unmount(t, mountPoint)
	assertRealFuse3BackingSealed(t, mountPoint)
}

func TestRealFuse3CrashHelper(t *testing.T) {
	if os.Getenv("CODEXFOLD_FUSE3_CRASH_HELPER") != "1" {
		t.Skip("helper process")
	}
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(os.Getenv("CODEXFOLD_FUSE3_NATIVE_ROOT"))
	if err := Mount(context.Background(), HostOptions{
		MountPoint: os.Getenv("CODEXFOLD_FUSE3_MOUNT_POINT"), Filesystem: filesystem, Foreground: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func requireRealFuse3(t *testing.T) {
	t.Helper()
	if os.Getenv("CODEXFOLD_RUN_FUSE3_TEST") != "1" {
		t.Skip("set CODEXFOLD_RUN_FUSE3_TEST=1 to run the real Linux FUSE3 adapter test")
	}
}

func startRealFuse3Mount(t *testing.T, mountPoint string, filesystem *Filesystem) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	mountDone := make(chan error, 1)
	go func() {
		mountDone <- Mount(ctx, HostOptions{MountPoint: mountPoint, Filesystem: filesystem, Foreground: true})
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-mountDone:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("FUSE3 mount shutdown: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("FUSE3 mount did not stop after cancellation")
			}
		})
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(20 * time.Second)
	var lastProbeErr error
	for time.Now().Before(deadline) {
		if err := probeRealFuse3Mount(mountPoint); err == nil {
			return stop
		} else {
			lastProbeErr = err
		}
		select {
		case err := <-mountDone:
			t.Fatalf("FUSE3 mount exited before health: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("FUSE3 mount did not become healthy: %v", lastProbeErr)
	return stop
}

func waitForRealFuse3Unmount(t *testing.T, mountPoint string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !linuxFuseMountVisible(mountPoint) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("FUSE3 mount remained active after shutdown")
}

func probeRealFuse3Mount(mountPoint string) error {
	identity, err := os.ReadFile(filepath.Join(mountPoint, ".codexfold-health"))
	if err != nil {
		return err
	}
	if len(identity) < 16 {
		return fmt.Errorf("mount identity is too short: %d", len(identity))
	}
	return nil
}

func assertRealFuse3BackingSealed(t *testing.T, mountPoint string) {
	t.Helper()
	info, err := os.Stat(mountPoint)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o200 != 0 {
		t.Fatalf("unmounted FUSE3 backing remained writable: mode=%#o", info.Mode().Perm())
	}
}

func startRealFuse3Helper(t *testing.T, mountPoint string, nativeRoot string) (*os.Process, <-chan error, *bytes.Buffer) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestRealFuse3CrashHelper$", "-test.v")
	command.Env = append(os.Environ(),
		"CODEXFOLD_FUSE3_CRASH_HELPER=1",
		"CODEXFOLD_FUSE3_MOUNT_POINT="+mountPoint,
		"CODEXFOLD_FUSE3_NATIVE_ROOT="+nativeRoot,
	)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	return command.Process, done, &output
}

func waitForRealFuse3ProcessMount(t *testing.T, mountPoint string, done <-chan error, output *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastProbeErr error
	for time.Now().Before(deadline) {
		if err := probeRealFuse3Mount(mountPoint); err == nil {
			return
		} else {
			lastProbeErr = err
		}
		select {
		case err := <-done:
			t.Fatalf("FUSE3 helper exited before health: %v output=%s", err, output.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("FUSE3 helper did not become healthy: %v output=%s", lastProbeErr, output.String())
}

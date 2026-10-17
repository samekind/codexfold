//go:build darwin

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/enroll"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/vfs"
)

// Runs the actual signed daemon, including index reload and background fold,
// with isolated data and the installed FSKit module. No Codex client is started.
func TestNamespaceReplacementSignedDaemonReload(t *testing.T) {
	testNamespaceReplacementSignedDaemonReload(t, false)
}

func TestSignedDaemonDeletionCleanupDoesNotBlockReload(t *testing.T) {
	testNamespaceReplacementSignedDaemonReload(t, true)
}

func TestSignedDaemonActualFullVolumeReadAndWriteRecovery(t *testing.T) {
	volume := os.Getenv("CODEXFOLD_TEST_FULL_VOLUME")
	if !strings.HasPrefix(volume, "/private/tmp/codexfold-pressure-") {
		t.Skip("explicit disposable full-volume fixture required")
	}
	t.Setenv("TMPDIR", volume)
	testNamespaceReplacementSignedDaemonReload(t, false, true)
}

func testNamespaceReplacementSignedDaemonReload(t *testing.T, pendingDeletion bool, fullVolume ...bool) {
	candidate := os.Getenv("CODEXFOLD_NAMESPACE_CANDIDATE")
	if candidate == "" {
		t.Skip("explicit signed candidate required")
	}
	filesystem, store, native, want := namespaceReplacementFixture(t)
	filesystem.CloseSessions()
	var foreign string
	if pendingDeletion {
		foreign = signedDaemonPendingDeletionFixture(t, store)
	}
	home := t.TempDir()
	user, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	resource, err := os.MkdirTemp(filepath.Join(user, "Library", "Group Containers", "group.vip.jstar.codexfold"), "r-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(resource)
	mount, err := os.MkdirTemp("/private/tmp", "cfd-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(mount)
	route := filepath.Join(mount, "sessions", "rollout.jsonl")
	writeStateFixture(t, home, route)
	if out, err := exec.Command("/usr/bin/sqlite3", filepath.Join(home, "state_5.sqlite"), "UPDATE threads SET id='logical';").CombinedOutput(); err != nil {
		t.Fatalf("fixture index: %v %s", err, out)
	}
	// Disable ordinary enrollment so this test isolates replacement folding.
	if err := enroll.SaveControl(enroll.ControlPath(store), enroll.Control{Enabled: false, Interval: time.Minute, StableFor: time.Hour}); err != nil {
		t.Fatal(err)
	}
	var daemonLog bytes.Buffer
	var cmd *exec.Cmd
	var done chan error
	start := func() {
		cmd = exec.Command(candidate, "fs", "serve", "--apply", "--canonical-namespace", "--frontend", "native-fskit", "--codex-home", home, "--store", store, "--mount", mount, "--native-root", native, "--fskit-resource", resource)
		cmd.Stdout, cmd.Stderr = &daemonLog, &daemonLog
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done = make(chan error, 1)
		go func(c *exec.Cmd, d chan error) { d <- c.Wait() }(cmd, done)
	}
	stop := func() {
		if cmd == nil {
			return
		}
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case err := <-done:
			if err != nil {
				t.Logf("isolated daemon exit: %v", err)
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("isolated daemon failed to stop")
		}
		cmd = nil
	}
	defer stop()
	start()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(resource, "daemon.sock")); err == nil {
			break
		}
		select {
		case err := <-done:
			cmd = nil
			t.Fatalf("isolated daemon exited: %v\n%s", err, daemonLog.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated daemon startup timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "/sbin/mount", "-F", "-t", "codexfoldnative", resource, mount).CombinedOutput(); err != nil {
		t.Fatalf("isolated mount: %v %s", err, out)
	}
	mounted := true
	unmount := func() {
		if !mounted {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "/sbin/umount", mount).CombinedOutput(); err != nil {
			t.Errorf("isolated unmount: %v %s", err, out)
		}
		mounted = false
	}
	defer unmount()
	check := func() {
		got, err := os.ReadFile(route)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("signed daemon current bytes: %v %q", err, got)
		}
		got, err = os.ReadFile(route + ".bak")
		if err != nil || !bytes.Contains(got, []byte("original")) {
			t.Fatalf("signed daemon backup: %v %q", err, got)
		}
	}
	check()
	if len(fullVolume) != 0 && fullVolume[0] {
		filler := filepath.Join(os.Getenv("CODEXFOLD_TEST_FULL_VOLUME"), "test-only-mount-filler")
		f, err := os.Create(filler)
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(filler)
		block := bytes.Repeat([]byte("x"), 1<<20)
		var fillErr error
		for i := 0; i < 300; i++ {
			if _, fillErr = f.Write(block); fillErr != nil {
				break
			}
		}
		_ = f.Close()
		if !errors.Is(fillErr, syscall.ENOSPC) {
			t.Fatalf("did not fill disposable volume: %v", fillErr)
		}
		check() // Native replacement and folded .bak through the real FSKit mount.
		writer, err := os.OpenFile(route, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		payload := append(append([]byte("{\"full_disk\":\""), block...), []byte("\"}\n")...)
		_, writeErr := writer.Write(payload)
		syncErr := writer.Sync()
		closeErr := writer.Close()
		if !errors.Is(errors.Join(writeErr, syncErr, closeErr), syscall.ENOSPC) {
			t.Fatalf("full-volume write was not rejected: write=%v sync=%v close=%v", writeErr, syncErr, closeErr)
		}
		got, err := os.ReadFile(filepath.Join(native, "sessions", "rollout.jsonl"))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("failed write changed durable original: bytes=%d err=%v", len(got), err)
		}
		if err := os.Remove(filler); err != nil {
			t.Fatal(err)
		}
		writer, err = os.OpenFile(route, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		appendix := []byte("{\"recovered\":true}\n")
		if _, err := writer.Write(appendix); err != nil {
			t.Fatal(err)
		}
		if err := writer.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		got, err = os.ReadFile(route)
		if err != nil || !bytes.Equal(got, append(append([]byte{}, want...), appendix...)) {
			t.Fatalf("mounted retry after freeing space differs: bytes=%d err=%v", len(got), err)
		}
		t.Log("actual full APFS volume: native/folded reads, ENOSPC without durable-byte changes, and mounted retry passed")
		return
	}
	// Let the real reload loop poll the original logical ID before cutover.
	time.Sleep(1200 * time.Millisecond)
	check()
	if err := enroll.SaveControl(enroll.ControlPath(store), enroll.Control{Enabled: true, Interval: time.Hour, StableFor: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(25 * time.Second)
	for {
		_, err := os.Stat(filepath.Join(native, "sessions", "rollout.jsonl"))
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("signed daemon did not finish replacement folding")
		}
		time.Sleep(100 * time.Millisecond)
	}
	check()
	if err := enroll.SaveControl(enroll.ControlPath(store), enroll.Control{Enabled: false, Interval: time.Minute, StableFor: time.Hour}); err != nil {
		t.Fatal(err)
	}
	stop()
	start()
	// The mounted frontend reconnects to the new daemon without unmounting.
	deadline = time.Now().Add(20 * time.Second)
	for {
		got, err := os.ReadFile(route)
		if err == nil && bytes.Equal(got, want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("signed daemon restart did not recover current: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	check()
	if pendingDeletion {
		if _, err := os.Stat(foreign); err != nil {
			t.Fatalf("unproved deletion object was removed: %v", err)
		}
	}
	unmount()
	stop()
	for _, failure := range []string{"replaced file cleanup:", "reload deferred", "enrollment cycle failed"} {
		if strings.Contains(daemonLog.String(), failure) {
			t.Fatalf("signed daemon diagnostics:\n%s", daemonLog.String())
		}
	}
	if pendingDeletion && !strings.Contains(daemonLog.String(), "unrelated sessions continue to refresh") {
		t.Fatalf("missing separate cleanup diagnostic: %s", daemonLog.String())
	}
	t.Logf("signed daemon reload and mounted reconnect passed; %s", daemonLog.String())
}

func signedDaemonPendingDeletionFixture(t *testing.T, store string) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "deleted.jsonl")
	if err := os.WriteFile(source, []byte("{\"deleted-fixture\":true}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := fold.Fold(context.Background(), fold.Session{ID: "deleted", RolloutPath: source}, fold.FoldOptions{StoreDir: store, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := fold.LoadManifest(store, "deleted")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := retainCanonicalSnapshot(context.Background(), store, "deleted", vfs.NativeFile{Path: source, Bytes: result.SourceBytes, SHA256: result.SourceSHA256}, nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{Root: store, ManifestPath: fold.ManifestPath(store, "deleted"), Manifest: manifest, Reader: mountfs.NamespaceReader(store, nil), NativeSnapshot: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	tombstone, err := vfs.PublishSessionDeletion(store, session.State(), "/sessions/deleted.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(tombstone.RetiredSessionPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tombstone.SessionPath, tombstone.RetiredSessionPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tombstone.ManifestPath, tombstone.RetiredManifestPath); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(tombstone.RetiredSessionPath, "foreign-do-not-delete")
	if err := os.WriteFile(foreign, []byte("unproved material"), 0600); err != nil {
		t.Fatal(err)
	}
	return foreign
}

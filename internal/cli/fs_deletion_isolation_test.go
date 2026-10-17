package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/vfs"
)

func TestManagedDeletionCleanupFailureDoesNotBlockOtherSessions(t *testing.T) {
	store, native := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(native, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"deleted.jsonl", "other.jsonl"} {
		if err := os.WriteFile(filepath.Join(native, "sessions", name), []byte("kept\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	filesystem := mountfs.NewCanonical()
	filesystem.SetNativeRoot(native)
	known := map[string]uint64{"deleted": 1, "other": 2}
	routes := map[string]string{"deleted": "/sessions/deleted.jsonl", "other": "/sessions/other.jsonl"}
	packs := map[string]string{"deleted": "gen", "other": "gen"}
	// Invalid physical-cleanup authority is deliberately not repaired or trusted.
	// Its durable hide barrier still must not block an unrelated valid route.
	deleted, detachErr, cleanupErr := reconcileManagedSessionDeletionSet(store, filesystem, true, []vfs.SessionDeletion{{SessionID: "deleted", Route: "/sessions/deleted.jsonl"}}, known, routes, packs)
	if detachErr != nil || cleanupErr == nil {
		t.Fatalf("detach=%v cleanup=%v", detachErr, cleanupErr)
	}
	if _, ok := deleted["deleted"]; !ok || known["other"] != 2 || routes["other"] == "" || packs["other"] != "gen" {
		t.Fatal("unrelated state was lost")
	}
	if _, errno := filesystem.Getattr("/sessions/deleted.jsonl"); errno != syscall.ENOENT {
		t.Fatalf("deleted source revived: %v", errno)
	}
	h, errno := filesystem.Open("/sessions/other.jsonl", os.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer filesystem.Release(h)
	b := make([]byte, 5)
	if n, errno := filesystem.Read(h, b, 0); errno != 0 || n != 5 || string(b) != "kept\n" {
		t.Fatalf("other read n=%d errno=%v bytes=%q", n, errno, b)
	}
	observation := managedReloadObservation{CleanupError: cleanupErr, ManagedSessions: 1}
	if observation.Err() != nil {
		t.Fatal("cleanup error still aborts reload")
	}
	snapshot := newManagedStatusTracker().snapshot(observation, time.Now())
	if snapshot.State != "healthy" || !strings.Contains(snapshot.Detail, "cleanup") {
		t.Fatalf("cleanup not separately reported: %+v", snapshot)
	}
	if _, err := os.Stat(filepath.Join(native, "sessions", "deleted.jsonl")); err != nil {
		t.Fatalf("uncertain material was removed: %v", err)
	}
}

func TestManagedDeletionHideFailureRemainsFatal(t *testing.T) {
	_, detachErr, _ := reconcileManagedSessionDeletionSet(t.TempDir(), mountfs.NewCanonical(), true, []vfs.SessionDeletion{{SessionID: "bad/id", Route: "/sessions/deleted.jsonl"}}, map[string]uint64{}, map[string]string{}, map[string]string{})
	if detachErr == nil || errors.Is(detachErr, os.ErrNotExist) {
		t.Fatalf("unsafe hide failure ignored: %v", detachErr)
	}
}

package storage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOperationLockExcludesSecondMutation(t *testing.T) {
	store := t.TempDir()
	first, err := AcquireOperationLock(store, "objects")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireOperationLock(store, "objects"); err == nil || !strings.Contains(err.Error(), "already held") {
		t.Fatalf("second lock error = %v, want held rejection", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireOperationLock(store, "objects")
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOperationLockWithinTimesOutAndRecovers(t *testing.T) {
	store := t.TempDir()
	first, err := AcquireOperationLock(store, "checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := AcquireOperationLockWithin(store, "checkpoint", 25*time.Millisecond); !errors.Is(err, ErrOperationLockHeld) {
		t.Fatalf("timed lock error = %v, want held", err)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond || elapsed > time.Second {
		t.Fatalf("timed lock waited %s", elapsed)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireOperationLockWithin(store, "checkpoint", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOperationLockCreatesMissingStoreRoot(t *testing.T) {
	store := filepath.Join(t.TempDir(), "new-store")
	lock, err := AcquireOperationLock(store, "objects")
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(store)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("operation lock store root info=%v err=%v", info, err)
	}
}

func TestOperationLockRejectsSymlinkedLockDirectory(t *testing.T) {
	store := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(store, "locks")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := AcquireOperationLock(store, "session-deletions"); err == nil {
		t.Fatal("operation lock accepted a symlinked lock directory")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("operation lock wrote outside the store: %#v", entries)
	}
}

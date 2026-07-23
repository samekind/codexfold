package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLeaseReportsActiveUntilClosed(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "leases")
	lease, err := AcquireLease(directory, "generation")
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	active, err := DirectoryHasActiveLease(directory, true)
	if err != nil {
		t.Fatalf("DirectoryHasActiveLease: %v", err)
	}
	if !active {
		t.Fatal("held lease was not reported active")
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("close lease: %v", err)
	}
	active, err = DirectoryHasActiveLease(directory, true)
	if err != nil {
		t.Fatalf("DirectoryHasActiveLease after close: %v", err)
	}
	if active {
		t.Fatal("closed lease remained active")
	}
}

func TestAcquireLeaseDoesNotCreateMissingAncestorDirectories(t *testing.T) {
	root := t.TempDir()
	missingParent := filepath.Join(root, "missing-generation")
	if _, err := AcquireLease(filepath.Join(missingParent, "leases"), "reader"); err == nil {
		t.Fatal("lease unexpectedly created a missing generation tree")
	}
	if _, err := os.Lstat(missingParent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing generation was recreated: %v", err)
	}
}

func TestDirectoryHasActiveLeaseCleansUnlockedStaleFiles(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "leases")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(directory, ".lease-stale")
	if err := os.WriteFile(stale, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	active, err := DirectoryHasActiveLease(directory, true)
	if err != nil {
		t.Fatalf("DirectoryHasActiveLease: %v", err)
	}
	if active {
		t.Fatal("unlocked stale lease was reported active")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale lease remains: %v", err)
	}
}

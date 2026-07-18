package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestGuardScansCurrentFootprintAndChecksLiveFreeSpace(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSizedFile(t, filepath.Join(store, "metadata.bin"), 32)
	var probed string
	guard := Guard{
		StoreDir: store,
		Limits:   Limits{FreeSpaceReserveBytes: 95},
		Probe: func(path string) (int64, error) {
			probed = path
			return 100, nil
		},
	}
	assessment, err := guard.Check(context.Background(), Projection{Operation: "materialize", AdditionalPersistentBytes: 6})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("Guard.Check error = %v, want ErrBudgetExceeded", err)
	}
	if probed != filepath.Clean(store) {
		t.Fatalf("space probe path = %q, want %q", probed, filepath.Clean(store))
	}
	if assessment.Inventory.TotalPhysicalBytes == 0 || assessment.Budget.CurrentPhysicalBytes != assessment.Inventory.TotalPhysicalBytes {
		t.Fatalf("assessment did not use scanned physical bytes: %#v", assessment)
	}
}

func TestGuardPropagatesSpaceProbeFailure(t *testing.T) {
	store := t.TempDir()
	want := errors.New("probe failed")
	guard := Guard{StoreDir: store, Probe: func(string) (int64, error) { return 0, want }}
	if _, err := guard.Check(context.Background(), Projection{Operation: "pack"}); !errors.Is(err, want) {
		t.Fatalf("Guard.Check error = %v, want %v", err, want)
	}
}

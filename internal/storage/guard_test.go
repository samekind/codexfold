package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestOnceInventoryGuardReusesPlanningSnapshotAndChecksLiveSpace(t *testing.T) {
	store := t.TempDir()
	writeSizedFile(t, filepath.Join(store, "first.bin"), 32)
	probes := 0
	guard := Guard{StoreDir: store, Probe: func(string) (int64, error) {
		probes++
		return int64(1000 - probes*100), nil
	}}
	planning := &OnceInventoryGuard{Guard: guard}
	first, err := planning.Check(context.Background(), Projection{Operation: "first"})
	if err != nil {
		t.Fatal(err)
	}
	writeSizedFile(t, filepath.Join(store, "second.bin"), 4096)
	second, err := planning.Check(context.Background(), Projection{Operation: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Inventory.TotalFiles != first.Inventory.TotalFiles || second.Budget.CurrentPhysicalBytes != first.Budget.CurrentPhysicalBytes {
		t.Fatalf("planning inventory changed within one batch: first=%#v second=%#v", first.Inventory, second.Inventory)
	}
	if second.Budget.AvailableBytes != 800 || probes != 2 {
		t.Fatalf("free-space probe was not refreshed: available=%d probes=%d", second.Budget.AvailableBytes, probes)
	}
	fresh, err := guard.Check(context.Background(), Projection{Operation: "apply"})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Inventory.TotalFiles <= second.Inventory.TotalFiles || probes != 3 {
		t.Fatalf("fresh mutation guard reused stale inventory: fresh=%d planned=%d probes=%d", fresh.Inventory.TotalFiles, second.Inventory.TotalFiles, probes)
	}
}

func TestProductionPlanningBudgetReadOnlyScale(t *testing.T) {
	store := os.Getenv("CODEXFOLD_READONLY_PLANNING_STORE")
	if store == "" {
		t.Skip("requires an explicitly selected read-only production-scale store")
	}
	guard, err := DefaultGuard(store)
	if err != nil {
		t.Fatal(err)
	}
	planning := &OnceInventoryGuard{Guard: guard}
	started := time.Now()
	first, err := planning.Check(context.Background(), Projection{Operation: "planning-first"})
	if err != nil {
		t.Fatal(err)
	}
	firstElapsed := time.Since(started)
	for index := range 200 {
		current, err := planning.Check(context.Background(), Projection{Operation: "planning-repeat"})
		if err != nil {
			t.Fatalf("budget check %d: %v", index, err)
		}
		if current.Inventory.TotalFiles != first.Inventory.TotalFiles || current.Budget.CurrentPhysicalBytes != first.Budget.CurrentPhysicalBytes {
			t.Fatal("planning budget did not retain its one-cycle inventory")
		}
	}
	t.Logf("planning budget first_inventory=%s repeat_200=%s scanned_files=%d", firstElapsed, time.Since(started)-firstElapsed, first.Inventory.TotalFiles)
}

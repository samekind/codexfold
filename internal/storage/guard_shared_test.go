package storage

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sharedTestGuard(t *testing.T) (SharedInventoryGuard, string) {
	t.Helper()
	store := t.TempDir()
	guard := Guard{StoreDir: store, Limits: DefaultLimits, Probe: func(string) (int64, error) { return 1 << 40, nil }}
	return SharedInventoryGuard{Guard: guard, MaxAge: time.Hour}, store
}

func stubScan(t *testing.T, fn func(context.Context, Options) (Inventory, error)) {
	t.Helper()
	previous := scanInventory
	scanInventory = fn
	t.Cleanup(func() { scanInventory = previous })
}

func TestSharedInventoryGuardScansOnceWithinMaxAge(t *testing.T) {
	guard, store := sharedTestGuard(t)
	var scans atomic.Int32
	stubScan(t, func(_ context.Context, options Options) (Inventory, error) {
		scans.Add(1)
		return Inventory{StoreDir: options.StoreDir}, nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := guard.Check(context.Background(), Projection{Operation: "copy-on-write"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := scans.Load(); got != 1 {
		t.Fatalf("scans = %d, want 1 for %s", got, store)
	}
}

func TestSharedInventoryGuardStaleInventoryDoesNotBlockForeground(t *testing.T) {
	guard, _ := sharedTestGuard(t)
	guard.MaxAge = time.Nanosecond
	release := make(chan struct{})
	var scans atomic.Int32
	stubScan(t, func(_ context.Context, options Options) (Inventory, error) {
		if scans.Add(1) > 1 {
			<-release
		}
		return Inventory{StoreDir: options.StoreDir}, nil
	})
	if _, err := guard.Check(context.Background(), Projection{Operation: "copy-on-write"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	done := make(chan error, 1)
	go func() {
		_, err := guard.Check(context.Background(), Projection{Operation: "copy-on-write"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("check blocked behind a background refresh of a stale inventory")
	}
	close(release)
}

func TestSharedInventoryGuardFirstScanErrorPropagatesAndRetries(t *testing.T) {
	guard, _ := sharedTestGuard(t)
	boom := errors.New("scan failed")
	var scans atomic.Int32
	stubScan(t, func(_ context.Context, options Options) (Inventory, error) {
		if scans.Add(1) == 1 {
			return Inventory{}, boom
		}
		return Inventory{StoreDir: options.StoreDir}, nil
	})
	if _, err := guard.Check(context.Background(), Projection{Operation: "copy-on-write"}); !errors.Is(err, boom) {
		t.Fatalf("first check error = %v, want %v", err, boom)
	}
	if _, err := guard.Check(context.Background(), Projection{Operation: "copy-on-write"}); err != nil {
		t.Fatalf("second check error = %v", err)
	}
}

func TestSharedInventoryGuardStillEnforcesFreeSpace(t *testing.T) {
	guard, _ := sharedTestGuard(t)
	guard.Guard.Probe = func(string) (int64, error) { return 1, nil }
	stubScan(t, func(_ context.Context, options Options) (Inventory, error) {
		return Inventory{StoreDir: options.StoreDir}, nil
	})
	_, err := guard.Check(context.Background(), Projection{Operation: "copy-on-write", AdditionalPersistentBytes: 1 << 20, TemporaryBytes: 1 << 20, TemporaryPersistentOverlapBytes: 1 << 20})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("error = %v, want ErrBudgetExceeded", err)
	}
}

func TestSharedInventoryGuardCancelledWaiterReturns(t *testing.T) {
	guard, _ := sharedTestGuard(t)
	release := make(chan struct{})
	stubScan(t, func(_ context.Context, options Options) (Inventory, error) {
		<-release
		return Inventory{StoreDir: options.StoreDir}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := guard.Check(ctx, Projection{Operation: "copy-on-write"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	close(release)
}

package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMaintenanceYieldsOnlyToLiveForegroundRequest(t *testing.T) {
	store := t.TempDir()
	requested, err := RequestForegroundAccess(store)
	if err != nil || requested {
		t.Fatalf("no maintenance: requested=%t err=%v", requested, err)
	}
	m, err := BeginMaintenance(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if requested, err := RequestForegroundAccess(store); err != nil || !requested {
		t.Fatalf("live request: requested=%t err=%v", requested, err)
	}
	select {
	case <-m.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("maintenance ignored foreground request")
	}
	if !errors.Is(m.YieldError(m.Context().Err()), ErrManagedSessionBusy) {
		t.Fatal("foreground yielding was not classified as ordinary contention")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if requested, err := RequestForegroundAccess(store); err != nil || requested {
		t.Fatalf("closed maintenance remained active: %t %v", requested, err)
	}
	stale := filepath.Join(maintenanceDirectory(store), ".lease-maintenance-123-stale")
	if err := os.WriteFile(stale, []byte("123\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if requested, err := RequestForegroundAccess(store); err != nil || requested {
		t.Fatalf("stale maintenance authorized a retry: %t %v", requested, err)
	}
}

func TestMaintenanceCancellationDoesNotReleaseDeletionLocks(t *testing.T) {
	store := t.TempDir()
	manifest := filepath.Join(store, "manifests", "session.json")
	writeJSONFile(t, manifest, manifestFixture("session", filepath.Join(store, "native.jsonl"), 4))
	directory := filepath.Join(store, "fs", "sessions", "session")
	delta := writeSizedFile(t, filepath.Join(directory, "delta.jsonl"), 0)
	writeJSONFile(t, filepath.Join(directory, "state.json"), stateFixture(t, "session", manifest, 4, delta, "", ""))
	guard, err := AcquireManagedSessionDeletionGuard(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if requested, err := RequestForegroundAccess(store); err != nil || !requested {
		t.Fatalf("foreground request: %t %v", requested, err)
	}
	select {
	case <-guard.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("deletion context did not yield")
	}
	lease := filepath.Join(directory, "writer.lease")
	if active, err := FileHasActiveLock(lease); err != nil || !active {
		t.Fatalf("request released a deletion lock before its owner unwound: %t %v", active, err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if active, err := FileHasActiveLock(lease); err != nil || active {
		t.Fatalf("lock survived orderly cleanup: %t %v", active, err)
	}
}

type cancelAfterRead struct {
	io.Reader
	cancel context.CancelFunc
}

func (r cancelAfterRead) Read(buffer []byte) (int, error) {
	n, err := r.Reader.Read(buffer)
	r.cancel()
	return n, err
}

func TestCopyContextInterruptsOneLargeFile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := bytes.Repeat([]byte("x"), 1<<20)
	written, err := CopyContext(ctx, io.Discard, cancelAfterRead{Reader: bytes.NewReader(source), cancel: cancel})
	if !errors.Is(err, context.Canceled) || written <= 0 || written > 64<<10 {
		t.Fatalf("large-file proof ignored cancellation: bytes=%d err=%v", written, err)
	}
}

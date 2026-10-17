package mountfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/storage"
)

func TestForegroundOpenPreemptsReclamationAndAppendsExactBytes(t *testing.T) {
	filesystem, source := mountFixture(t)
	state := filesystem.sessions["session"].State()
	store := filepath.Dir(filepath.Dir(state.ManifestPath))
	guard, err := storage.AcquireManagedSessionDeletionGuard(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	background := make(chan error, 1)
	go func() {
		<-guard.Context().Done()
		err := guard.YieldError(guard.Context().Err())
		background <- errors.Join(err, guard.Close())
	}()
	start := time.Now()
	handle, errno := filesystem.Open("/session.jsonl", os.O_RDWR|os.O_APPEND)
	if errno != 0 {
		t.Fatalf("foreground open exposed maintenance contention: %v", errno)
	}
	defer filesystem.Release(handle)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("foreground waited %s for maintenance", elapsed)
	}
	select {
	case err := <-background:
		if !errors.Is(err, storage.ErrManagedSessionBusy) {
			t.Fatalf("background yielding classification: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("background did not release its locks")
	}
	tail := []byte("foreground-tail\n")
	if n, errno := filesystem.Write(handle, tail, int64(len(source))); errno != 0 || n != len(tail) {
		t.Fatalf("foreground append after yielding: n=%d errno=%v", n, errno)
	}
	if errno := filesystem.Fsync(handle); errno != 0 {
		t.Fatal(errno)
	}
	want := append(append([]byte(nil), source...), tail...)
	actual := make([]byte, len(want))
	if n, errno := filesystem.Read(handle, actual, 0); errno != 0 || n != len(want) || !bytes.Equal(actual, want) {
		t.Fatalf("readback after foreground preemption: n=%d errno=%v equal=%t", n, errno, bytes.Equal(actual, want))
	}
}

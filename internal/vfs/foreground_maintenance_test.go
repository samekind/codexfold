package vfs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/storage"
)

func TestForegroundWriterPreemptsInFlightCompaction(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	preparing := make(chan struct{})
	background := make(chan error, 1)
	go func() {
		_, err := session.Compact(context.Background(), CompactOptions{Prepare: func(ctx context.Context, _ NativeFile, _ uint64) (PreparedGeneration, error) {
			close(preparing)
			<-ctx.Done()
			return PreparedGeneration{}, ctx.Err()
		}})
		background <- err
	}()
	select {
	case <-preparing:
	case <-time.After(time.Second):
		t.Fatal("compaction did not enter its slow phase")
	}
	start := time.Now()
	writer, err := session.OpenForegroundWriter()
	if err != nil {
		t.Fatalf("foreground saw background compaction as a busy session: %v", err)
	}
	defer writer.Close()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("foreground preemption took %s", elapsed)
	}
	select {
	case err := <-background:
		if !errors.Is(err, storage.ErrManagedSessionBusy) {
			t.Fatalf("compaction yielding was not deferred: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("compaction remained blocked")
	}
	if session.State().Generation != 1 {
		t.Fatal("cancelled compaction published a partial generation")
	}
}

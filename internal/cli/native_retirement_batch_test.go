package cli

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/vfs"
)

func TestNativeRetirementBatchReusesProofAndRetainsMutationLock(t *testing.T) {
	fixture := interruptedCanonicalMigrationFixture(t)
	defer fixture.resolver.Close()
	batch := &nativeRetirementBatch{}
	defer batch.Close()
	for range 3 {
		_, resolver, err := batch.open(context.Background(), fixture.store, fixture.managed.State())
		if err != nil {
			t.Fatal(err)
		}
		resolver.Close()
	}
	if batch.verifications != 1 {
		t.Fatalf("full verifications=%d want=1", batch.verifications)
	}
	if _, err := pack.Build(context.Background(), fixture.store, pack.BuildOptions{}); err == nil {
		t.Fatal("pack mutation allowed during shared proof")
	}
	state := fixture.managed.State()
	data, err := os.ReadFile(state.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.ManifestPath, append(data, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if _, r, err := batch.open(context.Background(), fixture.store, state); err == nil {
		r.Close()
		t.Fatal("changed manifest reused proof")
	}
	batch.Close()
	if _, r, err := batch.open(context.Background(), fixture.store, state); err == nil {
		r.Close()
		t.Fatal("closed batch remained valid")
	}
}

func TestNativeRetirementBatchInspectsOnlyItsTargetState(t *testing.T) {
	fixture := interruptedCanonicalMigrationFixture(t)
	defer fixture.resolver.Close()
	other := filepath.Join(fixture.store, "fs", "sessions", "other")
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "state.json"), []byte("in-flight publication"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), nativeRetirementBatchKey{}, &nativeRetirementBatch{})
	state, err := nativeRetirementState(ctx, fixture.store, "session")
	if err != nil || state.SessionID != "session" {
		t.Fatalf("unrelated publication blocked target: %v", err)
	}
	if _, err := nativeRetirementState(ctx, fixture.store, "other"); err == nil {
		t.Fatal("invalid target state was accepted")
	}
}

func TestNativeRetirementConcurrencyYieldsToForegroundIO(t *testing.T) {
	for _, busy := range []bool{false, true} {
		name := "idle"
		want := int32(4)
		if busy {
			name = "busy"
			want = 1
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			ctx = context.WithValue(ctx, nativeRetirementWorkersKey{}, 4)
			ctx = context.WithValue(ctx, nativeRetirementIdleKey{}, func() bool { return !busy })
			var active, peak atomic.Int32
			var once sync.Once
			ready := make(chan struct{})
			states := make([]vfs.SessionState, 12)
			completed := 0
			runNativeRetirementGroup(ctx, states, func(vfs.SessionState) error {
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				if n == want {
					once.Do(func() { close(ready) })
				}
				select {
				case <-ready:
				case <-ctx.Done():
					return ctx.Err()
				}
				time.Sleep(time.Millisecond)
				return nil
			}, func(_ vfs.SessionState, err error) {
				if err != nil {
					t.Error(err)
				}
				completed++
			})
			if peak.Load() != want || completed != len(states) || active.Load() != 0 {
				t.Fatalf("peak=%d want=%d completed=%d active=%d", peak.Load(), want, completed, active.Load())
			}
		})
	}
}

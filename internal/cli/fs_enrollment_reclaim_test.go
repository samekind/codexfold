package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/enroll"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
)

func TestEnrollmentRemainingWaitingExcludesCompletedSessions(t *testing.T) {
	result := FSEnrollmentApplyResult{
		Plan: enroll.Plan{Decisions: []enroll.Decision{
			{Selected: true},
			{Reasons: []enroll.Reason{enroll.ReasonBatchLimit}},
		}},
	}
	for applied, want := range map[int]int{0: 2, 1: 1, 2: 0, 3: 0} {
		result.Apply.Applied = applied
		if got := enrollmentRemainingWaiting(result); got != want {
			t.Fatalf("applied=%d waiting=%d want=%d", applied, got, want)
		}
	}
}

func TestPeriodicEnrollmentDeferredReclaimStaysIncompleteWithoutError(t *testing.T) {
	oldRunner := runServiceEnrollmentCycle
	t.Cleanup(func() { runServiceEnrollmentCycle = oldRunner })
	store := t.TempDir()
	runServiceEnrollmentCycle = func(_ context.Context, _ enrollmentFlags, hooks enrollmentApplyHooks) (FSEnrollmentApplyResult, error) {
		hooks.reportProgress(5, 6)
		return FSEnrollmentApplyResult{Maintenance: FSEnrollmentMaintenanceResult{ReclaimDeferred: true}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPeriodicEnrollment(ctx, enrollmentFlags{storeDir: store}, time.Minute, nil)
	}()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		progress, err := enroll.LoadProgress(enroll.ProgressPath(store))
		if err == nil && progress.Phase == enroll.PhaseWaitingReclaim {
			if progress.CycleDone != 5 || progress.CycleTotal != 6 || progress.LastError != "" || progress.ErrorKind != "" {
				t.Fatalf("deferred cleanup reported failure/completion: %+v", progress)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("deferred cleanup was not published")
}

func TestEnrollmentReclaimProgressWaitsForMaintenance(t *testing.T) {
	for _, failure := range []string{"", "retire-native", "retire-loose", "gc", "busy"} {
		name := failure
		if name == "" {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			home, store, rollout := fsFixture(t, true)
			allowEnrollmentWriterProbe(t)
			allowEnrollmentNamespaceReadiness(t)
			saveEnrollmentObservations(t, store, map[string]string{"session": rollout})
			oldProbe, oldRunner := mountHealthProbe, runEnrollmentCommand
			oldDiscover, oldGC := discoverEnrollmentSessionStates, runEnrollmentStorageGC
			t.Cleanup(func() {
				mountHealthProbe, runEnrollmentCommand = oldProbe, oldRunner
				discoverEnrollmentSessionStates, runEnrollmentStorageGC = oldDiscover, oldGC
			})
			mountHealthProbe = func(string) error { return nil }
			discoverEnrollmentSessionStates = func(string) ([]vfs.SessionState, error) {
				return []vfs.SessionState{{SessionID: "session", NativeSnapshot: vfs.NativeFile{Path: rollout}}}, nil
			}
			var points [][2]int
			var maintenanceCalls []string
			assertIncomplete := func() {
				t.Helper()
				if len(points) == 0 || points[len(points)-1] != [2]int{5, 6} {
					t.Fatalf("maintenance must start and stay at 5/6, progress=%v", points)
				}
			}
			runEnrollmentCommand = func(_ context.Context, args []string) error {
				if len(args) > 1 && (args[1] == "retire-native" || args[1] == "retire-loose") {
					assertIncomplete()
					maintenanceCalls = append(maintenanceCalls, args[1])
					if failure == "busy" && args[1] == "retire-loose" {
						return storage.ErrManagedSessionBusy
					}
					if args[1] == failure {
						return errors.New("injected maintenance failure")
					}
				}
				return nil
			}
			runEnrollmentStorageGC = func(context.Context, string) (storage.StorageGCResult, error) {
				assertIncomplete()
				maintenanceCalls = append(maintenanceCalls, "gc")
				if failure == "gc" {
					return storage.StorageGCResult{}, errors.New("injected maintenance failure")
				}
				return storage.StorageGCResult{RemovedCount: 1}, nil
			}
			result, err := applyEnrollmentCycle(context.Background(), enrollmentFlags{
				codexHome: home, storeDir: store, mountPoint: filepath.Join(home, "mount"),
				nativeRoot: filepath.Join(home, "fold-native"), canonicalNamespace: true,
				stableFor: time.Nanosecond, batchSize: 1,
			}, enrollmentApplyHooks{onProgress: func(done, total int) { points = append(points, [2]int{done, total}) }})
			if result.Apply.Applied != 1 {
				t.Fatalf("applied=%d", result.Apply.Applied)
			}
			wantLast := 6
			if failure == "busy" {
				wantLast = 5
				if err != nil || !result.Maintenance.ReclaimDeferred {
					t.Fatalf("live writer should defer cleanup, result=%+v err=%v", result, err)
				}
			} else if failure != "" {
				wantLast = 5
				if err == nil || !strings.Contains(err.Error(), "injected maintenance failure") {
					t.Fatalf("missing maintenance error: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			var want [][2]int
			for done := 0; done <= wantLast; done++ {
				want = append(want, [2]int{done, 6})
			}
			if !reflect.DeepEqual(points, want) {
				t.Fatalf("progress=%v, want=%v", points, want)
			}
			if !reflect.DeepEqual(maintenanceCalls, []string{"retire-native", "retire-loose", "gc"}) {
				t.Fatalf("maintenance commands=%v", maintenanceCalls)
			}
		})
	}
}

func TestEnrollmentRetriesLooseAfterWriterClosesWithoutNewFold(t *testing.T) {
	oldDiscover, oldRunner, oldGC := discoverEnrollmentSessionStates, runEnrollmentCommand, runEnrollmentStorageGC
	t.Cleanup(func() {
		discoverEnrollmentSessionStates, runEnrollmentCommand, runEnrollmentStorageGC = oldDiscover, oldRunner, oldGC
	})
	store := t.TempDir()
	if err := os.MkdirAll(filepath.Join(store, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "objects", "pending.zst"), []byte("test pending object"), 0o600); err != nil {
		t.Fatal(err)
	}
	discoverEnrollmentSessionStates = func(string) ([]vfs.SessionState, error) { return []vfs.SessionState{{SessionID: "retired"}}, nil }
	busy := true
	calls := 0
	runEnrollmentCommand = func(_ context.Context, args []string) error {
		if args[1] != "retire-loose" {
			t.Fatalf("unexpected command %v", args)
		}
		calls++
		if busy {
			return storage.ErrManagedSessionBusy
		}
		return nil
	}
	runEnrollmentStorageGC = func(context.Context, string) (storage.StorageGCResult, error) {
		if busy {
			return storage.StorageGCResult{}, storage.ErrManagedSessionBusy
		}
		return storage.StorageGCResult{}, nil
	}
	first, err := runEnrollmentMaintenance(context.Background(), t.TempDir(), store, 0, true)
	if err != nil || !first.ReclaimDeferred || first.LooseRetirementRan {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	busy = false
	second, err := runEnrollmentMaintenance(context.Background(), t.TempDir(), store, 0, true)
	if err != nil || second.ReclaimDeferred || !second.LooseRetirementRan || calls != 2 {
		t.Fatalf("retry=%+v calls=%d err=%v", second, calls, err)
	}
}

func TestEnrollmentReclaimCancellationStopsRemainingWork(t *testing.T) {
	for _, cancelAt := range []string{"before", "retire-native", "retire-loose"} {
		t.Run(cancelAt, func(t *testing.T) {
			oldDiscover, oldRunner, oldGC := discoverEnrollmentSessionStates, runEnrollmentCommand, runEnrollmentStorageGC
			t.Cleanup(func() {
				discoverEnrollmentSessionStates, runEnrollmentCommand, runEnrollmentStorageGC = oldDiscover, oldRunner, oldGC
			})
			discoverEnrollmentSessionStates = func(string) ([]vfs.SessionState, error) {
				return []vfs.SessionState{
					{SessionID: "first", NativeSnapshot: vfs.NativeFile{Path: "/first"}},
					{SessionID: "second", NativeSnapshot: vfs.NativeFile{Path: "/second"}},
				}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls []string
			runEnrollmentCommand = func(_ context.Context, args []string) error {
				calls = append(calls, args[1])
				if args[1] == cancelAt {
					cancel()
				}
				return nil
			}
			runEnrollmentStorageGC = func(context.Context, string) (storage.StorageGCResult, error) {
				t.Fatal("GC ran after cancellation")
				return storage.StorageGCResult{}, nil
			}
			if cancelAt == "before" {
				cancel()
			}
			_, err := runEnrollmentMaintenance(ctx, t.TempDir(), t.TempDir(), 1, true)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v, want canceled", err)
			}
			var want []string
			if cancelAt == "retire-native" {
				want = []string{"retire-native"}
			}
			if cancelAt == "retire-loose" {
				want = []string{"retire-native", "retire-native", "retire-loose"}
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("commands=%v, want=%v", calls, want)
			}
		})
	}
}

func TestEnrollmentReclaimRetriesCollectionWithoutNativeSnapshots(t *testing.T) {
	oldDiscover, oldRunner, oldGC := discoverEnrollmentSessionStates, runEnrollmentCommand, runEnrollmentStorageGC
	t.Cleanup(func() {
		discoverEnrollmentSessionStates, runEnrollmentCommand, runEnrollmentStorageGC = oldDiscover, oldRunner, oldGC
	})
	discoverEnrollmentSessionStates = func(string) ([]vfs.SessionState, error) {
		return []vfs.SessionState{{SessionID: "already-retired"}}, nil
	}
	runEnrollmentCommand = func(context.Context, []string) error {
		t.Fatal("no native/loose retirement is needed")
		return nil
	}
	collected := 0
	runEnrollmentStorageGC = func(context.Context, string) (storage.StorageGCResult, error) {
		collected++
		return storage.StorageGCResult{RemovedCount: 1}, nil
	}
	result, err := runEnrollmentMaintenance(context.Background(), t.TempDir(), t.TempDir(), 0, true)
	if err != nil || collected != 1 || result.StorageGC.RemovedCount != 1 {
		t.Fatalf("deferred old pack was not reconsidered: calls=%d result=%+v err=%v", collected, result, err)
	}
}

func TestEnrollmentReclaimsEvenWhenAMigrationFails(t *testing.T) {
	home, store, rollout := fsFixture(t, true)
	allowEnrollmentWriterProbe(t)
	allowEnrollmentNamespaceReadiness(t)
	saveEnrollmentObservations(t, store, map[string]string{"session": rollout})
	oldProbe, oldRunner := mountHealthProbe, runEnrollmentCommand
	oldDiscover, oldGC := discoverEnrollmentSessionStates, runEnrollmentStorageGC
	t.Cleanup(func() {
		mountHealthProbe, runEnrollmentCommand = oldProbe, oldRunner
		discoverEnrollmentSessionStates, runEnrollmentStorageGC = oldDiscover, oldGC
	})
	mountHealthProbe = func(string) error { return nil }
	discoverEnrollmentSessionStates = func(string) ([]vfs.SessionState, error) {
		return []vfs.SessionState{{SessionID: "session", NativeSnapshot: vfs.NativeFile{Path: rollout}}}, nil
	}
	var maintenanceCalls []string
	runEnrollmentCommand = func(_ context.Context, args []string) error {
		if len(args) > 1 && args[0] == "fs" && args[1] == "migrate" {
			return errors.New("retained canonical snapshot already exists")
		}
		if len(args) > 1 && (args[1] == "retire-native" || args[1] == "retire-loose") {
			maintenanceCalls = append(maintenanceCalls, args[1])
		}
		return nil
	}
	runEnrollmentStorageGC = func(context.Context, string) (storage.StorageGCResult, error) {
		maintenanceCalls = append(maintenanceCalls, "gc")
		return storage.StorageGCResult{RemovedCount: 1}, nil
	}

	result, err := applyEnrollmentCycle(context.Background(), enrollmentFlags{
		codexHome: home, storeDir: store, mountPoint: filepath.Join(home, "mount"),
		nativeRoot: filepath.Join(home, "fold-native"), canonicalNamespace: true,
		stableFor: time.Nanosecond, batchSize: 1,
	}, enrollmentApplyHooks{})

	if err == nil || !strings.Contains(err.Error(), "retained canonical snapshot already exists") {
		t.Fatalf("a session that cannot be routed must still be reported: %v", err)
	}
	if result.Apply.Applied != 0 {
		t.Fatalf("applied=%d, want 0", result.Apply.Applied)
	}
	// The pack build earlier in the cycle has already written a new generation.
	// Skipping reclamation because one session is stuck leaves the store larger
	// than it started, every cycle, for as long as that session stays stuck. What
	// does wait for a clean cycle is deleting the user's original.
	if !reflect.DeepEqual(maintenanceCalls, []string{"retire-loose", "gc"}) {
		t.Fatalf("reclamation did not run after a failed migration: %v", maintenanceCalls)
	}
	if result.Maintenance.NativeRetired != 0 || result.Maintenance.NativeDeferred == 0 {
		t.Fatalf("native retirement must wait for a clean cycle: %+v", result.Maintenance)
	}
}

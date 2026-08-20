package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/vfs"
)

func TestManagedSessionRegistryBootstrapRequiresObservableOrPristineStore(t *testing.T) {
	t.Run("observable sessions root", func(t *testing.T) {
		store := newManagedSessionRegistryStore(t)
		if err := os.Mkdir(filepath.Join(store, "fs", "sessions"), 0o700); err != nil {
			t.Fatal(err)
		}
		entries, err := managedSessionRegistryBootstrapEntries(store, map[string]uint64{"beta": 2, "alpha": 1})
		if err != nil {
			t.Fatal(err)
		}
		assertManagedSessionRegistryEntries(t, entries, []ManagedSessionRegistryEntry{{ID: "alpha", Generation: 1}, {ID: "beta", Generation: 2}})
	})

	t.Run("unknown generation", func(t *testing.T) {
		store := newManagedSessionRegistryStore(t)
		if err := os.Mkdir(filepath.Join(store, "fs", "sessions"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := managedSessionRegistryBootstrapEntries(store, map[string]uint64{"unavailable": 0}); err == nil || !strings.Contains(err.Error(), "generation") {
			t.Fatalf("unknown-generation bootstrap error = %v", err)
		}
	})

	t.Run("missing root with durable evidence", func(t *testing.T) {
		store := newManagedSessionRegistryStore(t)
		if err := os.MkdirAll(filepath.Join(store, "manifests"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(store, "manifests", "session.json"), []byte("durable evidence"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := managedSessionRegistryBootstrapEntries(store, nil); err == nil || !strings.Contains(err.Error(), "durable managed evidence") {
			t.Fatalf("evidence bootstrap error = %v", err)
		}
	})

	t.Run("pristine store", func(t *testing.T) {
		store := newManagedSessionRegistryStore(t)
		entries, err := managedSessionRegistryBootstrapEntries(store, nil)
		if err != nil || len(entries) != 0 {
			t.Fatalf("pristine bootstrap entries=%#v err=%v", entries, err)
		}
	})
}

func TestManagedSessionRegistryAtomicallyBootstrapsExactActiveStates(t *testing.T) {
	store := newManagedSessionRegistryStore(t)
	if err := os.Mkdir(filepath.Join(store, "fs", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManagedSessionRegistry(store); !os.IsNotExist(err) {
		t.Fatalf("registry before bootstrap error = %v", err)
	}
	entries, err := managedSessionRegistryBootstrapEntries(store, map[string]uint64{
		"beta":  2,
		"alpha": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := WriteManagedSessionRegistry(store, entries, ManagedSessionRegistryWriteOptions{Bootstrap: true})
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 {
		t.Fatalf("bootstrap revision = %d, want 1", created.Revision)
	}
	want := []ManagedSessionRegistryEntry{{ID: "alpha", Generation: 1}, {ID: "beta", Generation: 2}}
	assertManagedSessionRegistryEntries(t, created.Entries, want)
	loaded, err := LoadManagedSessionRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	assertManagedSessionRegistryEntries(t, loaded.Entries, want)
	if temporaries, err := filepath.Glob(filepath.Join(store, "fs", ".managed-registry-*.tmp")); err != nil || len(temporaries) != 0 {
		t.Fatalf("bootstrap temporary registry files = %#v err=%v", temporaries, err)
	}
}

func TestManagedSessionRegistryRefusesEmptyBootstrapWhenSessionsRootIsMissingWithDurableEvidence(t *testing.T) {
	store := newManagedSessionRegistryStore(t)
	if err := os.MkdirAll(filepath.Join(store, "fs", "snapshots", "session"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "fs", "snapshots", "session", "native.jsonl"), []byte("durable evidence"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := managedSessionRegistryBootstrapEntries(store, nil); err == nil || !strings.Contains(err.Error(), "durable managed evidence") {
		t.Fatalf("unsafe empty bootstrap error = %v", err)
	}
	if _, err := LoadManagedSessionRegistry(store); !os.IsNotExist(err) {
		t.Fatalf("unsafe empty bootstrap published registry: err=%v", err)
	}
}

func TestDurableManagedRegistryRemainsTheBaselineWhenDiscoveryDisappears(t *testing.T) {
	retained := retainedManagedSessionGenerations(
		map[string]uint64{"loaded": 3},
		nil,
		nil,
	)
	mergeManagedSessionRegistryGenerations(retained, ManagedSessionRegistry{
		Version: managedSessionRegistryVersion,
		Entries: []ManagedSessionRegistryEntry{{ID: "durable", Generation: 7}},
	})
	if retained["loaded"] != 3 || retained["durable"] != 7 || len(retained) != 2 {
		t.Fatalf("missing discovery shrank durable baseline: %#v", retained)
	}
}

func TestManagedReloadFinalPublicationDoesNotResurrectConcurrentRetirement(t *testing.T) {
	store := newManagedSessionRegistryStore(t)
	initial, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{{ID: "session", Generation: 4}}, ManagedSessionRegistryWriteOptions{Bootstrap: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := removeManagedSessionRegistryEntryIfPresent(store, "session"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := publishManagedSessionRegistryAtFinalFence(store, map[string]uint64{"session": 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Registry.Revision <= initial.Revision || len(snapshot.Registry.Entries) != 0 || len(snapshot.Retained) != 0 {
		t.Fatalf("final publication resurrected concurrent retirement: %#v", snapshot)
	}
}

func TestManagedReloadFinalPublicationDefersToConcurrentExplicitDeletion(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	state, err := managedState(fixture.store, fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteManagedSessionRegistry(fixture.store, []ManagedSessionRegistryEntry{{ID: fixture.sessionID, Generation: state.Generation}}, ManagedSessionRegistryWriteOptions{Bootstrap: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := vfs.PublishSessionDeletion(fixture.store, state, fixture.request.Route); err != nil {
		t.Fatal(err)
	}
	known := map[string]uint64{fixture.sessionID: state.Generation}
	snapshot, err := publishManagedSessionRegistryAtFinalFence(fixture.store, known, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Registry.Entries) != 0 || len(snapshot.Retained) != 0 || len(snapshot.States) != 0 {
		t.Fatalf("final publication ignored explicit deletion: %#v", snapshot)
	}
	if _, exists := known[fixture.sessionID]; exists {
		t.Fatalf("explicitly deleted session remained known: %#v", known)
	}
}

func TestManagedReloadPublishesRecoveredGeneration(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	statePath := filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID, "state.json")
	stale, err := managedState(fixture.store, fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := vfs.RepublishSessionState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Generation != stale.Generation+1 {
		t.Fatalf("recovered generation = %d, want %d", recovered.Generation, stale.Generation+1)
	}
	if _, err := WriteManagedSessionRegistry(fixture.store, []ManagedSessionRegistryEntry{{ID: fixture.sessionID, Generation: stale.Generation}}, ManagedSessionRegistryWriteOptions{Bootstrap: true}); err != nil {
		t.Fatal(err)
	}
	filesystem := mountfs.NewCanonical()
	filesystem.SetNativeRoot(fixture.nativeRoot)
	t.Cleanup(func() { _ = filesystem.CloseSessions() })
	known := make(map[string]uint64)
	knownRoutes := make(map[string]string)
	knownPacks := make(map[string]string)
	if err := upsertCanonicalManagedState(
		fixture.store, filesystem, stale, fixture.request.Route,
		known, knownRoutes, knownPacks,
		func(state vfs.SessionState) (*vfs.Session, *pack.Resolver, error) {
			return openManagedSession(context.Background(), fixture.store, state)
		},
	); err != nil {
		t.Fatal(err)
	}
	if known[fixture.sessionID] != recovered.Generation {
		t.Fatalf("known generation = %d, want %d", known[fixture.sessionID], recovered.Generation)
	}
	data, err := os.ReadFile(filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID, "mounted.json"))
	if err != nil {
		t.Fatal(err)
	}
	var acknowledgement mountAcknowledgement
	if err := json.Unmarshal(data, &acknowledgement); err != nil {
		t.Fatal(err)
	}
	if acknowledgement.Generation != recovered.Generation || acknowledgement.Route != fixture.request.Route {
		t.Fatalf("mount acknowledgement = %#v, want generation %d", acknowledgement, recovered.Generation)
	}
	snapshot, err := publishManagedSessionRegistryAtFinalFence(fixture.store, known, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertManagedSessionRegistryEntries(t, snapshot.Registry.Entries, []ManagedSessionRegistryEntry{{ID: fixture.sessionID, Generation: recovered.Generation}})
}

func TestRollbackCanonicalMigrationPreservesManagedOwnerAndRegistry(t *testing.T) {
	fixture := interruptedCanonicalMigrationFixture(t)
	state := fixture.managed.State()
	if _, err := WriteManagedSessionRegistry(fixture.store, []ManagedSessionRegistryEntry{{ID: state.SessionID, Generation: state.Generation}}, ManagedSessionRegistryWriteOptions{Bootstrap: true}); err != nil {
		_ = fixture.resolver.Close()
		t.Fatal(err)
	}
	filesystem := mountfs.NewCanonical()
	filesystem.SetNativeRoot(fixture.nativeRoot)
	route := "/archived_sessions/rollout-session.jsonl"
	if err := filesystem.AddSessionAtOwned(state.SessionID, route, fixture.managed, fixture.resolver); err != nil {
		_ = fixture.resolver.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = filesystem.CloseSessions() })
	known := map[string]uint64{state.SessionID: state.Generation}
	cause := errors.New("force canonical migration failure")
	if err := rollbackCanonicalMigration(fixture.nativePath, state.NativeSnapshot.Path, cause); !errors.Is(err, cause) {
		t.Fatalf("rollback error = %v, want original cause", err)
	}
	snapshot, err := publishManagedSessionRegistryAtFinalFence(fixture.store, known, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertManagedSessionRegistryEntries(t, snapshot.Registry.Entries, []ManagedSessionRegistryEntry{{ID: state.SessionID, Generation: state.Generation}})
	if known[state.SessionID] != state.Generation {
		t.Fatalf("migration failure removed live known owner: %#v", known)
	}
	if got := readMountedFilesystemFile(t, filesystem, route, len(fixture.source)); !bytes.Equal(got, fixture.source) {
		t.Fatalf("managed owner changed after migration failure: %q", got)
	}
}

func TestRemoveAndRestoreManagedSessionRegistryEntryAreExactAndIdempotent(t *testing.T) {
	store := newManagedSessionRegistryStore(t)
	if _, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{
		{ID: "remove", Generation: 2},
		{ID: "retain", Generation: 3},
	}, ManagedSessionRegistryWriteOptions{Bootstrap: true}); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := removeManagedSessionRegistryEntryIfPresent(store, "remove"); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := LoadManagedSessionRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	assertManagedSessionRegistryEntries(t, registry.Entries, []ManagedSessionRegistryEntry{{ID: "retain", Generation: 3}})

	if err := upsertManagedSessionRegistryEntryIfPresent(store, "remove", 4); err != nil {
		t.Fatal(err)
	}
	registry, err = LoadManagedSessionRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	assertManagedSessionRegistryEntries(t, registry.Entries, []ManagedSessionRegistryEntry{{ID: "remove", Generation: 4}, {ID: "retain", Generation: 3}})

	missingStore := filepath.Join(t.TempDir(), "missing-store")
	if err := removeManagedSessionRegistryEntryIfPresent(missingStore, "session"); err != nil {
		t.Fatalf("missing registry removal error = %v", err)
	}
	if err := upsertManagedSessionRegistryEntryIfPresent(missingStore, "session", 1); err != nil {
		t.Fatalf("missing registry upsert error = %v", err)
	}
}

func TestCommittedRetirementRegistryRemovalSurvivesRequestClearCrashAndRestartedReload(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	if _, err := WriteManagedSessionRegistry(fixture.store, []ManagedSessionRegistryEntry{{
		ID: fixture.sessionID, Generation: fixture.request.Generation,
	}}, ManagedSessionRegistryWriteOptions{Bootstrap: true}); err != nil {
		t.Fatal(err)
	}

	crash := errors.New("simulated crash before retirement request clear")
	crashObserved := false
	result, err := recoverCanonicalRetirementsWithOptions(
		context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot,
		func(_ string, phase string) error {
			if phase != retirementPhaseBeforeRequestCleared {
				return nil
			}
			crashObserved = true
			return crash
		},
		commitCanonicalRetirementForRegistryTest,
	)
	if !errors.Is(err, crash) || !crashObserved || result.Completed != 0 || result.Restored != 0 {
		t.Fatalf("crashed completion result=%#v observed=%t err=%v", result, crashObserved, err)
	}
	registry, err := LoadManagedSessionRegistry(fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	assertManagedSessionRegistryEntries(t, registry.Entries, nil)
	pending, err := discoverPendingCanonicalRetirements(fixture.store)
	if err != nil || len(pending) != 1 || pending[0].SessionID != fixture.sessionID {
		t.Fatalf("pending committed retirement after crash = %#v err=%v", pending, err)
	}

	result, err = recoverCanonicalRetirementsWithOwnerHandoff(
		context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot,
		commitCanonicalRetirementForRegistryTest,
	)
	if err != nil || result.Completed != 1 || result.Restored != 0 || len(result.CompletedSessionIDs) != 1 || result.CompletedSessionIDs[0] != fixture.sessionID {
		t.Fatalf("restarted completion result=%#v err=%v", result, err)
	}
	registry = publishManagedRegistryAsRestartedReload(
		t,
		fixture.store,
		map[string]uint64{fixture.sessionID: fixture.request.Generation},
		result.CompletedSessionIDs,
	)
	assertManagedSessionRegistryEntries(t, registry.Entries, nil)
}

func TestCompensatedRetirementRegistryGenerationSurvivesRequestClearCrashAndRestartedReload(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	if _, err := WriteManagedSessionRegistry(fixture.store, []ManagedSessionRegistryEntry{{
		ID: fixture.sessionID, Generation: fixture.request.Generation,
	}}, ManagedSessionRegistryWriteOptions{Bootstrap: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.targetPath, []byte("changed native rollback target"), 0o600); err != nil {
		t.Fatal(err)
	}

	crash := errors.New("simulated crash before compensation request clear")
	crashObserved := false
	result, err := recoverCanonicalRetirementsWithOptions(
		context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot,
		func(_ string, phase string) error {
			if phase != retirementPhaseBeforeRequestCleared {
				return nil
			}
			crashObserved = true
			return crash
		},
		commitCanonicalRetirementForRegistryTest,
	)
	if !errors.Is(err, crash) || !crashObserved || result.Completed != 0 || result.Restored != 0 {
		t.Fatalf("crashed compensation result=%#v observed=%t err=%v", result, crashObserved, err)
	}
	wantGeneration := fixture.request.Generation + 1
	registry, err := LoadManagedSessionRegistry(fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	assertManagedSessionRegistryEntries(t, registry.Entries, []ManagedSessionRegistryEntry{{
		ID: fixture.sessionID, Generation: wantGeneration,
	}})
	state, err := vfs.LoadSessionState(filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID, "state.json"))
	if err != nil || state.Generation != wantGeneration {
		t.Fatalf("compensated state generation=%d want=%d err=%v", state.Generation, wantGeneration, err)
	}

	result, err = recoverCanonicalRetirementsWithOwnerHandoff(
		context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot,
		commitCanonicalRetirementForRegistryTest,
	)
	if err != nil || result.Completed != 0 || result.Restored != 1 || len(result.RestoredSessionIDs) != 1 || result.RestoredSessionIDs[0] != fixture.sessionID {
		t.Fatalf("restarted compensation result=%#v err=%v", result, err)
	}
	registry = publishManagedRegistryAsRestartedReload(
		t,
		fixture.store,
		map[string]uint64{fixture.sessionID: fixture.request.Generation},
		nil,
	)
	assertManagedSessionRegistryEntries(t, registry.Entries, []ManagedSessionRegistryEntry{{
		ID: fixture.sessionID, Generation: wantGeneration,
	}})
}

func commitCanonicalRetirementForRegistryTest(_ string, _ string, commit func() error) error {
	if commit == nil {
		return nil
	}
	return commit()
}

func publishManagedRegistryAsRestartedReload(
	t *testing.T,
	store string,
	staleKnown map[string]uint64,
	completedSessionIDs []string,
) ManagedSessionRegistry {
	t.Helper()
	states, issues, err := vfs.DiscoverSessionStatesDetailedReadOnly(store)
	if err != nil {
		t.Fatal(err)
	}
	expected := retainedManagedSessionGenerations(staleKnown, states, issues)
	registry, err := LoadManagedSessionRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	revision := registry.Revision
	mergeManagedSessionRegistryGenerations(expected, registry)
	removals := make(map[string]struct{}, len(completedSessionIDs))
	for _, sessionID := range completedSessionIDs {
		delete(expected, sessionID)
		removals[sessionID] = struct{}{}
	}
	published, err := WriteManagedSessionRegistry(
		store,
		managedSessionRegistryEntriesFromGenerations(expected),
		ManagedSessionRegistryWriteOptions{
			ExpectedRevision:  &revision,
			RemovedSessionIDs: removals,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return published
}

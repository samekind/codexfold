package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/storage"
)

func TestManagedSessionRegistryBootstrapAndRoundTrip(t *testing.T) {
	store := newManagedSessionRegistryStore(t)
	if _, err := LoadManagedSessionRegistry(store); !errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrManagedSessionRegistryCorrupt) {
		t.Fatalf("missing registry error = %v", err)
	}
	if _, err := WriteManagedSessionRegistry(store, nil, ManagedSessionRegistryWriteOptions{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-bootstrap registry creation error = %v", err)
	}
	created, err := WriteManagedSessionRegistry(store, nil, ManagedSessionRegistryWriteOptions{Bootstrap: true})
	if err != nil {
		t.Fatal(err)
	}
	if created.Version != managedSessionRegistryVersion || created.Entries == nil || len(created.Entries) != 0 {
		t.Fatalf("created registry = %#v", created)
	}

	written, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{
		{ID: "beta", Generation: 2},
		{ID: "alpha", Generation: 1},
	}, ManagedSessionRegistryWriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []ManagedSessionRegistryEntry{{ID: "alpha", Generation: 1}, {ID: "beta", Generation: 2}}
	assertManagedSessionRegistryEntries(t, written.Entries, want)
	loaded, err := LoadManagedSessionRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	assertManagedSessionRegistryEntries(t, loaded.Entries, want)

	info, err := os.Lstat(ManagedSessionRegistryPath(store))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("registry mode = %v", info.Mode())
	}
	data, err := os.ReadFile(ManagedSessionRegistryPath(store))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(string(data), `"id": "alpha"`) > strings.Index(string(data), `"id": "beta"`) {
		t.Fatalf("registry entries are not sorted: %s", data)
	}
	if temporaries, err := filepath.Glob(filepath.Join(store, "fs", ".managed-registry-*.tmp")); err != nil || len(temporaries) != 0 {
		t.Fatalf("temporary registry files = %#v err=%v", temporaries, err)
	}
}

func TestManagedSessionRegistryNeverShrinksWithoutExactRemoval(t *testing.T) {
	store := newManagedSessionRegistryStore(t)
	bootstrapManagedSessionRegistry(t, store)
	if _, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{
		{ID: "alpha", Generation: 2},
		{ID: "beta", Generation: 3},
	}, ManagedSessionRegistryWriteOptions{}); err != nil {
		t.Fatal(err)
	}

	merged, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{
		{ID: "alpha", Generation: 4},
	}, ManagedSessionRegistryWriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertManagedSessionRegistryEntries(t, merged.Entries, []ManagedSessionRegistryEntry{
		{ID: "alpha", Generation: 4},
		{ID: "beta", Generation: 3},
	})

	removed, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{
		{ID: "alpha", Generation: 4},
	}, ManagedSessionRegistryWriteOptions{RemovedSessionIDs: map[string]struct{}{"beta": {}}})
	if err != nil {
		t.Fatal(err)
	}
	assertManagedSessionRegistryEntries(t, removed.Entries, []ManagedSessionRegistryEntry{{ID: "alpha", Generation: 4}})

	empty, err := WriteManagedSessionRegistry(store, nil, ManagedSessionRegistryWriteOptions{
		RemovedSessionIDs: map[string]struct{}{"alpha": {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if empty.Entries == nil || len(empty.Entries) != 0 {
		t.Fatalf("empty registry = %#v", empty)
	}
}

func TestManagedSessionRegistryRejectsStaleExpectedRevision(t *testing.T) {
	store := newManagedSessionRegistryStore(t)
	initial, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{
		{ID: "session", Generation: 1},
	}, ManagedSessionRegistryWriteOptions{Bootstrap: true})
	if err != nil {
		t.Fatal(err)
	}
	staleRevision := initial.Revision

	current, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{
		{ID: "session", Generation: 2},
	}, ManagedSessionRegistryWriteOptions{ExpectedRevision: &staleRevision})
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision == staleRevision {
		t.Fatalf("registry revision did not advance: stale=%d current=%d", staleRevision, current.Revision)
	}

	if _, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{
		{ID: "stale-observation", Generation: 1},
	}, ManagedSessionRegistryWriteOptions{ExpectedRevision: &staleRevision}); !errors.Is(err, ErrManagedSessionRegistryChanged) {
		t.Fatalf("stale compare-and-swap error = %v", err)
	}
	loaded, err := LoadManagedSessionRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	if !equalManagedSessionRegistries(loaded, current) {
		t.Fatalf("stale compare-and-swap changed registry: got=%#v want=%#v", loaded, current)
	}
}

func TestManagedSessionRegistryExactRemovalCannotBeUndoneByStaleObservation(t *testing.T) {
	store := newManagedSessionRegistryStore(t)
	stale, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{
		{ID: "remove", Generation: 4},
		{ID: "retain", Generation: 3},
	}, ManagedSessionRegistryWriteOptions{Bootstrap: true})
	if err != nil {
		t.Fatal(err)
	}
	staleRevision := stale.Revision

	removed, err := WriteManagedSessionRegistry(store, nil, ManagedSessionRegistryWriteOptions{
		ExpectedRevision:  &staleRevision,
		RemovedSessionIDs: map[string]struct{}{"remove": {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertManagedSessionRegistryEntries(t, removed.Entries, []ManagedSessionRegistryEntry{{ID: "retain", Generation: 3}})

	if _, err := WriteManagedSessionRegistry(store, stale.Entries, ManagedSessionRegistryWriteOptions{
		ExpectedRevision: &staleRevision,
	}); !errors.Is(err, ErrManagedSessionRegistryChanged) {
		t.Fatalf("stale observation resurrection error = %v", err)
	}
	loaded, err := LoadManagedSessionRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	if !equalManagedSessionRegistries(loaded, removed) {
		t.Fatalf("stale observation resurrected exact removal: got=%#v want=%#v", loaded, removed)
	}
}

func TestManagedSessionRegistryAbsentExactRemovalFencesStaleWriter(t *testing.T) {
	store := newManagedSessionRegistryStore(t)
	initial, err := WriteManagedSessionRegistry(store, nil, ManagedSessionRegistryWriteOptions{Bootstrap: true})
	if err != nil {
		t.Fatal(err)
	}
	staleRevision := initial.Revision
	fenced, err := WriteManagedSessionRegistry(store, nil, ManagedSessionRegistryWriteOptions{
		ExpectedRevision:  &staleRevision,
		RemovedSessionIDs: map[string]struct{}{"absent": {}},
		RemovalFence:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fenced.Revision != staleRevision+1 || len(fenced.Entries) != 0 {
		t.Fatalf("absent removal fence = %#v, want revision %d", fenced, staleRevision+1)
	}
	if _, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{{ID: "absent", Generation: 1}}, ManagedSessionRegistryWriteOptions{
		ExpectedRevision: &staleRevision,
	}); !errors.Is(err, ErrManagedSessionRegistryChanged) {
		t.Fatalf("stale writer after absent removal fence error = %v", err)
	}
}

func TestManagedSessionRegistryRemovalRetryResyncsAfterRenameSyncFailure(t *testing.T) {
	store := newManagedSessionRegistryStore(t)
	initial, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{{ID: "session", Generation: 1}}, ManagedSessionRegistryWriteOptions{Bootstrap: true})
	if err != nil {
		t.Fatal(err)
	}
	previousHook := managedSessionRegistrySyncHook
	t.Cleanup(func() { managedSessionRegistrySyncHook = previousHook })
	failSync := true
	managedSessionRegistrySyncHook = func() error {
		if failSync {
			failSync = false
			return errors.New("injected directory sync failure")
		}
		return nil
	}
	if err := removeManagedSessionRegistryEntryIfPresent(store, "session"); err == nil || !strings.Contains(err.Error(), "injected directory sync failure") {
		t.Fatalf("first removal error = %v", err)
	}
	afterFailedSync, err := LoadManagedSessionRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	if afterFailedSync.Revision != initial.Revision+1 || len(afterFailedSync.Entries) != 0 {
		t.Fatalf("registry after failed directory sync = %#v", afterFailedSync)
	}
	if err := removeManagedSessionRegistryEntryIfPresent(store, "session"); err != nil {
		t.Fatalf("retry exact removal: %v", err)
	}
	afterRetry, err := LoadManagedSessionRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	if afterRetry.Revision != afterFailedSync.Revision+1 || len(afterRetry.Entries) != 0 {
		t.Fatalf("registry after removal retry = %#v", afterRetry)
	}
}

func TestManagedSessionRegistrySemanticNoOpDoesNotRewriteFile(t *testing.T) {
	store := newManagedSessionRegistryStore(t)
	initial, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{
		{ID: "alpha", Generation: 2},
		{ID: "beta", Generation: 3},
	}, ManagedSessionRegistryWriteOptions{Bootstrap: true})
	if err != nil {
		t.Fatal(err)
	}
	path := ManagedSessionRegistryPath(store)
	fixedModTime := time.Unix(123, 0)
	if err := os.Chtimes(path, fixedModTime, fixedModTime); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	revision := initial.Revision

	unchanged, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{
		{ID: "beta", Generation: 3},
		{ID: "alpha", Generation: 2},
	}, ManagedSessionRegistryWriteOptions{ExpectedRevision: &revision})
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Revision != initial.Revision {
		t.Fatalf("semantic no-op revision = %d, want %d", unchanged.Revision, initial.Revision)
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	afterData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatalf("semantic no-op replaced registry file: before=%v after=%v", before, after)
	}
	if !before.ModTime().Equal(after.ModTime()) || string(beforeData) != string(afterData) {
		t.Fatalf("semantic no-op rewrote registry: before_mtime=%v after_mtime=%v", before.ModTime(), after.ModTime())
	}
	if temporaries, err := filepath.Glob(filepath.Join(store, "fs", ".managed-registry-*.tmp")); err != nil || len(temporaries) != 0 {
		t.Fatalf("semantic no-op temporary registry files = %#v err=%v", temporaries, err)
	}
}

func TestManagedSessionRegistryRejectsRollbackAndConflictingRemoval(t *testing.T) {
	store := newManagedSessionRegistryStore(t)
	bootstrapManagedSessionRegistry(t, store)
	if _, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{{ID: "session", Generation: 4}}, ManagedSessionRegistryWriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{{ID: "session", Generation: 3}}, ManagedSessionRegistryWriteOptions{}); err == nil || !strings.Contains(err.Error(), "backwards") {
		t.Fatalf("generation rollback error = %v", err)
	}
	if _, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{{ID: "session", Generation: 4}}, ManagedSessionRegistryWriteOptions{
		RemovedSessionIDs: map[string]struct{}{"session": {}},
	}); err == nil || !strings.Contains(err.Error(), "observed and removed") {
		t.Fatalf("conflicting removal error = %v", err)
	}
	loaded, err := LoadManagedSessionRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	assertManagedSessionRegistryEntries(t, loaded.Entries, []ManagedSessionRegistryEntry{{ID: "session", Generation: 4}})
}

func TestManagedSessionRegistryAtomicallyBootstrapsObservedEntries(t *testing.T) {
	store := newManagedSessionRegistryStore(t)
	created, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{{ID: "session", Generation: 1}}, ManagedSessionRegistryWriteOptions{Bootstrap: true})
	if err != nil {
		t.Fatal(err)
	}
	assertManagedSessionRegistryEntries(t, created.Entries, []ManagedSessionRegistryEntry{{ID: "session", Generation: 1}})
	loaded, err := LoadManagedSessionRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	assertManagedSessionRegistryEntries(t, loaded.Entries, created.Entries)
	missing := newManagedSessionRegistryStore(t)
	if _, err := WriteManagedSessionRegistry(missing, nil, ManagedSessionRegistryWriteOptions{
		Bootstrap: true, RemovedSessionIDs: map[string]struct{}{"session": {}},
	}); err == nil || !strings.Contains(err.Error(), "cannot apply removals") {
		t.Fatalf("bootstrap removal conflict error = %v", err)
	}
}

func TestManagedSessionRegistryBootstrapCreatesOnlyTheRegistryDirectory(t *testing.T) {
	root := realManagedSessionRegistryTempDir(t)
	store := filepath.Join(root, "store")
	if err := os.Mkdir(store, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManagedSessionRegistry(store); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing bootstrap registry error = %v", err)
	}
	created, err := WriteManagedSessionRegistry(store, nil, ManagedSessionRegistryWriteOptions{Bootstrap: true})
	if err != nil {
		t.Fatal(err)
	}
	if created.Entries == nil || len(created.Entries) != 0 {
		t.Fatalf("created bootstrap registry = %#v", created)
	}
	info, err := os.Lstat(filepath.Join(store, "fs"))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("bootstrap registry directory info=%v err=%v", info, err)
	}
}

func TestManagedSessionRegistryUpdateUsesOperationLock(t *testing.T) {
	store := newManagedSessionRegistryStore(t)
	bootstrapManagedSessionRegistry(t, store)
	lock, err := storage.AcquireOperationLock(store, managedSessionRegistryOperationLock)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := WriteManagedSessionRegistry(store, []ManagedSessionRegistryEntry{{ID: "session", Generation: 1}}, ManagedSessionRegistryWriteOptions{}); !errors.Is(err, storage.ErrOperationLockHeld) {
		t.Fatalf("concurrent registry update error = %v", err)
	}
	loaded, err := LoadManagedSessionRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Entries == nil || len(loaded.Entries) != 0 {
		t.Fatalf("locked registry changed = %#v", loaded)
	}
}

func TestManagedSessionRegistryRejectsCorruptSchemas(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "version", data: `{"version":2,"entries":[]}`},
		{name: "null entries", data: `{"version":1,"entries":null}`},
		{name: "unknown field", data: `{"version":1,"entries":[],"unknown":true}`},
		{name: "trailing value", data: `{"version":1,"entries":[]} {}`},
		{name: "unsorted", data: `{"version":1,"entries":[{"id":"beta","generation":1},{"id":"alpha","generation":1}]}`},
		{name: "duplicate", data: `{"version":1,"entries":[{"id":"session","generation":1},{"id":"session","generation":2}]}`},
		{name: "unsafe id", data: `{"version":1,"entries":[{"id":"../session","generation":1}]}`},
		{name: "zero generation", data: `{"version":1,"entries":[{"id":"session","generation":0}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newManagedSessionRegistryStore(t)
			writeRawManagedSessionRegistry(t, store, []byte(test.data))
			if _, err := LoadManagedSessionRegistry(store); !errors.Is(err, ErrManagedSessionRegistryCorrupt) || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("corrupt registry error = %v", err)
			}
			before, err := os.ReadFile(ManagedSessionRegistryPath(store))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := WriteManagedSessionRegistry(store, nil, ManagedSessionRegistryWriteOptions{}); !errors.Is(err, ErrManagedSessionRegistryCorrupt) {
				t.Fatalf("corrupt registry update error = %v", err)
			}
			after, err := os.ReadFile(ManagedSessionRegistryPath(store))
			if err != nil || string(after) != string(before) {
				t.Fatalf("corrupt registry was replaced: before=%q after=%q err=%v", before, after, err)
			}
		})
	}
}

func TestManagedSessionRegistryRejectsOversizeAndUnsafePaths(t *testing.T) {
	t.Run("oversize", func(t *testing.T) {
		store := newManagedSessionRegistryStore(t)
		writeRawManagedSessionRegistry(t, store, make([]byte, managedSessionRegistryMaxBytes+1))
		if _, err := LoadManagedSessionRegistry(store); !errors.Is(err, ErrManagedSessionRegistryCorrupt) {
			t.Fatalf("oversize registry error = %v", err)
		}
	})

	t.Run("final symlink", func(t *testing.T) {
		store := newManagedSessionRegistryStore(t)
		outside := filepath.Join(t.TempDir(), "outside.json")
		if err := os.WriteFile(outside, []byte(`{"version":1,"entries":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, ManagedSessionRegistryPath(store)); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadManagedSessionRegistry(store); !errors.Is(err, ErrManagedSessionRegistryCorrupt) {
			t.Fatalf("symlink registry error = %v", err)
		}
		if got, err := os.ReadFile(outside); err != nil || string(got) != `{"version":1,"entries":[]}` {
			t.Fatalf("outside registry changed: %q err=%v", got, err)
		}
	})

	t.Run("fs symlink", func(t *testing.T) {
		store := realManagedSessionRegistryTempDir(t)
		outside := realManagedSessionRegistryTempDir(t)
		if err := os.WriteFile(filepath.Join(outside, managedSessionRegistryFilename), []byte(`{"version":1,"entries":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(store, "fs")); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadManagedSessionRegistry(store); !errors.Is(err, ErrManagedSessionRegistryCorrupt) {
			t.Fatalf("fs symlink error = %v", err)
		}
	})

	t.Run("static intermediate parent symlink", func(t *testing.T) {
		outside := realManagedSessionRegistryTempDir(t)
		if err := os.Mkdir(filepath.Join(outside, "store"), 0o700); err != nil {
			t.Fatal(err)
		}
		linkRoot := realManagedSessionRegistryTempDir(t)
		if err := os.Symlink(outside, filepath.Join(linkRoot, "linked")); err != nil {
			t.Fatal(err)
		}
		store := filepath.Join(linkRoot, "linked", "store")
		if _, err := WriteManagedSessionRegistry(store, nil, ManagedSessionRegistryWriteOptions{Bootstrap: true}); err != nil {
			t.Fatalf("static intermediate symlink bootstrap error = %v", err)
		}
		if _, err := LoadManagedSessionRegistry(store); err != nil {
			t.Fatalf("static intermediate symlink load error = %v", err)
		}
	})

	t.Run("store final symlink", func(t *testing.T) {
		outside := realManagedSessionRegistryTempDir(t)
		if err := os.Mkdir(filepath.Join(outside, "store"), 0o700); err != nil {
			t.Fatal(err)
		}
		linkRoot := realManagedSessionRegistryTempDir(t)
		store := filepath.Join(linkRoot, "store")
		if err := os.Symlink(filepath.Join(outside, "store"), store); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadManagedSessionRegistry(store); err == nil || !strings.Contains(err.Error(), "store is not a real directory") {
			t.Fatalf("store final symlink error = %v", err)
		}
	})
}

func TestManagedSessionRegistryRequiresAbsoluteStore(t *testing.T) {
	if _, err := LoadManagedSessionRegistry("relative"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative load error = %v", err)
	}
	if _, err := WriteManagedSessionRegistry("relative", nil, ManagedSessionRegistryWriteOptions{Bootstrap: true}); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative write error = %v", err)
	}
}

func newManagedSessionRegistryStore(t *testing.T) string {
	t.Helper()
	root := realManagedSessionRegistryTempDir(t)
	store := filepath.Join(root, "store")
	if err := os.MkdirAll(filepath.Join(store, "fs"), 0o700); err != nil {
		t.Fatal(err)
	}
	return store
}

func realManagedSessionRegistryTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func bootstrapManagedSessionRegistry(t *testing.T, store string) {
	t.Helper()
	if _, err := WriteManagedSessionRegistry(store, nil, ManagedSessionRegistryWriteOptions{Bootstrap: true}); err != nil {
		t.Fatal(err)
	}
}

func writeRawManagedSessionRegistry(t *testing.T, store string, data []byte) {
	t.Helper()
	if err := os.WriteFile(ManagedSessionRegistryPath(store), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertManagedSessionRegistryEntries(t *testing.T, got []ManagedSessionRegistryEntry, want []ManagedSessionRegistryEntry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("registry entries = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("registry entries = %#v, want %#v", got, want)
		}
	}
}

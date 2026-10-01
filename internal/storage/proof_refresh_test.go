package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGuardRefreshRejectsManifestEditAfterAcquisition(t *testing.T) {
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
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, append(data, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Refresh(context.Background(), store); err == nil {
		t.Fatal("cached proof concealed manifest mutation")
	}
}

func TestProofFileStampDetectsRewriteWithRestoredMtime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proof.json")
	if err := os.WriteFile(path, []byte("aaaa"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, supported := fileChangeTime(before); !supported {
		t.Skip("platform uses full proof reload")
	}
	// A rewrite inside the same clock tick is indistinguishable by design; that
	// case is what MetadataSettled exists to refuse. What must hold is that a
	// rewrite in any later tick is always seen, so wait for one first.
	waitForLaterChangeTick(t, filepath.Dir(path), before)
	if err := os.WriteFile(path, []byte("bbbb"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if sameProofFiles(map[string]os.FileInfo{path: before}, map[string]os.FileInfo{path: after}) {
		t.Fatal("same-size rewrite with restored mtime concealed a change")
	}
}

func TestGuardRefreshCostComparison(t *testing.T) {
	requireCacheStamp(t)
	if os.Getenv("CODEXFOLD_RETIRE_BENCH") == "" {
		t.Skip("opt-in cost measurement")
	}
	store := t.TempDir()
	for i := range 400 {
		id := fmt.Sprintf("session-%04d", i)
		path := filepath.Join(store, "manifests", id+".json")
		writeJSONFile(t, path, manifestFixture(id, filepath.Join(store, id+".jsonl"), 4))
		dir := filepath.Join(store, "fs", "sessions", id)
		delta := writeSizedFile(t, filepath.Join(dir, "delta.jsonl"), 0)
		writeJSONFile(t, filepath.Join(dir, "state.json"), stateFixture(t, id, path, 4, delta, "", ""))
	}
	guard, err := AcquireManagedSessionDeletionGuard(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	for _, full := range []bool{true, false} {
		start := time.Now()
		for range 50 {
			if full {
				_, err = ManagedSessionReferences(context.Background(), store)
			} else {
				_, err = guard.Refresh(context.Background(), store)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("full_reload=%t sessions=400 iterations=50 per_refresh=%s", full, time.Since(start)/50)
	}
}

func TestGuardRefreshRejectsDirectoryChangedDuringReload(t *testing.T) {
	requireCacheStamp(t)
	for _, initiallyPresent := range []bool{false, true} {
		t.Run(fmt.Sprint(initiallyPresent), func(t *testing.T) {
			store := t.TempDir()
			root := filepath.Join(store, "fs", "sessions")
			if initiallyPresent {
				if err := os.MkdirAll(root, 0700); err != nil {
					t.Fatal(err)
				}
			}
			old := reloadManagedSessionReferences
			t.Cleanup(func() { reloadManagedSessionReferences = old })
			reloadManagedSessionReferences = func(context.Context, string) ([]ManagedSessionReference, error) {
				return nil, os.MkdirAll(filepath.Join(root, "new-session"), 0700)
			}
			guard := &ManagedSessionDeletionGuard{}
			if _, err := guard.Refresh(context.Background(), store); err == nil {
				t.Fatal("cached a scan that missed a concurrent publication")
			}
			if guard.sessions.valid {
				t.Fatal("failed refresh left a valid cache")
			}
		})
	}
}

func TestGuardRefreshCancellationCannotUseCache(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&ManagedSessionDeletionGuard{}).Refresh(ctx, t.TempDir()); err != context.Canceled {
		t.Fatalf("got %v", err)
	}
}

func TestGuardRefreshRejectsClosedGuard(t *testing.T) {
	guard := &ManagedSessionDeletionGuard{}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Refresh(context.Background(), t.TempDir()); err == nil {
		t.Fatal("proof remained usable after releasing locks")
	}
}

func TestGuardRefreshDoesNotReloadWhenTheSessionSetIsUnchanged(t *testing.T) {
	requireCacheStamp(t)
	// Settling is covered on its own below; here the stamp only has to be
	// eligible for reuse as soon as it exists.
	setMetadataSettleWindow(t, 0)
	store := t.TempDir()
	sessions := filepath.Join(store, "fs", "sessions")
	if err := os.MkdirAll(filepath.Join(sessions, "session"), 0o700); err != nil {
		t.Fatal(err)
	}

	reloads := 0
	old := reloadManagedSessionReferences
	reloadManagedSessionReferences = func(ctx context.Context, storeDir string) ([]ManagedSessionReference, error) {
		reloads++
		return nil, nil
	}
	t.Cleanup(func() { reloadManagedSessionReferences = old })

	guard := &ManagedSessionDeletionGuard{}
	// The first refresh has nothing to compare against and must reload.
	if _, err := guard.Refresh(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if reloads != 1 {
		t.Fatalf("first refresh reloads = %d, want 1", reloads)
	}

	// Reclamation asks this once per loose object. Reloading every manifest to
	// answer it is what made reclaiming a large store take hours.
	for range 500 {
		if _, err := guard.Refresh(context.Background(), store); err != nil {
			t.Fatal(err)
		}
	}
	if reloads != 1 {
		t.Fatalf("an unchanged session set reloaded %d times, want 1", reloads)
	}
}

func TestGuardRefreshStillReloadsWhenASessionAppears(t *testing.T) {
	requireCacheStamp(t)
	setMetadataSettleWindow(t, 0)
	store := t.TempDir()
	sessions := filepath.Join(store, "fs", "sessions")
	if err := os.MkdirAll(filepath.Join(sessions, "first"), 0o700); err != nil {
		t.Fatal(err)
	}

	reloads := 0
	old := reloadManagedSessionReferences
	reloadManagedSessionReferences = func(ctx context.Context, storeDir string) ([]ManagedSessionReference, error) {
		reloads++
		return nil, nil
	}
	t.Cleanup(func() { reloadManagedSessionReferences = old })

	guard := &ManagedSessionDeletionGuard{}
	if _, err := guard.Refresh(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Refresh(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if reloads != 1 {
		t.Fatalf("reloads before the change = %d, want 1", reloads)
	}

	// A session appearing changes the directory that holds it, and that has to
	// bring the full proof back.
	if err := os.MkdirAll(filepath.Join(sessions, "second"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Refresh(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if reloads != 2 {
		t.Fatalf("a new managed session did not force a reload: reloads = %d, want 2", reloads)
	}
}

func TestGuardRefreshNeverReusesAStampInsideItsClockTick(t *testing.T) {
	requireCacheStamp(t)
	// A window no write in this test can fall outside of, so every stamp taken
	// here is one a same-tick rewrite could still hide behind. On a filesystem
	// with coarse change times that is exactly what happens: ext4 under a
	// 1000 Hz kernel gave a manifest rewritten within the same millisecond the
	// same ctime, and the cached proof reported the old manifest as current.
	setMetadataSettleWindow(t, time.Hour)
	store := t.TempDir()
	if err := os.MkdirAll(filepath.Join(store, "fs", "sessions", "session"), 0o700); err != nil {
		t.Fatal(err)
	}

	reloads := 0
	old := reloadManagedSessionReferences
	reloadManagedSessionReferences = func(ctx context.Context, storeDir string) ([]ManagedSessionReference, error) {
		reloads++
		return nil, nil
	}
	t.Cleanup(func() { reloadManagedSessionReferences = old })

	guard := &ManagedSessionDeletionGuard{}
	for range 5 {
		if _, err := guard.Refresh(context.Background(), store); err != nil {
			t.Fatal(err)
		}
	}
	if reloads != 5 {
		t.Fatalf("an unsettled stamp was reused: reloads = %d, want 5", reloads)
	}
	if guard.sessions.valid || guard.proofFiles != nil {
		t.Fatal("an unsettled stamp was kept for reuse")
	}
}

func TestMetadataSettledMeasuresChangeTimeAgainstTheRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proof.json")
	if err := os.WriteFile(path, []byte("aaaa"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, ok := fileChangeInstant(info)
	if !ok {
		if MetadataSettled(time.Now().Add(time.Hour), info) {
			t.Fatal("a platform without change times reported settled metadata")
		}
		t.Skip("platform has no change-time field")
	}
	setMetadataSettleWindow(t, time.Second)
	if MetadataSettled(changed.Add(time.Second), info) {
		t.Fatal("metadata exactly one window old must not be trusted yet")
	}
	if !MetadataSettled(changed.Add(time.Second+time.Nanosecond), info) {
		t.Fatal("metadata older than the window must be trusted")
	}
	if !MetadataSettled(changed.Add(time.Second+time.Nanosecond), info, nil) {
		t.Fatal("an absent file must not unsettle the rest")
	}
}

func setMetadataSettleWindow(t *testing.T, window time.Duration) {
	t.Helper()
	old := MetadataSettleWindow
	MetadataSettleWindow = window
	t.Cleanup(func() { MetadataSettleWindow = old })
}

// waitForLaterChangeTick returns once a file created in directory receives a
// change time later than reference's, which proves the filesystem clock has
// moved on to a tick a subsequent write cannot share with reference.
func waitForLaterChangeTick(t *testing.T, directory string, reference os.FileInfo) {
	t.Helper()
	before, ok := fileChangeInstant(reference)
	if !ok {
		t.Skip("platform has no change-time field")
	}
	probe := filepath.Join(directory, ".tick-probe")
	defer os.Remove(probe)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// Created afresh each time, so its change time is the clock's now
		// rather than whatever a truncation of an empty file does or doesn't
		// update.
		if err := os.Remove(probe); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.WriteFile(probe, nil, 0600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(probe)
		if err != nil {
			t.Fatal(err)
		}
		if changed, _ := fileChangeInstant(info); changed.After(before) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Skip("filesystem change time did not advance within five seconds")
}

func requireCacheStamp(t *testing.T) {
	t.Helper()
	store := t.TempDir()
	if err := os.MkdirAll(filepath.Join(store, "fs", "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	if !stampManagedSessions(store).valid {
		t.Skip("platform uses full proof reload")
	}
}

func TestGuardRefreshReloadsWhenTheSessionDirectoryCannotBeStamped(t *testing.T) {
	store := t.TempDir()
	reloads := 0
	old := reloadManagedSessionReferences
	reloadManagedSessionReferences = func(ctx context.Context, storeDir string) ([]ManagedSessionReference, error) {
		reloads++
		return nil, nil
	}
	t.Cleanup(func() { reloadManagedSessionReferences = old })

	// No sessions directory at all: there is nothing to compare, so the cheap
	// path must never claim the set is unchanged.
	guard := &ManagedSessionDeletionGuard{}
	for range 3 {
		if _, err := guard.Refresh(context.Background(), store); err != nil {
			t.Fatal(err)
		}
	}
	if reloads != 3 {
		t.Fatalf("an unstampable directory was treated as unchanged: reloads = %d, want 3", reloads)
	}
}

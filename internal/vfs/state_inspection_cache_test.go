package vfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/storage"
)

func TestStateInspectionCacheTracksPublishedStateAndCheckpointChanges(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session, err := OpenSession(context.Background(), SessionOptions{Root: root, ManifestPath: filepath.Join(root, "manifests", manifest.Session.ID+".json"), Manifest: manifest, Reader: reader, NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "fs", "sessions", manifest.Session.ID, "state.json")
	// The session was published a moment ago, so under the real settle window
	// nothing would be cached yet. This test is about what a cached entry
	// notices, so let the entry exist immediately.
	oldWindow := storage.MetadataSettleWindow
	storage.MetadataSettleWindow = 0
	t.Cleanup(func() { storage.MetadataSettleWindow = oldWindow })
	cache := &StateInspectionCache{}
	original, err := cache.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	// The production reload polls every ten seconds. An unchanged session
	// should not be fully decoded on every poll just because that interval has
	// elapsed; a changed publication must still invalidate its metadata stamp.
	cached := cache.entries[path]
	cached.at = time.Now().Add(-30 * time.Second)
	cache.entries[path] = cached
	if again, err := cache.Inspect(path); err != nil || again != original {
		t.Fatalf("unchanged inspection: %v", err)
	}
	if !cache.entries[path].at.Equal(cached.at) {
		t.Fatal("unchanged state was decoded again before cache expiry")
	}
	next := session.State()
	next.Generation++
	if err := publishSessionState(path, next); err != nil {
		t.Fatal(err)
	}
	if got, err := cache.Inspect(path); err != nil || got.Generation != next.Generation {
		t.Fatalf("cached old publication: %+v %v", got, err)
	}
	catalog, err := loadStateCatalog(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(filepath.Dir(path), stateGenerationsDirectoryName, catalog.Checkpoint)
	data, err := os.ReadFile(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checkpoint, append(data, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Inspect(path); err == nil {
		t.Fatal("checkpoint corruption was concealed by cache")
	}
}

func TestStateInspectionCacheDoesNotKeepAStampInsideItsClockTick(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	if _, err := OpenSession(context.Background(), SessionOptions{Root: root, ManifestPath: filepath.Join(root, "manifests", manifest.Session.ID+".json"), Manifest: manifest, Reader: reader, NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "fs", "sessions", manifest.Session.ID, "state.json")
	oldWindow := storage.MetadataSettleWindow
	storage.MetadataSettleWindow = time.Hour
	t.Cleanup(func() { storage.MetadataSettleWindow = oldWindow })
	cache := &StateInspectionCache{}
	if _, err := cache.Inspect(path); err != nil {
		t.Fatal(err)
	}
	// Just published, so a rewrite now could share the publication's clock
	// tick and leave every stamp field equal. The state is still returned; it
	// just must not be reused on the strength of that stamp.
	if _, cached := cache.entries[path]; cached {
		t.Fatal("an inspection whose stamp had not settled was cached")
	}
}

func TestStateInspectionCacheComparison(t *testing.T) {
	root := os.Getenv("CODEXFOLD_OBSERVER_SAMPLE_STORE")
	if root == "" {
		t.Skip("opt-in read-only observer comparison")
	}
	cache := &StateInspectionCache{}
	if _, _, err := cache.Discover(root); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"uncached", "warm-cache", "aged-thirty-seconds", "uncached-after"} {
		start := time.Now()
		count := 0
		for range 10 {
			var states []SessionState
			var issues []SessionStateIssue
			var err error
			if mode == "warm-cache" || mode == "aged-thirty-seconds" {
				if mode == "aged-thirty-seconds" {
					cache.mu.Lock()
					for path, entry := range cache.entries {
						entry.at = time.Now().Add(-30 * time.Second)
						cache.entries[path] = entry
					}
					cache.mu.Unlock()
				}
				states, issues, err = cache.Discover(root)
			} else {
				states, issues, err = DiscoverSessionStatesDetailedReadOnly(root)
			}
			if err != nil || len(issues) != 0 {
				t.Fatalf("read-only observer: %v issues=%d", err, len(issues))
			}
			count = len(states)
		}
		t.Logf("mode=%s sessions=%d per_scan=%s", mode, count, time.Since(start)/10)
	}
}

func TestDiscoveryDistinguishesLivePublicationFromAbandonedStaging(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "fs", "sessions")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireInitialSessionLease(context.Background(), filepath.Join(parent, initialSessionLockName("sample")))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, err := os.MkdirTemp(parent, initialSessionStagingNamePrefix("sample")); err != nil {
		t.Fatal(err)
	}
	_, issues, err := DiscoverSessionStatesDetailedReadOnly(root)
	if err != nil || len(issues) != 0 {
		t.Fatalf("live publication reported as abandoned: %+v %v", issues, err)
	}
	if err := unlockWriterFile(lease); err != nil {
		t.Fatal(err)
	}
	_, issues, err = DiscoverSessionStatesDetailedReadOnly(root)
	if err != nil || len(issues) != 1 || issues[0].Kind != SessionStateIssueStaging {
		t.Fatalf("abandoned publication not detected: %+v %v", issues, err)
	}
}

func TestStateInspectionCacheDoesNotHideSameSizePrimaryRewrite(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	if _, err := OpenSession(context.Background(), SessionOptions{Root: root, ManifestPath: filepath.Join(root, "manifests", manifest.Session.ID+".json"), Manifest: manifest, Reader: reader, NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "fs", "sessions", manifest.Session.ID, "state.json")
	cache := &StateInspectionCache{}
	if _, err := cache.Inspect(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[0] = '!'
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Inspect(path); err == nil {
		t.Fatal("same-size corrupted state was cached")
	}
}

package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestScanClassifiesManagedStorageAndDeduplicatesHardLinks(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	nativeRoot := filepath.Join(root, "native")

	nativeA := writeSizedFile(t, filepath.Join(nativeRoot, "managed-a.jsonl"), 13)
	nativeB := writeSizedFile(t, filepath.Join(nativeRoot, "managed-b.jsonl"), 20)
	nativeC := writeSizedFile(t, filepath.Join(nativeRoot, "archived.jsonl"), 50)

	manifestA := filepath.Join(store, "manifests", "managed-a.json")
	manifestB := filepath.Join(store, "manifests", "managed-b.json")
	manifestC := filepath.Join(store, "manifests", "archived.json")
	writeJSONFile(t, manifestA, manifestFixture("managed-a", nativeA, 100))
	writeJSONFile(t, manifestB, manifestFixture("managed-b", nativeB, 20))
	writeJSONFile(t, manifestC, manifestFixture("archived", nativeC, 50))

	writeSizedFile(t, filepath.Join(store, "objects", "aa", "aaaaaaaa.zst"), 3)
	writeSizedFile(t, filepath.Join(store, "packs", "gen-current", "pack-000001.pack"), 4)
	writeSizedFile(t, filepath.Join(store, "packs", "gen-current", "index.json"), 2)
	writeSizedFile(t, filepath.Join(store, "packs", "gen-old", "pack-000001.pack"), 5)
	writeSizedFile(t, filepath.Join(store, "packs", "gen-old", "index.json"), 2)
	writeBytesFile(t, filepath.Join(store, "packs", "CURRENT"), []byte("gen-current\n"))

	snapshotA := filepath.Join(store, "fs", "snapshots", "managed-a", "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(snapshotA), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(nativeA, snapshotA); err != nil {
		t.Fatalf("hard-link retained snapshot: %v", err)
	}
	snapshotB := writeSizedFile(t, filepath.Join(store, "fs", "snapshots", "managed-b", "native.jsonl"), 20)

	sessionA := filepath.Join(store, "fs", "sessions", "managed-a")
	deltaA := writeSizedFile(t, filepath.Join(sessionA, "delta.jsonl"), 4)
	backingA := writeSizedFile(t, filepath.Join(sessionA, "backing-00000000000000000002.jsonl"), 17)
	writeJSONFile(t, filepath.Join(sessionA, "state.json"), stateFixture("managed-a", manifestA, 100, deltaA, backingA, snapshotA))

	sessionB := filepath.Join(store, "fs", "sessions", "managed-b")
	deltaB := writeSizedFile(t, filepath.Join(sessionB, "delta.jsonl"), 5)
	writeSizedFile(t, filepath.Join(sessionB, "delta-00000000000000000001.jsonl"), 6)
	writeJSONFile(t, filepath.Join(sessionB, "state.json"), stateFixture("managed-b", manifestB, 20, deltaB, "", snapshotB))

	scratch := writeSizedFile(t, filepath.Join(sessionB, ".compact-00000000000000000001.jsonl"), 25)
	stateTemp := writeSizedFile(t, filepath.Join(sessionB, ".state-compact-00000000000000000002.tmp"), 7)
	writeJSONLine(t, filepath.Join(sessionB, "journal.jsonl"), map[string]any{
		"operation_id": "compact-00000000000000000001",
		"phase":        "prepared",
		"temp_path":    stateTemp,
		"native":       map[string]any{"path": scratch},
	})
	writeSizedFile(t, filepath.Join(sessionB, ".backing-orphan.tmp"), 8)

	writeSizedFile(t, filepath.Join(store, "fs", "fallbacks", "managed-a", "fallback-current.jsonl"), 9)
	writeSizedFile(t, filepath.Join(store, "fs", "retired", "managed-a-1", "state.json"), 11)
	writeSizedFile(t, filepath.Join(store, "fs", "retired", "managed-a-1", "retained-native", "native.jsonl"), 12)

	inventory, err := Scan(context.Background(), Options{StoreDir: store})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if inventory.LogicalSessionBytes != 92 {
		t.Fatalf("logical session bytes = %d, want 92", inventory.LogicalSessionBytes)
	}
	assertUsage(t, "loose objects", inventory.UniqueLooseObjects, 1, 3)
	assertUsage(t, "current packs", inventory.Packs, 2, 6)
	assertUsage(t, "native sources", inventory.NativeSources, 3, 83)
	assertUsage(t, "retained snapshots", inventory.RetainedSnapshots, 2, 33)
	assertUsage(t, "current fallbacks", inventory.CurrentFallbacks, 1, 9)
	assertUsage(t, "active deltas", inventory.ActiveDeltas, 1, 5)
	assertUsage(t, "writable backings", inventory.WritableBackings, 1, 17)
	assertUsage(t, "old generations", inventory.OldGenerations, 4, 17)
	assertUsage(t, "retirement state", inventory.RetirementState, 2, 23)
	assertUsage(t, "journal recovery", inventory.JournalRecovery, 2, 32)
	assertUsage(t, "unowned temporary", inventory.UnownedTemporary, 1, 8)

	if inventory.TotalPhysicalBytes <= 0 {
		t.Fatalf("total physical bytes = %d", inventory.TotalPhysicalBytes)
	}
	if inventory.HardlinkAliases != 1 {
		t.Fatalf("hard-link aliases = %d, want 1", inventory.HardlinkAliases)
	}
}

func TestScanRejectsPathsOutsideTheDeclaredStore(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	manifest := filepath.Join(store, "manifests", "session.json")
	native := writeSizedFile(t, filepath.Join(root, "native.jsonl"), 4)
	writeJSONFile(t, manifest, manifestFixture("session", native, 4))
	writeJSONFile(t, filepath.Join(store, "fs", "sessions", "session", "state.json"), stateFixture(
		"session",
		manifest,
		4,
		filepath.Join(root, "unsafe-delta.jsonl"),
		"",
		native,
	))

	if _, err := Scan(context.Background(), Options{StoreDir: store}); err == nil {
		t.Fatal("Scan should reject a managed data path outside its session directory")
	}
}

func TestScannerRecognizesCanonicalNestedMount(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	canonical := filepath.Join(filepath.Dir(store), "canonical-store")
	s := &scanner{
		store:          store,
		canonicalStore: canonical,
		nestedMounts: map[string]struct{}{
			filepath.Join(canonical, "nested", "mount"): {},
		},
	}
	if !s.isNestedMount(filepath.Join(store, "nested", "mount")) {
		t.Fatal("canonical nested mount was not recognized")
	}
	if s.isNestedMount(filepath.Join(store, "nested")) || s.isNestedMount(store) {
		t.Fatal("ordinary storage directory was treated as a mount")
	}
}

func assertUsage(t *testing.T, name string, usage FileUsage, files int, apparentBytes int64) {
	t.Helper()
	if usage.Files != files || usage.ApparentBytes != apparentBytes {
		t.Fatalf("%s usage = %#v, want files=%d apparent_bytes=%d", name, usage, files, apparentBytes)
	}
}

func manifestFixture(sessionID string, rolloutPath string, sourceBytes int64) map[string]any {
	return map[string]any{
		"session": map[string]any{"id": sessionID, "rollout_path": rolloutPath},
		"source":  map[string]any{"bytes": sourceBytes},
	}
}

func stateFixture(sessionID string, manifestPath string, baseBytes int64, deltaPath string, backingPath string, snapshotPath string) map[string]any {
	return map[string]any{
		"version":         1,
		"session_id":      sessionID,
		"generation":      2,
		"manifest_path":   manifestPath,
		"base_bytes":      baseBytes,
		"base_sha256":     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"delta_path":      deltaPath,
		"backing_path":    backingPath,
		"native_snapshot": map[string]any{"path": snapshotPath, "bytes": baseBytes, "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
}

func writeJSONFile(t *testing.T, path string, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeJSONLine(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeSizedFile(t *testing.T, path string, size int64) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, size)
	for i := range buffer {
		buffer[i] = byte('a' + i%26)
	}
	if _, err := file.Write(buffer); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeBytesFile(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCollectBoundsGenerationsRetiredStateAndTemporaryFiles(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	now := time.Unix(2_000_000, 0)
	old := now.Add(-2 * time.Hour)

	writeBytesFile(t, filepath.Join(store, "packs", "CURRENT"), []byte("gen-3\n"))
	for _, generation := range []string{"gen-0", "gen-1", "gen-2", "gen-3"} {
		path := writeSizedFile(t, filepath.Join(store, "packs", generation, "pack-000001.pack"), 16)
		setModTime(t, path, old.Add(time.Duration(generation[len(generation)-1]-'0')*time.Minute))
		writeJSONFile(t, filepath.Join(store, "packs", generation, "index.json"), map[string]any{"generation": generation})
	}
	leased, err := AcquireLease(filepath.Join(store, "packs", "gen-0", "leases"), "resolver")
	if err != nil {
		t.Fatal(err)
	}

	manifestRoot := filepath.Join(store, "manifests", "generations", "session")
	for generation := 1; generation <= 3; generation++ {
		writeJSONFile(t, filepath.Join(manifestRoot, string(rune('0'+generation))+".json"), manifestFixture("session", filepath.Join(store, "native.jsonl"), int64(generation*10)))
	}
	sessionDir := filepath.Join(store, "fs", "sessions", "session")
	currentDelta := writeSizedFile(t, filepath.Join(sessionDir, "delta-00000000000000000003.jsonl"), 3)
	oldDelta := writeSizedFile(t, filepath.Join(sessionDir, "delta-00000000000000000001.jsonl"), 7)
	oldBacking := writeSizedFile(t, filepath.Join(sessionDir, "backing-00000000000000000002.jsonl"), 9)
	writeJSONFile(t, filepath.Join(sessionDir, "state.json"), stateFixture(
		"session", filepath.Join(manifestRoot, "3.json"), 30, currentDelta, "", "",
	))

	for index := 1; index <= 3; index++ {
		directory := filepath.Join(store, "fs", "retired", "retired-"+string(rune('0'+index)))
		writeJSONFile(t, filepath.Join(directory, "state.json"), map[string]any{"session_id": "session"})
		setModTime(t, directory, old.Add(time.Duration(index)*time.Minute))
	}
	oldTemp := writeSizedFile(t, filepath.Join(store, "fs", "sessions", "session", ".backing-abandoned.tmp"), 11)
	recentTemp := writeSizedFile(t, filepath.Join(store, "fs", "sessions", "session", ".state-recent.tmp"), 13)
	setModTime(t, oldTemp, old)
	setModTime(t, recentTemp, now.Add(-10*time.Minute))
	setModTime(t, oldDelta, old)
	setModTime(t, oldBacking, old)

	options := GCOptions{
		StoreDir: store, TemporaryGrace: time.Hour, Now: func() time.Time { return now },
		KeepPackGenerations: 2, KeepManifestGenerations: 2, KeepRetiredPerSession: 1,
	}
	dry, err := Collect(context.Background(), options)
	if err != nil {
		t.Fatalf("Collect dry-run: %v", err)
	}
	if !dry.DryRun || dry.CandidateCount != 7 || dry.RemovedCount != 0 || dry.ProjectedReclaimableBytes <= 0 || dry.ActualReclaimedBytes != 0 {
		t.Fatalf("unexpected dry-run result: %#v", dry)
	}
	for _, path := range []string{filepath.Join(store, "packs", "gen-1"), oldDelta, oldBacking, oldTemp, filepath.Join(manifestRoot, "1.json")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("dry-run removed %s: %v", path, err)
		}
	}

	options.Apply = true
	applied, err := Collect(context.Background(), options)
	if err != nil {
		t.Fatalf("Collect apply: %v", err)
	}
	if applied.RemovedCount != dry.CandidateCount || applied.ActualReclaimedBytes <= 0 {
		t.Fatalf("unexpected apply result: %#v", applied)
	}
	for _, path := range []string{filepath.Join(store, "packs", "gen-3"), filepath.Join(store, "packs", "gen-2"), filepath.Join(store, "packs", "gen-0"), currentDelta, filepath.Join(manifestRoot, "3.json"), filepath.Join(manifestRoot, "2.json"), filepath.Join(store, "fs", "retired", "retired-3"), recentTemp} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained path missing %s: %v", path, err)
		}
	}

	if err := leased.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Collect(context.Background(), options)
	if err != nil {
		t.Fatalf("Collect after lease close: %v", err)
	}
	if second.RemovedCount != 1 {
		t.Fatalf("closed leased generation was not collected: %#v", second)
	}
	third, err := Collect(context.Background(), options)
	if err != nil || third.RemovedCount != 0 {
		t.Fatalf("repeated Collect is not idempotent: %#v err=%v", third, err)
	}
}

func TestCollectKeepsSoleRecoveryStateAndJournalOwnedFiles(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	now := time.Unix(3_000_000, 0)
	manifest := filepath.Join(store, "manifests", "generations", "session", "1.json")
	writeJSONFile(t, manifest, manifestFixture("session", filepath.Join(store, "native.jsonl"), 5))
	sessionDir := filepath.Join(store, "fs", "sessions", "session")
	delta := writeSizedFile(t, filepath.Join(sessionDir, "delta.jsonl"), 5)
	scratch := writeSizedFile(t, filepath.Join(sessionDir, ".compact-00000000000000000001.jsonl"), 5)
	writeJSONFile(t, filepath.Join(sessionDir, "state.json"), stateFixture("session", manifest, 0, delta, "", ""))
	writeJSONLine(t, filepath.Join(sessionDir, "journal.jsonl"), map[string]any{
		"operation_id": "compact-1", "phase": "prepared", "native": map[string]any{"path": scratch},
	})
	writeJSONFile(t, filepath.Join(store, "fs", "retired", "only", "state.json"), map[string]any{"session_id": "session"})
	writeSizedFile(t, filepath.Join(store, "packs", "gen-only", "pack-000001.pack"), 5)
	setModTime(t, scratch, now.Add(-24*time.Hour))

	result, err := Collect(context.Background(), GCOptions{StoreDir: store, Apply: true, TemporaryGrace: time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.RemovedCount != 0 {
		t.Fatalf("sole recovery state was removed: %#v", result)
	}
	for _, path := range []string{manifest, delta, scratch, filepath.Join(store, "fs", "retired", "only"), filepath.Join(store, "packs", "gen-only")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected path missing %s: %v", path, err)
		}
	}
}

func TestCollectKeepsTruePreviousManifestAndRemovesAbandonedFuture(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	manifestRoot := filepath.Join(store, "manifests", "generations", "session")
	for generation := 1; generation <= 3; generation++ {
		writeJSONFile(t, filepath.Join(manifestRoot, string(rune('0'+generation))+".json"), manifestFixture("session", filepath.Join(store, "native.jsonl"), int64(generation)))
	}
	sessionDir := filepath.Join(store, "fs", "sessions", "session")
	delta := writeSizedFile(t, filepath.Join(sessionDir, "delta-00000000000000000002.jsonl"), 2)
	state := stateFixture("session", filepath.Join(manifestRoot, "2.json"), 2, delta, "", "")
	state["generation"] = 2
	writeJSONFile(t, filepath.Join(sessionDir, "state.json"), state)
	writeJSONLine(t, filepath.Join(sessionDir, "journal.jsonl"), map[string]any{"operation_id": "compact-2", "phase": "prepared"})

	blocked, err := Collect(context.Background(), GCOptions{StoreDir: store, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if blocked.RemovedCount != 0 {
		t.Fatalf("pending compaction allowed manifest cleanup: %#v", blocked)
	}
	writeJSONLine(t, filepath.Join(sessionDir, "journal.jsonl"), map[string]any{"operation_id": "compact-2", "phase": "rolled-back"})
	collected, err := Collect(context.Background(), GCOptions{StoreDir: store, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if collected.RemovedCount != 1 {
		t.Fatalf("abandoned future manifest was not collected: %#v", collected)
	}
	if _, err := os.Stat(filepath.Join(manifestRoot, "3.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned future manifest remains: %v", err)
	}
	for _, name := range []string{"1.json", "2.json"} {
		if _, err := os.Stat(filepath.Join(manifestRoot, name)); err != nil {
			t.Fatalf("current/previous manifest missing %s: %v", name, err)
		}
	}
}

func TestCollectReportsZeroPhysicalReclamationForRemainingHardLink(t *testing.T) {
	store := t.TempDir()
	keep := writeSizedFile(t, filepath.Join(store, "keep.bin"), 4096)
	temporary := filepath.Join(store, ".backing-abandoned.tmp")
	if err := os.Link(keep, temporary); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	setModTime(t, temporary, old)
	result, err := Collect(context.Background(), GCOptions{StoreDir: store, Apply: true, TemporaryGrace: time.Hour, Now: time.Now})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.RemovedCount != 1 || result.ProjectedReclaimableBytes != 0 || result.ActualReclaimedBytes != 0 {
		t.Fatalf("hard-link reclamation was overstated: %#v", result)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("retained hard link missing: %v", err)
	}
	if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary hard link remains: %v", err)
	}
}

func setModTime(t *testing.T, path string, modTime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

func writeMinimalState(t *testing.T, path string, sessionID string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"session_id": sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

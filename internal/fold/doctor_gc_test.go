package fold

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDoctorDetectsReferencedObjectCorruption(t *testing.T) {
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	sourcePath := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(sourcePath, []byte("{\"value\":\"large-field-value\"}\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if _, err := Fold(context.Background(), Session{ID: "doctor", RolloutPath: sourcePath, Archived: true}, FoldOptions{
		StoreDir: storeDir, Apply: true, FieldThreshold: 4,
	}); err != nil {
		t.Fatalf("Fold returned error: %v", err)
	}
	clean, err := Doctor(context.Background(), storeDir)
	if err != nil {
		t.Fatalf("Doctor returned error: %v", err)
	}
	if clean.IssueCount != 0 || clean.ManifestCount != 1 {
		t.Fatalf("unexpected clean doctor result: %#v", clean)
	}
	if clean.Storage.LogicalSessionBytes == 0 || clean.Storage.TotalPhysicalBytes == 0 || clean.StorageLimits.MaxPhysicalBytes == 0 || clean.AvailableBytes == 0 {
		t.Fatalf("doctor storage accounting is incomplete: %#v", clean)
	}
	manifest, err := LoadManifest(storeDir, "doctor")
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	store := NewObjectStore(storeDir)
	if err := os.WriteFile(store.ObjectPath(manifest.Parts[0].Object.SHA256), []byte("corrupt"), 0o644); err != nil {
		t.Fatalf("corrupt object: %v", err)
	}
	broken, err := Doctor(context.Background(), storeDir)
	if err != nil {
		t.Fatalf("Doctor returned top-level error: %v", err)
	}
	if broken.IssueCount == 0 {
		t.Fatalf("doctor did not report object corruption: %#v", broken)
	}
}

func TestGCDryRunAndApplyRemoveOnlyUnreferencedObjects(t *testing.T) {
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	sourcePath := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(sourcePath, []byte("{\"value\":\"large-field-value\"}\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if _, err := Fold(context.Background(), Session{ID: "gc", RolloutPath: sourcePath, Archived: true}, FoldOptions{
		StoreDir: storeDir, Apply: true, FieldThreshold: 4,
	}); err != nil {
		t.Fatalf("Fold returned error: %v", err)
	}
	store := NewObjectStore(storeDir)
	orphan, _, err := store.Put([]byte("unreferenced object"), true)
	if err != nil {
		t.Fatalf("write orphan: %v", err)
	}
	dry, err := GC(context.Background(), storeDir, false)
	if err != nil {
		t.Fatalf("GC dry-run returned error: %v", err)
	}
	if !dry.DryRun || dry.OrphanCount != 1 || dry.RemovedCount != 0 {
		t.Fatalf("unexpected dry-run result: %#v", dry)
	}
	if _, err := os.Stat(store.ObjectPath(orphan.SHA256)); err != nil {
		t.Fatalf("dry-run removed orphan: %v", err)
	}
	applied, err := GC(context.Background(), storeDir, true)
	if err != nil {
		t.Fatalf("GC apply returned error: %v", err)
	}
	if applied.RemovedCount != 1 {
		t.Fatalf("unexpected apply result: %#v", applied)
	}
	if _, err := os.Stat(store.ObjectPath(orphan.SHA256)); !os.IsNotExist(err) {
		t.Fatalf("orphan still exists after apply: %v", err)
	}
	verified, err := Doctor(context.Background(), storeDir)
	if err != nil || verified.IssueCount != 0 {
		t.Fatalf("referenced objects damaged by GC: result=%#v err=%v", verified, err)
	}
}

func TestGCIncludesBoundedGenerationAndTemporaryCleanup(t *testing.T) {
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	sourcePath := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(sourcePath, []byte("{\"value\":\"storage-gc\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Fold(context.Background(), Session{ID: "session", RolloutPath: sourcePath, Archived: true}, FoldOptions{StoreDir: storeDir, Apply: true, FieldThreshold: 4}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(storeDir, "packs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "packs", "CURRENT"), []byte("gen-3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, generation := range []string{"gen-1", "gen-2", "gen-3"} {
		directory := filepath.Join(storeDir, "packs", generation)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "pack-000001.pack"), []byte(generation), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(directory, old.Add(time.Duration(generation[len(generation)-1]-'0')*time.Minute), old.Add(time.Duration(generation[len(generation)-1]-'0')*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	temporary := filepath.Join(storeDir, ".backing-abandoned.tmp")
	if err := os.WriteFile(temporary, []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(temporary, old, old); err != nil {
		t.Fatal(err)
	}

	result, err := GC(context.Background(), storeDir, true)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if result.Storage.RemovedCount != 2 || result.ActualReclaimedBytes <= 0 {
		t.Fatalf("bounded storage was not collected: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(storeDir, "packs", "gen-1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old pack generation remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(storeDir, "packs", "gen-2")); err != nil {
		t.Fatalf("previous pack generation was removed: %v", err)
	}
	if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned temporary remains: %v", err)
	}
}

func TestDoctorAndGCKeepGenerationManifestObjects(t *testing.T) {
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	sourcePath := filepath.Join(root, "generation.jsonl")
	if err := os.WriteFile(sourcePath, []byte("{\"value\":\"generation-only-field\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(storeDir, "manifests", "generations", "session", "2.json")
	if _, err := Fold(context.Background(), Session{ID: "session", RolloutPath: sourcePath, Archived: true}, FoldOptions{StoreDir: storeDir, ManifestPathOverride: manifestPath, Apply: true, FieldThreshold: 4}); err != nil {
		t.Fatalf("Fold generation: %v", err)
	}
	doctor, err := Doctor(context.Background(), storeDir)
	if err != nil || doctor.ManifestCount != 1 || doctor.IssueCount != 0 {
		t.Fatalf("generation manifest not covered by doctor: %#v err=%v", doctor, err)
	}
	gc, err := GC(context.Background(), storeDir, true)
	if err != nil || gc.OrphanCount != 0 || gc.Referenced == 0 {
		t.Fatalf("generation object treated as orphan: %#v err=%v", gc, err)
	}
}

func TestRemoveSourceRequiresGuardAndCanMaterializeAgain(t *testing.T) {
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	sourcePath := filepath.Join(root, "rollout.jsonl")
	source := []byte("{\"value\":\"large-field-value\"}\n")
	if err := os.WriteFile(sourcePath, source, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	_, err := Fold(context.Background(), Session{ID: "active", RolloutPath: sourcePath}, FoldOptions{
		StoreDir: storeDir, Apply: true, RemoveSource: true, FieldThreshold: 4,
	})
	if err == nil {
		t.Fatalf("non-archived source removal should require --allow-active")
	}
	result, err := Fold(context.Background(), Session{ID: "archived", RolloutPath: sourcePath, Archived: true}, FoldOptions{
		StoreDir: storeDir, Apply: true, RemoveSource: true, FieldThreshold: 4,
	})
	if err != nil {
		t.Fatalf("archived Fold returned error: %v", err)
	}
	if !result.RemovedSource {
		t.Fatalf("source was not removed: %#v", result)
	}
	if _, err := os.Stat(sourcePath); !os.IsNotExist(err) {
		t.Fatalf("source still exists: %v", err)
	}
	restored, err := Unfold(context.Background(), storeDir, "archived", "", false)
	if err != nil {
		t.Fatalf("materialize returned error: %v", err)
	}
	if !restored.Verified {
		t.Fatalf("materialize not verified: %#v", restored)
	}
	got, err := os.ReadFile(sourcePath)
	if err != nil || string(got) != string(source) {
		t.Fatalf("materialized source mismatch: %q err=%v", got, err)
	}
}

func TestManifestPathRejectsUnsafeSessionID(t *testing.T) {
	storeDir := t.TempDir()
	manifest := Manifest{
		Version: ManifestVersion,
		Kind:    ManifestKind,
		Session: ManifestSession{ID: "../outside"},
		Source:  ManifestSource{SHA256: strings.Repeat("0", 64)},
	}
	if err := writeManifest(storeDir, manifest, false); err == nil {
		t.Fatalf("writeManifest should reject a session ID that escapes the store")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(storeDir), "outside.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe manifest escaped the store: %v", err)
	}
}

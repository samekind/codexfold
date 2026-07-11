package fold

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jstar0/codexfold/internal/codex"
)

func TestDoctorDetectsReferencedObjectCorruption(t *testing.T) {
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	sourcePath := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(sourcePath, []byte("{\"value\":\"large-field-value\"}\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if _, err := Fold(context.Background(), codex.Session{ID: "doctor", RolloutPath: sourcePath, Archived: true}, FoldOptions{
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
	if _, err := Fold(context.Background(), codex.Session{ID: "gc", RolloutPath: sourcePath, Archived: true}, FoldOptions{
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

func TestRemoveSourceRequiresGuardAndCanMaterializeAgain(t *testing.T) {
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	sourcePath := filepath.Join(root, "rollout.jsonl")
	source := []byte("{\"value\":\"large-field-value\"}\n")
	if err := os.WriteFile(sourcePath, source, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	_, err := Fold(context.Background(), codex.Session{ID: "active", RolloutPath: sourcePath}, FoldOptions{
		StoreDir: storeDir, Apply: true, RemoveSource: true, FieldThreshold: 4,
	})
	if err == nil {
		t.Fatalf("non-archived source removal should require --allow-active")
	}
	result, err := Fold(context.Background(), codex.Session{ID: "archived", RolloutPath: sourcePath, Archived: true}, FoldOptions{
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

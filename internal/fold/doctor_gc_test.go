package fold

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func TestGCRefusesLooseObjectWhoseDecodedBytesDoNotMatchItsDigestName(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "store")
	store := NewObjectStore(storeDir)
	actual, _, err := store.Put([]byte("actual loose bytes"), true)
	if err != nil {
		t.Fatal(err)
	}
	wanted := sha256.Sum256([]byte("different digest name"))
	fakeDigest := hex.EncodeToString(wanted[:])
	fakePath := store.ObjectPath(fakeDigest)
	if err := os.MkdirAll(filepath.Dir(fakePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(store.ObjectPath(actual.SHA256), fakePath); err != nil {
		t.Fatal(err)
	}

	if _, err := GC(context.Background(), storeDir, true); err == nil {
		t.Fatal("digest-shaped filename authorized deletion of unrelated compressed bytes")
	}
	if _, err := os.Lstat(fakePath); err != nil {
		t.Fatalf("unproved loose object was removed: %v", err)
	}
}

func TestGCRefusesLooseObjectChangedInPlaceAfterExactProof(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "store")
	store := NewObjectStore(storeDir)
	object, _, err := store.Put([]byte("stable loose object bytes"), true)
	if err != nil {
		t.Fatal(err)
	}
	path := store.ObjectPath(object.SHA256)
	compressed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = GCWithOptions(context.Background(), storeDir, GCOptions{
		Apply: true,
		BeforeObjectRemove: func(candidate string) error {
			if candidate != path {
				return nil
			}
			changed := append([]byte(nil), compressed...)
			changed[len(changed)/2] ^= 0x7f
			if err := os.WriteFile(path, changed, 0o600); err != nil {
				return err
			}
			return os.Chtimes(path, info.ModTime(), info.ModTime())
		},
	})
	if err == nil {
		t.Fatal("same-size in-place rewrite passed loose-object final proof")
	}
	if current, statErr := os.Stat(path); statErr != nil || current.Size() != int64(len(compressed)) {
		t.Fatalf("changed loose object was not preserved: info=%v err=%v", current, statErr)
	}
}

func TestGCPreservesSymlinkAtDigestShapedLooseObjectPath(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "store")
	outsideStore := NewObjectStore(filepath.Join(t.TempDir(), "outside"))
	object, _, err := outsideStore.Put([]byte("outside loose object"), true)
	if err != nil {
		t.Fatal(err)
	}
	link := NewObjectStore(storeDir).ObjectPath(object.SHA256)
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideStore.ObjectPath(object.SHA256), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := GC(context.Background(), storeDir, true); err == nil {
		t.Fatal("digest-shaped loose-object symlink was not reported as unproved")
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("digest-shaped loose-object symlink was removed: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(outsideStore.ObjectPath(object.SHA256)); err != nil {
		t.Fatalf("outside symlink target changed: %v", err)
	}
}

func TestGCPreservesValidLooseObjectOutsideCanonicalShard(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "store")
	store := NewObjectStore(storeDir)
	object, _, err := store.Put([]byte("valid object in a foreign directory"), true)
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(storeDir, "objects", "foreign", object.SHA256+".zst")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(store.ObjectPath(object.SHA256), foreign); err != nil {
		t.Fatal(err)
	}

	if _, err := GC(context.Background(), storeDir, true); err == nil {
		t.Fatal("noncanonical digest-shaped object path was accepted for deletion")
	}
	if _, err := os.Lstat(foreign); err != nil {
		t.Fatalf("foreign loose object was removed: %v", err)
	}
}

func TestGCIgnoresSymlinkedForeignObjectDirectory(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "store")
	outsideStore := NewObjectStore(filepath.Join(t.TempDir(), "outside"))
	object, _, err := outsideStore.Put([]byte("outside object behind foreign directory symlink"), true)
	if err != nil {
		t.Fatal(err)
	}
	objects := filepath.Join(storeDir, "objects")
	if err := os.MkdirAll(objects, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(objects, "foreign")
	if err := os.Symlink(filepath.Dir(outsideStore.ObjectPath(object.SHA256)), foreign); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := GC(context.Background(), storeDir, true); err != nil {
		t.Fatalf("GC followed or rejected an unvisited foreign directory symlink: %v", err)
	}
	if _, err := os.Stat(outsideStore.ObjectPath(object.SHA256)); err != nil {
		t.Fatalf("outside object changed: %v", err)
	}
}

func TestGCRetainsStorageCandidatesWithoutExactDeletionProof(t *testing.T) {
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
	if result.Storage.CandidateCount != 2 || result.Storage.RemovedCount != 0 || result.Storage.RetainedUnprovedCount != 2 || result.ActualReclaimedBytes != 0 {
		t.Fatalf("unproved storage candidates were not retained: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(storeDir, "packs", "gen-1")); err != nil {
		t.Fatalf("unproved old pack generation was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(storeDir, "packs", "gen-2")); err != nil {
		t.Fatalf("previous pack generation was removed: %v", err)
	}
	if _, err := os.Stat(temporary); err != nil {
		t.Fatalf("unproved abandoned temporary was removed: %v", err)
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

func TestGCRefusesMissingManagedCurrentManifest(t *testing.T) {
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	sourcePath := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(sourcePath, []byte("{\"value\":\"managed-manifest\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Fold(context.Background(), Session{ID: "session", RolloutPath: sourcePath, Archived: true}, FoldOptions{StoreDir: storeDir, Apply: true, FieldThreshold: 4}); err != nil {
		t.Fatal(err)
	}
	manifest, err := LoadManifest(storeDir, "session")
	if err != nil {
		t.Fatal(err)
	}
	delta := filepath.Join(storeDir, "fs", "sessions", "session", "delta.jsonl")
	if err := os.MkdirAll(filepath.Dir(delta), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(delta, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{
		"version": 1, "session_id": "session", "generation": 1,
		"manifest_path": ManifestPath(storeDir, "session"), "base_bytes": manifest.Source.Bytes,
		"base_sha256": manifest.Source.SHA256, "delta_path": delta,
	}
	stateData, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(delta), "state.json"), stateData, 0o600); err != nil {
		t.Fatal(err)
	}
	objectPath := NewObjectStore(storeDir).ObjectPath(manifest.Parts[0].Object.SHA256)
	if err := os.Remove(ManifestPath(storeDir, "session")); err != nil {
		t.Fatal(err)
	}
	if _, err := GC(context.Background(), storeDir, true); err == nil {
		t.Fatal("missing managed manifest allowed loose-object GC")
	}
	if _, err := os.Stat(objectPath); err != nil {
		t.Fatalf("managed object removed without its current manifest: %v", err)
	}
}

func TestGCRechecksManifestReferencesBeforeObjectRemoval(t *testing.T) {
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	sourcePath := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(sourcePath, []byte("{\"value\":\"referenced-before-delete\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Fold(context.Background(), Session{ID: "session", RolloutPath: sourcePath, Archived: true}, FoldOptions{StoreDir: storeDir, Apply: true, FieldThreshold: 4}); err != nil {
		t.Fatal(err)
	}
	objectStore := NewObjectStore(storeDir)
	orphan, _, err := objectStore.Put([]byte("candidate-becomes-referenced"), true)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := ManifestPath(storeDir, "session")
	changed := false
	result, err := GCWithOptions(context.Background(), storeDir, GCOptions{
		Apply: true,
		BeforeObjectRemove: func(path string) error {
			if path != objectStore.ObjectPath(orphan.SHA256) || changed {
				return nil
			}
			changed = true
			manifest, err := LoadManifestPath(manifestPath)
			if err != nil {
				return err
			}
			manifest.Source.Bytes += orphan.RawBytes
			manifest.Source.SHA256 = strings.Repeat("b", 64)
			manifest.Parts = append(manifest.Parts, Part{Kind: PartResidual, Object: orphan})
			data, err := json.Marshal(manifest)
			if err != nil {
				return err
			}
			return os.WriteFile(manifestPath, data, 0o600)
		},
	})
	if err != nil {
		t.Fatalf("GC should safely skip a newly referenced object: %v", err)
	}
	if result.RemovedCount != 0 {
		t.Fatalf("newly referenced object was counted as removed: %#v", result)
	}
	if !changed {
		t.Fatal("object deletion hook did not run")
	}
	if _, statErr := os.Stat(objectStore.ObjectPath(orphan.SHA256)); statErr != nil {
		t.Fatalf("newly referenced object was removed: %v", statErr)
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

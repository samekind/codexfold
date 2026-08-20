package fold

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// packOnlyReader stands in for the pack resolver: it serves objects that no
// longer have a loose copy. internal/fold cannot import internal/pack because
// pack imports fold, so the resolver is represented by its interface here and
// exercised for real in the cli package.
type packOnlyReader struct {
	objects map[string][]byte
}

func (r packOnlyReader) OpenObject(_ context.Context, ref ObjectRef) (io.ReadCloser, error) {
	raw, ok := r.objects[ref.SHA256]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (r packOnlyReader) HasObject(ref ObjectRef) bool {
	_, ok := r.objects[ref.SHA256]
	return ok
}

// A pack-only store keeps no loose copy of a session's objects, so manifest
// verification can only succeed through the injected resolver. The real
// refusal this fixes needs a registered managed session, which this package
// cannot construct (vfs owns that layout); that half is verified against a real
// managed store.
func TestGCVerifiesManifestObjectsThroughTheInjectedReader(t *testing.T) {
	storeDir, manifest, digests := packOnlyManagedStoreFixture(t)
	reader := packOnlyReader{objects: digests}

	for _, part := range manifest.Parts {
		if NewObjectStore(storeDir).HasObject(part.Object) {
			t.Fatalf("fixture still has a loose copy of %s", part.Object.SHA256)
		}
		if !reader.HasObject(part.Object) {
			t.Fatalf("injected reader cannot resolve %s", part.Object.SHA256)
		}
	}
	if _, err := GCWithOptions(context.Background(), storeDir, GCOptions{Reader: reader}); err != nil {
		t.Fatalf("GC with the pack resolver returned error: %v", err)
	}
}

// The reader must never widen what GC removes. A referenced object that still
// has a loose copy belongs to retire-loose, never to GC.
func TestGCWithReaderKeepsReferencedLooseObjectsAndRemovesOnlyOrphans(t *testing.T) {
	storeDir, manifest, digests := packOnlyManagedStoreFixture(t)
	store := NewObjectStore(storeDir)

	// Restore a loose copy of every referenced object, so the store is mixed.
	for _, part := range manifest.Parts {
		if _, _, err := store.Put(digests[part.Object.SHA256], true); err != nil {
			t.Fatal(err)
		}
	}
	orphan, _, err := store.Put([]byte("unreferenced object"), true)
	if err != nil {
		t.Fatal(err)
	}

	result, err := GCWithOptions(context.Background(), storeDir, GCOptions{Apply: true, Reader: packOnlyReader{objects: digests}})
	if err != nil {
		t.Fatalf("GC apply returned error: %v", err)
	}
	if result.RemovedCount != 1 {
		t.Fatalf("removed=%d, want exactly the one orphan", result.RemovedCount)
	}
	if _, err := os.Stat(store.ObjectPath(orphan.SHA256)); !os.IsNotExist(err) {
		t.Fatalf("orphan survived apply: %v", err)
	}
	for _, part := range manifest.Parts {
		if _, err := os.Stat(store.ObjectPath(part.Object.SHA256)); err != nil {
			t.Fatalf("GC removed a referenced object %s: %v", part.Object.SHA256, err)
		}
	}
}

// A genuinely unreadable manifest must still fail closed even with a reader.
func TestGCWithReaderStillRefusesACorruptManifest(t *testing.T) {
	storeDir, _, digests := packOnlyManagedStoreFixture(t)
	corrupt := filepath.Join(storeDir, "manifests", "corrupt.json")
	if err := os.MkdirAll(filepath.Dir(corrupt), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := GCWithOptions(context.Background(), storeDir, GCOptions{Reader: packOnlyReader{objects: digests}}); err == nil {
		t.Fatal("a corrupt manifest must still block GC")
	}
}

// packOnlyManagedStoreFixture folds one session, records the raw bytes of every
// referenced object, then deletes the loose copies so only an injected reader
// can resolve them.
func packOnlyManagedStoreFixture(t *testing.T) (string, Manifest, map[string][]byte) {
	t.Helper()
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	sourcePath := filepath.Join(root, "rollout.jsonl")
	source := []byte("{\"value\":\"large-field-value\"}\n{\"value\":\"large-field-value\"}\n")
	if err := os.WriteFile(sourcePath, source, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Fold(context.Background(), Session{ID: "packonly", RolloutPath: sourcePath, Archived: true}, FoldOptions{
		StoreDir: storeDir, Apply: true, FieldThreshold: 4,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, err := LoadManifest(storeDir, "packonly")
	if err != nil {
		t.Fatal(err)
	}
	store := NewObjectStore(storeDir)
	digests := make(map[string][]byte)
	for _, part := range manifest.Parts {
		stream, err := store.OpenObject(context.Background(), part.Object)
		if err != nil {
			t.Fatal(err)
		}
		raw, readErr := io.ReadAll(stream)
		closeErr := stream.Close()
		if readErr != nil || closeErr != nil {
			t.Fatal(readErr, closeErr)
		}
		digests[part.Object.SHA256] = raw
	}
	for _, part := range manifest.Parts {
		if err := os.Remove(store.ObjectPath(part.Object.SHA256)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	return storeDir, manifest, digests
}

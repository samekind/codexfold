package fold

import (
	"context"
	"os"
	"testing"
)

func TestObjectStoreReusesAndReadsExactBytes(t *testing.T) {
	store := NewObjectStore(t.TempDir())
	data := []byte("exact bytes that should be compressed and reused")
	first, reused, err := store.Put(data, true)
	if err != nil {
		t.Fatalf("first Put returned error: %v", err)
	}
	if reused {
		t.Fatalf("first object should not be reused")
	}
	second, reused, err := store.Put(data, true)
	if err != nil {
		t.Fatalf("second Put returned error: %v", err)
	}
	if !reused || second.SHA256 != first.SHA256 {
		t.Fatalf("second object was not reused: first=%#v second=%#v reused=%t", first, second, reused)
	}
	restored, err := store.Read(first)
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if string(restored) != string(data) {
		t.Fatalf("restored object differs: %q", restored)
	}
}

func TestObjectStoreDetectsCorruption(t *testing.T) {
	store := NewObjectStore(t.TempDir())
	ref, _, err := store.Put([]byte("protected bytes"), true)
	if err != nil {
		t.Fatalf("Put returned error: %v", err)
	}
	if err := os.WriteFile(store.ObjectPath(ref.SHA256), []byte("corrupt"), 0o644); err != nil {
		t.Fatalf("corrupt object: %v", err)
	}
	if _, err := store.Read(ref); err == nil {
		t.Fatalf("Read should reject a corrupt object")
	}
}

func TestObjectStorePutRejectsCorruptExistingObject(t *testing.T) {
	root := t.TempDir()
	store := NewObjectStore(root)
	data := []byte("protected bytes")
	ref, _, err := store.Put(data, true)
	if err != nil {
		t.Fatalf("Put returned error: %v", err)
	}
	if err := os.WriteFile(store.ObjectPath(ref.SHA256), []byte("corrupt"), 0o644); err != nil {
		t.Fatalf("corrupt object: %v", err)
	}

	if _, reused, err := NewObjectStore(root).Put(data, true); err == nil || reused {
		t.Fatalf("Put should reject the corrupt existing object: reused=%t err=%v", reused, err)
	}
}

func TestObjectStoreSyncPendingClearsNewObjectSet(t *testing.T) {
	store := NewObjectStore(t.TempDir())
	for _, value := range []string{"first", "second", "first"} {
		if _, _, err := store.Put([]byte(value), true); err != nil {
			t.Fatalf("Put(%q) returned error: %v", value, err)
		}
	}
	if got := store.pendingCount(); got != 2 {
		t.Fatalf("pending count = %d, want 2 unique new objects", got)
	}
	if err := store.SyncPending(context.Background()); err != nil {
		t.Fatalf("SyncPending returned error: %v", err)
	}
	if got := store.pendingCount(); got != 0 {
		t.Fatalf("pending count after sync = %d, want 0", got)
	}
}

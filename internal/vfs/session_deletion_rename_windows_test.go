//go:build windows

package vfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsDeletionRenameNoReplaceKeepsCollision(t *testing.T) {
	root := t.TempDir()
	store, err := openSessionDeletionStoreRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "quarantine", "target")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "data"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sessionDeletionRenameNoReplace(store, source, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "data"), []byte("new source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sessionDeletionRenameNoReplace(store, source, target); err == nil {
		t.Fatal("existing quarantine replaced")
	}
	for path, expected := range map[string]string{filepath.Join(source, "data"): "new source", filepath.Join(target, "data"): "source"} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != expected {
			t.Fatalf("collision changed %s: %q, %v", path, data, err)
		}
	}
}

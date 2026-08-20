package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestFSServeDryRunAcceptsMissingStoreWithoutCreatingIt(t *testing.T) {
	home := t.TempDir()
	store := filepath.Join(home, "missing-store")
	executeFS(t, []string{
		"fs", "serve",
		"--codex-home", home,
		"--store", store,
	})
	if _, err := os.Lstat(store); !os.IsNotExist(err) {
		t.Fatalf("dry-run created missing store: %v", err)
	}
}

func TestEnsureFSServeStoreCreatesDirectoryAndRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "new-store")
	if err := ensureFSServeStore(store); err != nil {
		t.Fatalf("create store: %v", err)
	}
	if info, err := os.Lstat(store); err != nil || !info.IsDir() {
		t.Fatalf("store was not created as a directory: info=%v err=%v", info, err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "store-link")
	if err := os.Symlink(outside, symlink); err != nil {
		t.Fatal(err)
	}
	if err := ensureFSServeStore(symlink); err == nil {
		t.Fatal("symlink store was accepted")
	}
}

func TestFSServeDryRunNeverRepairsSessionState(t *testing.T) {
	fixture := interruptedCanonicalMigrationFixture(t)
	if err := fixture.resolver.Close(); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(fixture.store, "fs", "sessions", "session", "state.json")
	corrupt := []byte("{\"dry_run_must_not_repair\":true}\n")
	if err := os.WriteFile(statePath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	executeFS(t, []string{
		"fs", "serve", "--canonical-namespace",
		"--codex-home", fixture.home,
		"--store", fixture.store,
		"--native-root", fixture.nativeRoot,
	})
	got, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(got, corrupt) {
		t.Fatalf("fs serve dry-run changed state: got=%q err=%v", got, err)
	}
}

package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestManagedSessionDeletionGuardRejectsSemanticallyEqualDifferentManifestBytes(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	manifestPath := filepath.Join(store, "manifests", "session.json")
	writeJSONFile(t, manifestPath, manifestFixture("session", filepath.Join(store, "native.jsonl"), 4))
	sessionDir := filepath.Join(store, "fs", "sessions", "session")
	deltaPath := writeSizedFile(t, filepath.Join(sessionDir, "delta.jsonl"), 0)
	writeJSONFile(t, filepath.Join(sessionDir, "state.json"), stateFixture(t, "session", manifestPath, 4, deltaPath, "", ""))

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(data, ' ', '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	guard, err := AcquireManagedSessionDeletionGuard(context.Background(), store)
	if guard != nil {
		_ = guard.Close()
	}
	if err == nil {
		t.Fatal("semantically equal manifest bytes not named by state allowed destructive proof")
	}
}

func TestManagedSessionDeletionGuardRejectsLegacyStateWithoutManifestIdentity(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	manifestPath := filepath.Join(store, "manifests", "session.json")
	writeJSONFile(t, manifestPath, manifestFixture("session", filepath.Join(store, "native.jsonl"), 4))
	sessionDir := filepath.Join(store, "fs", "sessions", "session")
	deltaPath := writeSizedFile(t, filepath.Join(sessionDir, "delta.jsonl"), 0)
	state := stateFixture(t, "session", manifestPath, 4, deltaPath, "", "")
	state["version"] = 1
	delete(state, "manifest_sha256")
	writeJSONFile(t, filepath.Join(sessionDir, "state.json"), state)

	guard, err := AcquireManagedSessionDeletionGuard(context.Background(), store)
	if guard != nil {
		_ = guard.Close()
	}
	if err == nil {
		t.Fatal("legacy state without an exact manifest identity allowed destructive proof")
	}
}

func TestManagedSessionDeletionGuardRejectsManifestRootSymlinkOutsideStore(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	manifestPath := filepath.Join(store, "manifests", "session.json")
	writeJSONFile(t, manifestPath, manifestFixture("session", filepath.Join(store, "native.jsonl"), 4))
	sessionDir := filepath.Join(store, "fs", "sessions", "session")
	deltaPath := writeSizedFile(t, filepath.Join(sessionDir, "delta.jsonl"), 0)
	writeJSONFile(t, filepath.Join(sessionDir, "state.json"), stateFixture(t, "session", manifestPath, 4, deltaPath, "", ""))
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Dir(manifestPath)); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "session.json"), manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Dir(manifestPath)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	guard, err := AcquireManagedSessionDeletionGuard(context.Background(), store)
	if guard != nil {
		_ = guard.Close()
	}
	if err == nil {
		t.Fatal("external manifest reached through a store symlink allowed destructive proof")
	}
}

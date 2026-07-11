package vfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverSessionStatesReturnsValidatedStatesInSessionOrder(t *testing.T) {
	root := t.TempDir()
	for _, sessionID := range []string{"beta", "alpha"} {
		directory := filepath.Join(root, "fs", "sessions", sessionID)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create state directory: %v", err)
		}
		state := SessionState{
			Version: sessionStateVersion, SessionID: sessionID, Generation: 1,
			ManifestPath: filepath.Join(root, "manifests", sessionID+".json"),
			BaseBytes:    1, BaseSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			DeltaPath:      filepath.Join(directory, "delta.jsonl"),
			NativeSnapshot: NativeFile{Path: filepath.Join(root, sessionID+".jsonl"), Bytes: 1, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		}
		if err := writeSessionState(filepath.Join(directory, "state.json"), state); err != nil {
			t.Fatalf("write state: %v", err)
		}
	}
	states, err := DiscoverSessionStates(root)
	if err != nil {
		t.Fatalf("DiscoverSessionStates: %v", err)
	}
	if len(states) != 2 || states[0].SessionID != "alpha" || states[1].SessionID != "beta" {
		t.Fatalf("unexpected states: %#v", states)
	}
}

func TestLoadSessionStateRejectsStateOutsideManagedSessionDirectory(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "fs", "sessions", "session")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	state := SessionState{
		Version: sessionStateVersion, SessionID: "session", Generation: 1,
		ManifestPath: filepath.Join(root, "manifest.json"), BaseBytes: 1,
		BaseSHA256:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DeltaPath:      filepath.Join(root, "outside.jsonl"),
		NativeSnapshot: NativeFile{Path: filepath.Join(root, "native.jsonl"), Bytes: 1, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	if err := writeSessionState(filepath.Join(directory, "state.json"), state); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionState(filepath.Join(directory, "state.json")); err == nil {
		t.Fatal("LoadSessionState should reject data paths outside the managed session directory")
	}
}

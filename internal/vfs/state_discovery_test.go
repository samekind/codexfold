package vfs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlyDiscoveryReportsCheckpointRecoveryWithoutMutation(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	openFixtureSession(t, root, manifest, reader, nil)
	statePath := filepath.Join(root, "fs", "sessions", manifest.Session.ID, "state.json")
	corrupt := []byte("{\"incomplete\":true}\n")
	if err := os.WriteFile(statePath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	states, issues, err := DiscoverSessionStatesDetailedReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 || len(issues) != 1 || issues[0].SessionID != manifest.Session.ID {
		t.Fatalf("read-only discovery states=%#v issues=%#v", states, issues)
	}
	if got, err := os.ReadFile(statePath); err != nil || !bytes.Equal(got, corrupt) {
		t.Fatalf("read-only discovery changed primary state: got=%q err=%v", got, err)
	}

	recovered, err := LoadSessionState(statePath)
	if err != nil || recovered.SessionID != manifest.Session.ID {
		t.Fatalf("explicit checkpoint recovery = %#v, %v", recovered, err)
	}
}

func TestReadOnlyDiscoveryDoesNotRecreateMissingPrimaryState(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	openFixtureSession(t, root, manifest, reader, nil)
	statePath := filepath.Join(root, "fs", "sessions", manifest.Session.ID, "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}

	states, issues, err := DiscoverSessionStatesDetailedReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 || len(issues) != 1 || issues[0].Kind != SessionStateIssueMissingState {
		t.Fatalf("read-only missing-state discovery states=%#v issues=%#v", states, issues)
	}
	if _, err := os.Lstat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only discovery recreated missing state: %v", err)
	}
	if _, err := LoadSessionState(statePath); err != nil {
		t.Fatalf("explicit missing-state recovery: %v", err)
	}
}

func TestDiscoverSessionStatesReturnsValidatedStatesInSessionOrder(t *testing.T) {
	root := t.TempDir()
	for _, sessionID := range []string{"beta", "alpha"} {
		directory := filepath.Join(root, "fs", "sessions", sessionID)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create state directory: %v", err)
		}
		state := SessionState{
			Version: sessionStateVersion, SessionID: sessionID, Generation: 1,
			ManifestPath:   filepath.Join(root, "manifests", sessionID+".json"),
			ManifestSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			BaseBytes:      1, BaseSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			DeltaPath:      filepath.Join(directory, "delta.jsonl"),
			NativeSnapshot: NativeFile{Path: filepath.Join(root, sessionID+".jsonl"), Bytes: 1, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		}
		if err := writeSessionState(filepath.Join(directory, "state.json"), state); err != nil {
			t.Fatalf("write state: %v", err)
		}
		if err := os.WriteFile(state.DeltaPath, nil, 0o600); err != nil {
			t.Fatalf("write delta: %v", err)
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

func TestDiscoverSessionStatesIsolatesIncompleteAndInvalidSessions(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "fs", "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}

	healthyDirectory := filepath.Join(sessions, "healthy")
	if err := os.Mkdir(healthyDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	healthy := SessionState{
		Version: sessionStateVersion, SessionID: "healthy", Generation: 1,
		ManifestPath:   filepath.Join(root, "manifests", "healthy.json"),
		ManifestSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", BaseBytes: 1,
		BaseSHA256:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DeltaPath:      filepath.Join(healthyDirectory, "delta.jsonl"),
		NativeSnapshot: NativeFile{Path: filepath.Join(root, "healthy.jsonl"), Bytes: 1, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	if err := os.WriteFile(healthy.DeltaPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSessionState(filepath.Join(healthyDirectory, "state.json"), healthy); err != nil {
		t.Fatal(err)
	}

	staging := filepath.Join(sessions, initialSessionStagingNamePrefix("staged")+"killed")
	missing := filepath.Join(sessions, "missing")
	invalid := filepath.Join(sessions, "invalid")
	escaped := filepath.Join(sessions, "escaped")
	for _, directory := range []string{staging, missing, invalid, escaped} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(invalid, "state.json"), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "not-a-directory"), []byte("temporary metadata failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	escapedState := healthy
	escapedState.SessionID = "escaped"
	escapedState.DeltaPath = filepath.Join(root, "outside.jsonl")
	if err := writeSessionState(filepath.Join(escaped, "state.json"), escapedState); err != nil {
		t.Fatal(err)
	}
	states, issues, err := DiscoverSessionStatesDetailed(root)
	if err != nil {
		t.Fatalf("DiscoverSessionStatesDetailed: %v", err)
	}
	if len(states) != 1 || states[0].SessionID != "healthy" {
		t.Fatalf("healthy states = %#v, want only healthy", states)
	}
	if len(issues) != 5 {
		t.Fatalf("issues = %#v, want five isolated problems", issues)
	}
	kinds := make(map[SessionStateIssueKind]int)
	for _, issue := range issues {
		kinds[issue.Kind]++
		if issue.SessionID == "" || issue.Path == "" || issue.Err == nil {
			t.Fatalf("issue lacks diagnostics: %#v", issue)
		}
	}
	if kinds[SessionStateIssueStaging] != 1 || kinds[SessionStateIssueMissingState] != 1 || kinds[SessionStateIssueInvalidState] != 3 {
		t.Fatalf("issue kinds = %#v", kinds)
	}

	legacyStates, err := DiscoverSessionStates(root)
	if len(legacyStates) != 1 || legacyStates[0].SessionID != "healthy" {
		t.Fatalf("strict discovery returned %#v, want the healthy state alongside the error", legacyStates)
	}
	var issueErr *SessionStateIssuesError
	if !errors.As(err, &issueErr) || len(issueErr.Issues) != 5 {
		t.Fatalf("strict discovery error = %#v, want five isolated issues", err)
	}
}

func TestDiscoverSessionStatesDetailedPreservesRootReadErrors(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "fs", "sessions")
	if err := os.MkdirAll(filepath.Dir(sessions), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessions, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DiscoverSessionStatesDetailed(root); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root read error = %v, want non-missing failure", err)
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
		ManifestPath:   filepath.Join(root, "manifests", "session.json"),
		ManifestSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", BaseBytes: 1,
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

func TestRepublishSessionStateAdvancesGeneration(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	statePath := filepath.Join(root, "fs", "sessions", manifest.Session.ID, "state.json")

	republished, err := RepublishSessionState(statePath)
	if err != nil {
		t.Fatalf("RepublishSessionState: %v", err)
	}
	if republished.Generation != session.State().Generation+1 {
		t.Fatalf("republished generation = %d, want %d", republished.Generation, session.State().Generation+1)
	}
	loaded, err := LoadSessionState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Generation != republished.Generation {
		t.Fatalf("persisted generation = %d, want %d", loaded.Generation, republished.Generation)
	}
}

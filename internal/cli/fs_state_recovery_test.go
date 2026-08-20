package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
)

func TestRecoverManagedSessionStateIssuesRequiresCodexSnapshotMembership(t *testing.T) {
	fixture := interruptedCanonicalMigrationFixture(t)
	if err := fixture.resolver.Close(); err != nil {
		t.Fatal(err)
	}
	home, store := fixture.home, fixture.store
	statePath := filepath.Join(store, "fs", "sessions", "session", "state.json")
	corrupt := []byte("{\"corrupt\":true}\n")
	if err := os.WriteFile(statePath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	_, issues, err := vfs.DiscoverSessionStatesDetailedReadOnly(store)
	if err != nil || len(issues) != 1 {
		t.Fatalf("read-only discovery issues=%#v err=%v", issues, err)
	}

	recovered, err := recoverManagedSessionStateIssues(store, nil, issues, nil)
	if err != nil || recovered {
		t.Fatalf("recovery without Codex membership = %t, %v", recovered, err)
	}
	if got, err := os.ReadFile(statePath); err != nil || !bytes.Equal(got, corrupt) {
		t.Fatalf("unproven state changed: got=%q err=%v", got, err)
	}

	sessions, err := codex.LoadSessions(home)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err = recoverManagedSessionStateIssues(store, sessions, issues, nil)
	if err != nil || !recovered {
		t.Fatalf("proven state recovery = %t, %v", recovered, err)
	}
	state, err := vfs.InspectSessionState(statePath)
	if err != nil || state.SessionID != "session" {
		t.Fatalf("recovered state = %#v, %v", state, err)
	}
}

func TestRecoverManagedSessionStateIssuesExcludesTombstonedAndNoncanonicalIssues(t *testing.T) {
	fixture := interruptedCanonicalMigrationFixture(t)
	if err := fixture.resolver.Close(); err != nil {
		t.Fatal(err)
	}
	store := fixture.store
	sessions := []codex.Session{{ID: "session"}}
	statePath := filepath.Join(store, "fs", "sessions", "session", "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	issue := vfs.SessionStateIssue{
		SessionID: "session", Path: statePath, Kind: vfs.SessionStateIssueMissingState, Err: os.ErrNotExist,
	}
	recovered, err := recoverManagedSessionStateIssues(store, sessions, []vfs.SessionStateIssue{issue}, map[string]struct{}{"session": {}})
	if err != nil || recovered {
		t.Fatalf("tombstoned state recovery = %t, %v", recovered, err)
	}
	if _, err := os.Lstat(statePath); !os.IsNotExist(err) {
		t.Fatalf("tombstoned state was recreated: %v", err)
	}

	issue.Path = filepath.Join(store, "outside", "state.json")
	if _, err := recoverManagedSessionStateIssues(store, sessions, []vfs.SessionStateIssue{issue}, nil); err == nil {
		t.Fatal("noncanonical state issue was accepted for recovery")
	}
}

func TestRecoverManagedSessionStateIssuesSerializesWithDeletionPublication(t *testing.T) {
	fixture := interruptedCanonicalMigrationFixture(t)
	if err := fixture.resolver.Close(); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(fixture.store, "fs", "sessions", "session", "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	sessions, err := codex.LoadSessions(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	issue := vfs.SessionStateIssue{
		SessionID: "session", Path: statePath, Kind: vfs.SessionStateIssueMissingState, Err: os.ErrNotExist,
	}
	deletionLock, err := storage.AcquireOperationLock(fixture.store, "session-deletions")
	if err != nil {
		t.Fatal(err)
	}
	recovered, recoveryErr := recoverManagedSessionStateIssues(fixture.store, sessions, []vfs.SessionStateIssue{issue}, nil)
	if recovered || !errors.Is(recoveryErr, storage.ErrOperationLockHeld) {
		t.Fatalf("state recovery while deletion lock held = %t, %v", recovered, recoveryErr)
	}
	if _, err := os.Lstat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state changed without deletion serialization: %v", err)
	}
	if err := deletionLock.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, recoveryErr = recoverManagedSessionStateIssues(fixture.store, sessions, []vfs.SessionStateIssue{issue}, nil)
	if recoveryErr != nil || !recovered {
		t.Fatalf("serialized state recovery = %t, %v", recovered, recoveryErr)
	}
}

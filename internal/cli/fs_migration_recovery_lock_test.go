package cli

import (
	"errors"
	"testing"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
)

func TestInterruptedCanonicalMigrationRecoverySerializesWithExplicitDeletion(t *testing.T) {
	fixture := interruptedCanonicalMigrationFixture(t)
	if err := fixture.resolver.Close(); err != nil {
		t.Fatal(err)
	}
	state := fixture.managed.State()
	sessions, err := codex.LoadSessions(fixture.home)
	if err != nil {
		t.Fatal(err)
	}

	deletionLock, err := storage.AcquireOperationLock(fixture.store, "session-deletions")
	if err != nil {
		t.Fatal(err)
	}
	retired, recoveryErr := recoverInterruptedCanonicalMigrationsWithOwnerHandoff(
		fixture.home, fixture.store, fixture.nativeRoot, []vfs.SessionState{state}, sessions, commitRetirementOwnerForTest,
	)
	if len(retired) != 0 || !errors.Is(recoveryErr, storage.ErrOperationLockHeld) {
		t.Fatalf("recovery while deletion lock held: retired=%#v err=%v", retired, recoveryErr)
	}
	if _, err := managedState(fixture.store, state.SessionID); err != nil {
		t.Fatalf("state changed without deletion serialization: %v", err)
	}
	if err := deletionLock.Close(); err != nil {
		t.Fatal(err)
	}

	retired, recoveryErr = recoverInterruptedCanonicalMigrationsWithOwnerHandoff(
		fixture.home, fixture.store, fixture.nativeRoot, []vfs.SessionState{state}, sessions, commitRetirementOwnerForTest,
	)
	if recoveryErr != nil || len(retired) != 1 || retired[0] != state.SessionID {
		t.Fatalf("serialized recovery: retired=%#v err=%v", retired, recoveryErr)
	}
}

func TestInterruptedCanonicalMigrationRecoveryNeverOverridesDeletionTombstone(t *testing.T) {
	fixture := interruptedCanonicalMigrationFixture(t)
	if err := fixture.resolver.Close(); err != nil {
		t.Fatal(err)
	}
	state := fixture.managed.State()
	sessions, err := codex.LoadSessions(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vfs.PublishSessionDeletion(fixture.store, state, "/archived_sessions/rollout-session.jsonl"); err != nil {
		t.Fatal(err)
	}

	retired, recoveryErr := recoverInterruptedCanonicalMigrationsWithOwnerHandoff(
		fixture.home, fixture.store, fixture.nativeRoot, []vfs.SessionState{state}, sessions, commitRetirementOwnerForTest,
	)
	if len(retired) != 0 || recoveryErr == nil {
		t.Fatalf("tombstoned migration recovery: retired=%#v err=%v", retired, recoveryErr)
	}
	if _, err := managedState(fixture.store, state.SessionID); err != nil {
		t.Fatalf("tombstoned state was retired by migration recovery: %v", err)
	}
}

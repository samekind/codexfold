package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/vfs"
)

func TestInterruptedCanonicalMigrationOwnerHandoffPrecedesStateRetirement(t *testing.T) {
	fixture := interruptedCanonicalMigrationFixture(t)
	if err := fixture.resolver.Close(); err != nil {
		t.Fatal(err)
	}
	state := fixture.managed.State()
	sessions, err := codex.LoadSessions(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	handoffErr := errors.New("owner handoff unavailable")
	handoffCalls := 0
	retired, recoveryErr := recoverInterruptedCanonicalMigrationsWithOwnerHandoff(
		fixture.home, fixture.store, fixture.nativeRoot, []vfs.SessionState{state}, sessions,
		func(sessionID string, _ string, _ func() error) error {
			handoffCalls++
			if sessionID != state.SessionID {
				t.Fatalf("handoff session = %s, want %s", sessionID, state.SessionID)
			}
			if _, pending, err := readRetirementRequest(fixture.store, state.SessionID); err != nil || !pending {
				t.Fatalf("retirement request was not durable before handoff: pending=%t err=%v", pending, err)
			}
			return handoffErr
		},
	)
	if len(retired) != 0 || !errors.Is(recoveryErr, handoffErr) || handoffCalls != 1 {
		t.Fatalf("failed handoff recovery: retired=%#v calls=%d err=%v", retired, handoffCalls, recoveryErr)
	}
	if _, err := managedState(fixture.store, state.SessionID); err != nil {
		t.Fatalf("state retired before owner handoff: %v", err)
	}
	if _, pending, err := readRetirementRequest(fixture.store, state.SessionID); err != nil || !pending {
		t.Fatalf("handoff failure lost durable request: pending=%t err=%v", pending, err)
	}

	result, recoveryErr := recoverCanonicalRetirementsWithOwnerHandoff(
		context.Background(), fixture.home, fixture.store, filepath.Join(fixture.home, "fold-fs"), fixture.nativeRoot,
		func(_ string, _ string, commit func() error) error {
			handoffCalls++
			if commit != nil {
				return commit()
			}
			return nil
		},
	)
	if recoveryErr != nil || result.Completed != 1 || len(result.CompletedSessionIDs) != 1 || result.CompletedSessionIDs[0] != state.SessionID || handoffCalls != 2 {
		t.Fatalf("successful handoff replay: result=%#v calls=%d err=%v", result, handoffCalls, recoveryErr)
	}
}

func TestInterruptedCanonicalMigrationCommitRevalidatesFreshCodexRoute(t *testing.T) {
	fixture := interruptedCanonicalMigrationFixture(t)
	if err := fixture.resolver.Close(); err != nil {
		t.Fatal(err)
	}
	state := fixture.managed.State()
	staleSessions, err := codex.LoadSessions(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	changedRoute := filepath.Join(fixture.home, "sessions", "moved-before-cutover.jsonl")
	recovered, err := recoverInterruptedCanonicalMigrationWithOptions(
		fixture.home,
		fixture.store,
		fixture.nativeRoot,
		state,
		func() ([]codex.Session, error) { return staleSessions, nil },
		func(_ string, _ string, commit func() error) error {
			db := openRetirementRecoveryDB(t, fixture.home)
			defer db.Close()
			if _, err := db.Exec(`update threads set rollout_path = ? where id = ?`, changedRoute, state.SessionID); err != nil {
				return err
			}
			return commit()
		},
		nil,
	)
	if err != nil || recovered {
		t.Fatalf("fresh-route rejection recovered=%t err=%v", recovered, err)
	}
	acknowledgement, err := readRetirementAcknowledgementForTest(fixture.store, state.SessionID)
	if err != nil || acknowledgement.Error == "" {
		t.Fatalf("stale route did not publish durable rejection: %#v err=%v", acknowledgement, err)
	}
	active := filepath.Join(fixture.store, "fs", "sessions", state.SessionID)
	if _, err := os.Stat(filepath.Join(active, "state.json")); err != nil {
		t.Fatalf("fresh-route rejection retired managed state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(active, retirementRequestFilename)); err != nil {
		t.Fatalf("fresh-route rejection lost request: %v", err)
	}
}

func TestInterruptedCanonicalMigrationCrashPhasesReplayFromDurableRetirementRequest(t *testing.T) {
	phases := []string{
		retirementPhaseRequestPublished,
		retirementPhaseStateRetired,
		retirementPhaseNativeRetired,
		retirementPhaseAcknowledgementGone,
	}
	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			fixture := interruptedCanonicalMigrationFixture(t)
			if err := fixture.resolver.Close(); err != nil {
				t.Fatal(err)
			}
			state := fixture.managed.State()
			sessions, err := codex.LoadSessions(fixture.home)
			if err != nil {
				t.Fatal(err)
			}
			crash := errors.New("simulated interrupted migration crash")
			recovered, err := recoverInterruptedCanonicalMigrationWithOptions(
				fixture.home,
				fixture.store,
				fixture.nativeRoot,
				state,
				func() ([]codex.Session, error) { return sessions, nil },
				commitRetirementOwnerForTest,
				func(_ string, reached string) error {
					if reached == phase {
						return crash
					}
					return nil
				},
			)
			if recovered || !errors.Is(err, crash) {
				t.Fatalf("first recovery phase=%s recovered=%t err=%v", phase, recovered, err)
			}
			pending, err := discoverPendingCanonicalRetirements(fixture.store)
			if err != nil || len(pending) != 1 {
				t.Fatalf("pending transaction after %s = %#v err=%v", phase, pending, err)
			}
			if _, err := os.Stat(filepath.Join(pending[0].Directory, retirementRequestFilename)); err != nil {
				t.Fatalf("request was not the final completion signal after %s: %v", phase, err)
			}

			result, err := recoverCanonicalRetirements(
				context.Background(), fixture.home, fixture.store,
				filepath.Join(fixture.home, "fold-fs"), fixture.nativeRoot,
			)
			if err != nil || result.Completed != 1 || result.Restored != 0 || result.Deferred != 0 {
				t.Fatalf("replayed migration result=%#v err=%v", result, err)
			}
			if _, err := os.Stat(filepath.Join(fixture.store, "fs", "sessions", state.SessionID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("replayed migration left active state: %v", err)
			}
		})
	}
}

package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
)

type retirementRecoveryFixture struct {
	home       string
	store      string
	mount      string
	nativeRoot string
	sessionID  string
	route      string
	sourcePath string
	targetPath string
	source     vfs.NativeFile
	sourceData []byte
	target     vfs.NativeFile
	request    retirementControl
}

func TestCanonicalRetirementRecoveryReplaysEverySuccessfulCrashPhase(t *testing.T) {
	phases := []string{
		retirementPhaseStateRetired,
		retirementPhaseNativeRetired,
		retirementPhaseAcknowledgementGone,
	}
	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			fixture := newRetirementRecoveryFixture(t)
			crash := errors.New("simulated recovery crash")
			result, err := recoverCanonicalRetirementsWithHook(
				context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot,
				func(_ string, reached string) error {
					if reached == phase {
						return crash
					}
					return nil
				},
			)
			if !errors.Is(err, crash) || result.Completed != 0 || result.Restored != 0 {
				t.Fatalf("first recovery result=%#v err=%v", result, err)
			}
			pending, err := discoverPendingCanonicalRetirements(fixture.store)
			if err != nil || len(pending) != 1 {
				t.Fatalf("pending transaction after %s = %#v err=%v", phase, pending, err)
			}
			if _, err := os.Stat(filepath.Join(pending[0].Directory, retirementRequestFilename)); err != nil {
				t.Fatalf("request was not the last completion signal after %s: %v", phase, err)
			}

			result, err = recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
			if err != nil || result.Completed != 1 || result.Restored != 0 || result.Deferred != 0 || len(result.CompletedSessionIDs) != 1 || result.CompletedSessionIDs[0] != fixture.sessionID {
				t.Fatalf("replayed recovery result=%#v err=%v", result, err)
			}
			assertRetirementCompleted(t, fixture)
			result, err = recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
			if err != nil || !emptyCanonicalRetirementRecoveryResult(result) {
				t.Fatalf("idempotent recovery result=%#v err=%v", result, err)
			}
		})
	}
}

func TestCanonicalRetirementRecoveryReplaysEveryRestoreCrashPhase(t *testing.T) {
	phases := []string{
		retirementPhaseNativeRestored,
		retirementPhaseStateRestored,
		retirementPhaseStateRepublished,
		retirementPhaseAcknowledgementGone,
	}
	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			fixture := newRetirementRecoveryFixture(t)
			retired := retireRecoveryFixtureState(t, fixture)
			retireRecoveryFixtureNative(t, fixture, retired)
			if err := os.WriteFile(fixture.targetPath, []byte("rollback target changed"), 0o600); err != nil {
				t.Fatal(err)
			}
			crash := errors.New("simulated restoration crash")
			result, err := recoverCanonicalRetirementsWithHook(
				context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot,
				func(_ string, reached string) error {
					if reached == phase {
						return crash
					}
					return nil
				},
			)
			if !errors.Is(err, crash) || result.Completed != 0 || result.Restored != 0 {
				t.Fatalf("first restoration result=%#v err=%v", result, err)
			}
			pending, err := discoverPendingCanonicalRetirements(fixture.store)
			if err != nil || len(pending) != 1 {
				t.Fatalf("pending restoration after %s = %#v err=%v", phase, pending, err)
			}
			if _, err := os.Stat(filepath.Join(pending[0].Directory, retirementRequestFilename)); err != nil {
				t.Fatalf("request was not the last restoration signal after %s: %v", phase, err)
			}

			result, err = recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
			if err != nil || result.Completed != 0 || result.Restored != 1 || result.Deferred != 0 || len(result.RestoredSessionIDs) != 1 || result.RestoredSessionIDs[0] != fixture.sessionID {
				t.Fatalf("replayed restoration result=%#v err=%v", result, err)
			}
			assertRetirementRestored(t, fixture)
		})
	}
}

func TestCanonicalRetirementRecoveryFailsClosedOnIdentityMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, retirementRecoveryFixture, string)
	}{
		{
			name: "request generation",
			mutate: func(t *testing.T, fixture retirementRecoveryFixture, retired string) {
				request := fixture.request
				request.Generation++
				if err := writeSessionControlFile(retired, retirementRequestFilename, request); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "state bytes",
			mutate: func(t *testing.T, _ retirementRecoveryFixture, retired string) {
				path := filepath.Join(retired, "state.json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var state vfs.SessionState
				if err := json.Unmarshal(data, &state); err != nil {
					t.Fatal(err)
				}
				state.Generation++
				data, err = json.MarshalIndent(state, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "native snapshot SHA",
			mutate: func(t *testing.T, fixture retirementRecoveryFixture, _ string) {
				if err := os.WriteFile(fixture.sourcePath, []byte("native snapshot changed"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "manifest SHA",
			mutate: func(t *testing.T, fixture retirementRecoveryFixture, _ string) {
				manifestPath := fold.ManifestPath(fixture.store, fixture.sessionID)
				file, err := os.OpenFile(manifestPath, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := file.Write([]byte("\n"))
				closeErr := file.Close()
				if err := errors.Join(writeErr, closeErr); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRetirementRecoveryFixture(t)
			retired := retireRecoveryFixtureState(t, fixture)
			test.mutate(t, fixture, retired)
			result, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
			if err == nil || !emptyCanonicalRetirementRecoveryResult(result) {
				t.Fatalf("mismatch recovery result=%#v err=%v", result, err)
			}
			if _, statErr := os.Stat(filepath.Join(retired, retirementRequestFilename)); statErr != nil {
				t.Fatalf("mismatched transaction request was removed: %v", statErr)
			}
			if _, statErr := os.Stat(filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("mismatched retired state was restored: %v", statErr)
			}
		})
	}
}

func TestCanonicalRetirementRecoveryRestoresWhenCodexNoLongerAuthorizesRetirement(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, retirementRecoveryFixture)
	}{
		{
			name: "session missing from DB snapshot",
			mutate: func(t *testing.T, fixture retirementRecoveryFixture) {
				db := openRetirementRecoveryDB(t, fixture.home)
				if _, err := db.Exec(`delete from threads where id = ?`, fixture.sessionID); err != nil {
					_ = db.Close()
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "route changed",
			mutate: func(t *testing.T, fixture retirementRecoveryFixture) {
				db := openRetirementRecoveryDB(t, fixture.home)
				changed := filepath.Join(fixture.home, "archived_sessions", "different.jsonl")
				if _, err := db.Exec(`update threads set rollout_path = ? where id = ?`, changed, fixture.sessionID); err != nil {
					_ = db.Close()
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRetirementRecoveryFixture(t)
			expectedTarget, err := os.ReadFile(fixture.targetPath)
			if err != nil {
				t.Fatal(err)
			}
			_ = retireRecoveryFixtureState(t, fixture)
			test.mutate(t, fixture)
			result, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
			if err != nil || result.Completed != 0 || result.Restored != 1 || result.Deferred != 0 || len(result.RestoredSessionIDs) != 1 || result.RestoredSessionIDs[0] != fixture.sessionID {
				t.Fatalf("compensating recovery result=%#v err=%v", result, err)
			}
			assertRetirementRestoredWithTarget(t, fixture, expectedTarget)
		})
	}
}

func TestCanonicalRetirementRecoveryRevalidatesRouteAndTargetAtMutationBoundary(t *testing.T) {
	t.Run("target changed before owner handoff", func(t *testing.T) {
		fixture := newRetirementRecoveryFixture(t)
		changed := []byte("native target changed before owner handoff")
		result, err := recoverCanonicalRetirementsWithHook(
			context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot,
			func(_ string, phase string) error {
				if phase != retirementPhaseBeforeOwnerHandoff {
					return nil
				}
				return os.WriteFile(fixture.targetPath, changed, 0o600)
			},
		)
		if err != nil || result.Completed != 0 || result.Restored != 1 {
			t.Fatalf("target-race recovery result=%#v err=%v", result, err)
		}
		assertRetirementRestoredWithTarget(t, fixture, changed)
	})

	t.Run("committed route move after state retirement stays native", func(t *testing.T) {
		fixture := newRetirementRecoveryFixture(t)
		movedPath := fixture.sourcePath
		suffix := []byte("{\"after_cutover_move\":true}\n")
		result, err := recoverCanonicalRetirementsWithHook(
			context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot,
			func(_ string, phase string) error {
				if phase != retirementPhaseStateRetired {
					return nil
				}
				if err := os.Remove(movedPath); err != nil {
					return err
				}
				if err := os.Rename(fixture.targetPath, movedPath); err != nil {
					return err
				}
				file, err := os.OpenFile(movedPath, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					return err
				}
				_, writeErr := file.Write(suffix)
				closeErr := file.Close()
				if err := errors.Join(writeErr, closeErr); err != nil {
					return err
				}
				db := openRetirementRecoveryDB(t, fixture.home)
				defer db.Close()
				changed := filepath.Join(fixture.home, "archived_sessions", filepath.Base(fixture.sourcePath))
				_, updateErr := db.Exec(`update threads set rollout_path = ? where id = ?`, changed, fixture.sessionID)
				return updateErr
			},
		)
		if err != nil || result.Completed != 1 || result.Restored != 0 {
			t.Fatalf("route-race recovery result=%#v err=%v", result, err)
		}
		if _, matches, err := hashPathPrefix(movedPath, fixture.request.Bytes, fixture.request.SHA256); err != nil || !matches {
			t.Fatalf("moved committed native target lost prefix: matches=%t err=%v", matches, err)
		}
		if got, err := os.ReadFile(movedPath); err != nil || !bytes.HasSuffix(got, suffix) {
			t.Fatalf("moved committed native suffix = %q err=%v", got, err)
		}
	})
}

func TestCanonicalRetirementCommittedAppendRemainsForwardOnly(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	if err := writeRetirementAcknowledgement(fixture.store, fixture.sessionID, fixture.request); err != nil {
		t.Fatal(err)
	}
	suffix := []byte("{\"committed_append\":true}\n")
	file, err := os.OpenFile(fixture.targetPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write(suffix)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		t.Fatal(err)
	}
	result, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
	if err != nil || result.Completed != 1 || result.Restored != 0 {
		t.Fatalf("committed append recovery result=%#v err=%v", result, err)
	}
	assertRetirementCompleted(t, fixture)
	if got, err := os.ReadFile(fixture.targetPath); err != nil || !bytes.HasSuffix(got, suffix) {
		t.Fatalf("committed append was not preserved: %q err=%v", got, err)
	}
}

func TestCanonicalRetirementCommittedFinishesWhenMetadataAndTargetDisappear(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	if err := writeRetirementAcknowledgement(fixture.store, fixture.sessionID, fixture.request); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fixture.targetPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fixture.home, "state_5.sqlite")); err != nil {
		t.Fatal(err)
	}
	result, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
	if err != nil || result.Completed != 1 || result.Restored != 0 {
		t.Fatalf("committed missing metadata/target result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed missing metadata/target restored managed authority: %v", err)
	}
	retired, err := filepath.Glob(filepath.Join(fixture.store, "fs", "retired", fixture.sessionID+"-*"))
	if err != nil || len(retired) != 1 {
		t.Fatalf("committed missing metadata/target retired state=%#v err=%v", retired, err)
	}
	if _, err := os.Stat(filepath.Join(retired[0], retirementRequestFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed missing metadata/target retained request: %v", err)
	}
}

func TestCanonicalRetirementCommittedPrefixRewriteCompletesForward(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	if err := writeRetirementAcknowledgement(fixture.store, fixture.sessionID, fixture.request); err != nil {
		t.Fatal(err)
	}
	rewritten := []byte("rewritten-after-ack\n")
	result, err := recoverCanonicalRetirementsWithHook(
		context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot,
		func(_ string, phase string) error {
			if phase != retirementPhaseBeforeCommittedMove {
				return nil
			}
			return os.WriteFile(fixture.targetPath, rewritten, 0o600)
		},
	)
	if err != nil || result.Completed != 1 || result.Restored != 0 {
		t.Fatalf("committed prefix rewrite result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed rewrite restored managed authority: %v", err)
	}
	if got, err := os.ReadFile(fixture.targetPath); err != nil || !bytes.Equal(got, rewritten) {
		t.Fatalf("committed native rewrite was changed: %q err=%v", got, err)
	}
}

func TestCanonicalRetirementCheckpointAdvancePublishesDurableRejection(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	state, err := managedState(fixture.store, fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	managed, resolver, err := openManagedSession(context.Background(), fixture.store, state)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := managed.OpenWriter()
	if err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	tail := []byte("{\"managed_advanced\":true}\n")
	if _, err := writer.Append(context.Background(), tail); err != nil {
		_ = writer.Close()
		_ = resolver.Close()
		t.Fatal(err)
	}
	if err := writer.Sync(); err != nil {
		_ = writer.Close()
		_ = resolver.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	if err := resolver.Close(); err != nil {
		t.Fatal(err)
	}
	nativeTail := []byte("{\"native_diverged\":true}\n")
	nativeFile, err := os.OpenFile(fixture.targetPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, nativeWriteErr := nativeFile.Write(nativeTail)
	nativeCloseErr := nativeFile.Close()
	if err := errors.Join(nativeWriteErr, nativeCloseErr); err != nil {
		t.Fatal(err)
	}
	crash := errors.New("stop after durable rejection")
	result, err := recoverCanonicalRetirementsWithHook(
		context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot,
		func(_ string, phase string) error {
			if phase == retirementPhaseNativeRestored {
				return crash
			}
			return nil
		},
	)
	if !errors.Is(err, crash) || result.Completed != 0 || result.Restored != 0 {
		t.Fatalf("checkpoint advance first recovery result=%#v err=%v", result, err)
	}
	acknowledgement, err := readRetirementAcknowledgementForTest(fixture.store, fixture.sessionID)
	if err != nil || acknowledgement.Error == "" {
		t.Fatalf("checkpoint advance did not publish rejected acknowledgement: %#v err=%v", acknowledgement, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID, retirementRequestFilename)); err != nil {
		t.Fatalf("checkpoint advance lost request before compensation: %v", err)
	}
	result, err = recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
	if err != nil || result.Restored != 1 || result.Completed != 0 {
		t.Fatalf("checkpoint advance compensation result=%#v err=%v", result, err)
	}
	restored, err := managedState(fixture.store, fixture.sessionID)
	if err != nil || restored.Generation != state.Generation+1 {
		t.Fatalf("compensated state=%#v err=%v", restored, err)
	}
	managed, resolver, err = openManagedSession(context.Background(), fixture.store, restored)
	if err != nil {
		t.Fatal(err)
	}
	visiblePath := filepath.Join(t.TempDir(), "visible.jsonl")
	if _, err := managed.MaterializeCurrent(context.Background(), visiblePath, false); err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	if err := resolver.Close(); err != nil {
		t.Fatal(err)
	}
	visible, err := os.ReadFile(visiblePath)
	if err != nil || !bytes.HasSuffix(visible, tail) {
		t.Fatalf("latest managed append was not preserved: %q err=%v", visible, err)
	}
	if native, err := os.ReadFile(fixture.targetPath); err != nil || !bytes.HasSuffix(native, nativeTail) {
		t.Fatalf("divergent native evidence was not preserved: %q err=%v", native, err)
	}
}

func TestCanonicalRetirementRecoveryCleansExactControlTemporariesUnderGuard(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	directory := filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID)
	temporary := filepath.Join(directory, ".retire.ack.json-crashed.tmp")
	if err := os.WriteFile(temporary, []byte("partial acknowledgement"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
	if err != nil || result.Completed != 1 {
		t.Fatalf("control temporary recovery result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale control temporary remains: %v", err)
	}

	fixture = newRetirementRecoveryFixture(t)
	directory = filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID)
	outside := filepath.Join(t.TempDir(), "ack.tmp")
	if err := os.WriteFile(outside, []byte("do not follow"), 0o600); err != nil {
		t.Fatal(err)
	}
	temporary = filepath.Join(directory, ".retire.ack.json-symlink.tmp")
	if err := os.Symlink(outside, temporary); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot); err == nil || !strings.Contains(err.Error(), "bounded regular") {
		t.Fatalf("unsafe control temporary error = %v", err)
	}
	if info, err := os.Lstat(temporary); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("unsafe control temporary changed: info=%v err=%v", info, err)
	}
}

func TestCanonicalRetirementRecoveryCleansCheckpointTemporariesUnderGuard(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	directory := filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID, "state-generations")
	temporary := filepath.Join(directory, ".state-checkpoint-crashed.tmp")
	if err := os.WriteFile(temporary, []byte("partial checkpoint"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
	if err != nil || result.Completed != 1 {
		t.Fatalf("checkpoint temporary recovery result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale checkpoint temporary remains: %v", err)
	}

	fixture = newRetirementRecoveryFixture(t)
	directory = filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID, "state-generations")
	outside := filepath.Join(t.TempDir(), "checkpoint.tmp")
	if err := os.WriteFile(outside, []byte("do not follow"), 0o600); err != nil {
		t.Fatal(err)
	}
	temporary = filepath.Join(directory, ".state-checkpoint-symlink.tmp")
	if err := os.Symlink(outside, temporary); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot); err == nil || !strings.Contains(err.Error(), "bounded regular") {
		t.Fatalf("unsafe checkpoint temporary error = %v", err)
	}
	if info, err := os.Lstat(temporary); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("unsafe checkpoint temporary changed: info=%v err=%v", info, err)
	}
}

func TestCanonicalRetirementRequestCompletionPointIsRequestRemoval(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	crash := errors.New("stop after request removal")
	result, err := recoverCanonicalRetirementsWithHook(
		context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot,
		func(_ string, phase string) error {
			if phase == retirementPhaseRequestCleared {
				return crash
			}
			return nil
		},
	)
	if !errors.Is(err, crash) || result.Completed != 0 {
		t.Fatalf("request removal crash result=%#v err=%v", result, err)
	}
	if pending, err := discoverPendingCanonicalRetirements(fixture.store); err != nil || len(pending) != 0 {
		t.Fatalf("request remained after request-removal crash: pending=%#v err=%v", pending, err)
	}
	retired, err := filepath.Glob(filepath.Join(fixture.store, "fs", "retired", fixture.sessionID+"-*"))
	if err != nil || len(retired) != 1 {
		t.Fatalf("retired state after request-removal crash=%#v err=%v", retired, err)
	}
	if _, err := os.Stat(filepath.Join(retired[0], retirementAcknowledgementFilename)); err != nil {
		t.Fatalf("residual acknowledgement should be harmless after request removal: %v", err)
	}
	result, err = recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
	if err != nil || !emptyCanonicalRetirementRecoveryResult(result) {
		t.Fatalf("request-removal replay result=%#v err=%v", result, err)
	}
}

func readRetirementAcknowledgementForTest(store string, sessionID string) (retirementControl, error) {
	data, err := os.ReadFile(filepath.Join(store, "fs", "sessions", sessionID, retirementAcknowledgementFilename))
	if err != nil {
		return retirementControl{}, err
	}
	var acknowledgement retirementControl
	if err := json.Unmarshal(data, &acknowledgement); err != nil {
		return retirementControl{}, err
	}
	return acknowledgement, nil
}

func TestCanonicalRetirementRouteMatchingRejectsDuplicateSessionIDs(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	sessions := []codex.Session{
		{ID: fixture.sessionID, RolloutPath: filepath.Join(fixture.home, "archived_sessions", "wrong.jsonl")},
		{ID: fixture.sessionID, RolloutPath: filepath.Join(fixture.home, "sessions", "2026", "07", "25", "rollout-session.jsonl")},
	}
	if _, err := canonicalRetirementRouteMatchesSessions(fixture.home, fixture.mount, sessions, fixture.sessionID, fixture.route); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate route match error = %v", err)
	}
}

func TestCanonicalRetirementRecoveryRejectsActiveRetiredConflictAndUnknownData(t *testing.T) {
	t.Run("active and retired conflict", func(t *testing.T) {
		fixture := newRetirementRecoveryFixture(t)
		retired := retireRecoveryFixtureState(t, fixture)
		active := filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID)
		if err := os.MkdirAll(active, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(active, "conflict"), []byte("do not overwrite"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot); err == nil || !strings.Contains(err.Error(), "active and retired") {
			t.Fatalf("active/retired conflict error = %v", err)
		}
		if got, err := os.ReadFile(filepath.Join(active, "conflict")); err != nil || string(got) != "do not overwrite" {
			t.Fatalf("active conflict was changed: %q err=%v", got, err)
		}
		if _, err := os.Stat(filepath.Join(retired, retirementRequestFilename)); err != nil {
			t.Fatalf("retired request changed after conflict: %v", err)
		}
	})

	t.Run("unknown nonempty retired data", func(t *testing.T) {
		fixture := newRetirementRecoveryFixture(t)
		retired := retireRecoveryFixtureState(t, fixture)
		if err := os.WriteFile(filepath.Join(retired, "unknown.bin"), []byte("unknown user data"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot); err == nil || !strings.Contains(err.Error(), "unknown nonempty") {
			t.Fatalf("unknown data error = %v", err)
		}
		if got, err := os.ReadFile(filepath.Join(retired, "unknown.bin")); err != nil || string(got) != "unknown user data" {
			t.Fatalf("unknown data was changed: %q err=%v", got, err)
		}
		if _, err := os.Stat(filepath.Join(retired, retirementRequestFilename)); err != nil {
			t.Fatalf("request changed after unknown data rejection: %v", err)
		}
	})

	t.Run("exact native bytes through symlink", func(t *testing.T) {
		fixture := newRetirementRecoveryFixture(t)
		retired := retireRecoveryFixtureState(t, fixture)
		outside := filepath.Join(t.TempDir(), "outside-native.jsonl")
		if err := os.WriteFile(outside, fixture.sourceData, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(fixture.sourcePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, fixture.sourcePath); err != nil {
			t.Fatal(err)
		}
		if _, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("native symlink error = %v", err)
		}
		if info, err := os.Lstat(fixture.sourcePath); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("native symlink was changed: info=%v err=%v", info, err)
		}
		if _, err := os.Stat(filepath.Join(retired, retirementRequestFilename)); err != nil {
			t.Fatalf("request changed after symlink rejection: %v", err)
		}
	})

	t.Run("exact native bytes through intermediate symlink", func(t *testing.T) {
		fixture := newRetirementRecoveryFixture(t)
		retired := retireRecoveryFixtureState(t, fixture)
		outsideDirectory := t.TempDir()
		outside := filepath.Join(outsideDirectory, filepath.Base(fixture.sourcePath))
		if err := os.WriteFile(outside, fixture.sourceData, 0o600); err != nil {
			t.Fatal(err)
		}
		originalDirectory := filepath.Dir(fixture.sourcePath)
		if err := os.Remove(fixture.sourcePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(originalDirectory); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideDirectory, originalDirectory); err != nil {
			t.Fatal(err)
		}
		if _, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot); err == nil || !strings.Contains(err.Error(), "contains a symlink") {
			t.Fatalf("intermediate native symlink error = %v", err)
		}
		if info, err := os.Lstat(originalDirectory); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("intermediate native symlink was changed: info=%v err=%v", info, err)
		}
		if _, err := os.Stat(filepath.Join(retired, retirementRequestFilename)); err != nil {
			t.Fatalf("request changed after intermediate symlink rejection: %v", err)
		}
	})
}

func TestCanonicalRetirementRecoveryIgnoresCompletedRetirement(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	retired := retireRecoveryFixtureState(t, fixture)
	retiredSnapshot := retireRecoveryFixtureNative(t, fixture, retired)
	if err := clearRetirementControl(retired); err != nil {
		t.Fatal(err)
	}
	result, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
	if err != nil || !emptyCanonicalRetirementRecoveryResult(result) {
		t.Fatalf("completed retirement recovery result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed retirement was restored: %v", err)
	}
	if got, err := os.ReadFile(retiredSnapshot); err != nil || got == nil {
		t.Fatalf("completed retired snapshot changed: bytes=%d err=%v", len(got), err)
	}
}

func TestCanonicalRetirementRecoveryNeverOverridesExplicitDeletion(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	state, err := managedState(fixture.store, fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vfs.PublishSessionDeletion(fixture.store, state, fixture.route); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot); err == nil || !strings.Contains(err.Error(), "deletion tombstone supersedes") {
		t.Fatalf("deletion precedence error = %v", err)
	}
	request := filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID, retirementRequestFilename)
	if _, err := os.Stat(request); err != nil {
		t.Fatalf("retirement request changed despite deletion authority: %v", err)
	}
	if identity, err := hashPath(fixture.sourcePath); err != nil || identity.Bytes != fixture.source.Bytes || identity.SHA256 != fixture.source.SHA256 {
		t.Fatalf("native snapshot changed despite deletion authority: %#v err=%v", identity, err)
	}
}

func TestCanonicalRetirementRecoveryDefersWhileLocksAreHeld(t *testing.T) {
	t.Run("writer lease", func(t *testing.T) {
		fixture := newRetirementRecoveryFixture(t)
		leasePath := filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID, "writer.lease")
		guard, acquired, err := vfs.TryAcquireWriterLeaseGuardAtPath(leasePath)
		if err != nil || !acquired {
			t.Fatalf("hold writer lease: acquired=%t err=%v", acquired, err)
		}
		result, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
		if err != nil || result.Deferred != 1 || result.Completed != 0 || result.Restored != 0 || len(result.DeferredSessionIDs) != 1 || result.DeferredSessionIDs[0] != fixture.sessionID {
			t.Fatalf("writer-busy result=%#v err=%v", result, err)
		}
		if _, err := os.Stat(filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID, retirementRequestFilename)); err != nil {
			t.Fatalf("writer-busy request changed: %v", err)
		}
		if err := guard.Close(); err != nil {
			t.Fatal(err)
		}
		result, err = recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
		if err != nil || result.Completed != 1 {
			t.Fatalf("writer release recovery result=%#v err=%v", result, err)
		}
	})

	t.Run("operation lock", func(t *testing.T) {
		fixture := newRetirementRecoveryFixture(t)
		lock, err := storage.AcquireOperationLock(fixture.store, canonicalRetirementRecoveryLock)
		if err != nil {
			t.Fatal(err)
		}
		result, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
		if err != nil || result.Deferred != 1 || result.Completed != 0 || result.Restored != 0 || len(result.DeferredSessionIDs) != 1 || result.DeferredSessionIDs[0] != fixture.sessionID {
			t.Fatalf("operation-lock result=%#v err=%v", result, err)
		}
		if _, err := os.Stat(filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID, retirementRequestFilename)); err != nil {
			t.Fatalf("operation-lock request changed: %v", err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
		result, err = recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
		if err != nil || result.Completed != 1 {
			t.Fatalf("operation lock release recovery result=%#v err=%v", result, err)
		}
	})

	t.Run("deletion operation lock", func(t *testing.T) {
		fixture := newRetirementRecoveryFixture(t)
		lock, err := storage.AcquireOperationLock(fixture.store, "session-deletions")
		if err != nil {
			t.Fatal(err)
		}
		result, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
		if err != nil || result.Deferred != 1 || result.Completed != 0 || result.Restored != 0 || len(result.DeferredSessionIDs) != 1 || result.DeferredSessionIDs[0] != fixture.sessionID {
			t.Fatalf("deletion-lock result=%#v err=%v", result, err)
		}
		if _, err := os.Stat(filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID, retirementRequestFilename)); err != nil {
			t.Fatalf("deletion-lock request changed: %v", err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
		result, err = recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
		if err != nil || result.Completed != 1 {
			t.Fatalf("deletion lock release recovery result=%#v err=%v", result, err)
		}
	})
}

func TestCanonicalRetirementRecoveryHoldsBothOperationLocksThroughMutation(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	crash := errors.New("stop after lock assertion")
	checked := false
	_, err := recoverCanonicalRetirementsWithHook(
		context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot,
		func(_ string, phase string) error {
			if phase != retirementPhaseStateRetired {
				return nil
			}
			for _, name := range []string{canonicalRetirementRecoveryLock, "session-deletions"} {
				lock, lockErr := storage.AcquireOperationLock(fixture.store, name)
				if lockErr == nil {
					_ = lock.Close()
					return fmt.Errorf("operation lock %s was not held", name)
				}
				if !errors.Is(lockErr, storage.ErrOperationLockHeld) {
					return lockErr
				}
			}
			checked = true
			return crash
		},
	)
	if !errors.Is(err, crash) || !checked {
		t.Fatalf("operation lock assertion checked=%t err=%v", checked, err)
	}
	result, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
	if err != nil || result.Completed != 1 {
		t.Fatalf("replay after lock assertion result=%#v err=%v", result, err)
	}
}

func TestCanonicalRetirementRecoveryKeepsRequestUntilOwnerHandoffCompletes(t *testing.T) {
	t.Run("handoff failure", func(t *testing.T) {
		fixture := newRetirementRecoveryFixture(t)
		handoffFailure := errors.New("owner detach failed")
		calls := 0
		result, err := recoverCanonicalRetirementsWithOwnerHandoff(
			context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot,
			func(sessionID string, _ string, _ func() error) error {
				calls++
				if sessionID != fixture.sessionID {
					return fmt.Errorf("handoff session = %s", sessionID)
				}
				pending, pendingErr := discoverPendingCanonicalRetirements(fixture.store)
				if pendingErr != nil || len(pending) != 1 {
					return fmt.Errorf("request disappeared before handoff: pending=%#v err=%v", pending, pendingErr)
				}
				return handoffFailure
			},
		)
		if !errors.Is(err, handoffFailure) || result.Completed != 0 || calls != 1 {
			t.Fatalf("failed handoff result=%#v calls=%d err=%v", result, calls, err)
		}
		pending, err := discoverPendingCanonicalRetirements(fixture.store)
		if err != nil || len(pending) != 1 {
			t.Fatalf("pending request after handoff failure = %#v err=%v", pending, err)
		}

		result, err = recoverCanonicalRetirementsWithOwnerHandoff(
			context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot,
			func(sessionID string, _ string, commit func() error) error {
				calls++
				if sessionID != fixture.sessionID {
					return fmt.Errorf("handoff session = %s", sessionID)
				}
				if commit != nil {
					return commit()
				}
				return nil
			},
		)
		if err != nil || result.Completed != 1 || calls != 2 {
			t.Fatalf("replayed handoff result=%#v calls=%d err=%v", result, calls, err)
		}
		assertRetirementCompleted(t, fixture)
	})

	t.Run("crash after successful handoff", func(t *testing.T) {
		fixture := newRetirementRecoveryFixture(t)
		crash := errors.New("crash after owner detach")
		calls := 0
		_, err := recoverCanonicalRetirementsWithOptions(
			context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot,
			func(_ string, phase string) error {
				if phase == retirementPhaseOwnerDetached {
					return crash
				}
				return nil
			},
			func(_ string, _ string, commit func() error) error {
				calls++
				if commit != nil {
					return commit()
				}
				return nil
			},
		)
		if !errors.Is(err, crash) || calls != 1 {
			t.Fatalf("post-handoff crash calls=%d err=%v", calls, err)
		}
		pending, err := discoverPendingCanonicalRetirements(fixture.store)
		if err != nil || len(pending) != 1 {
			t.Fatalf("request disappeared after post-handoff crash: pending=%#v err=%v", pending, err)
		}
		result, err := recoverCanonicalRetirementsWithOwnerHandoff(
			context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot,
			func(_ string, _ string, commit func() error) error {
				calls++
				if commit != nil {
					return commit()
				}
				return nil
			},
		)
		if err != nil || result.Completed != 1 || calls != 2 {
			t.Fatalf("post-handoff replay result=%#v calls=%d err=%v", result, calls, err)
		}
	})
}

func TestCanonicalRetirementRecoveryRequiresCompleteCodexSnapshotBeforeMutation(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	retired := retireRecoveryFixtureState(t, fixture)
	if err := os.Remove(filepath.Join(fixture.home, "state_5.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot); err == nil || !strings.Contains(err.Error(), "Codex metadata") {
		t.Fatalf("missing DB recovery error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(retired, retirementRequestFilename)); err != nil {
		t.Fatalf("request changed without DB snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.store, "locks", canonicalRetirementRecoveryLock+".lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery lock was created before DB snapshot: %v", err)
	}
}

func TestCanonicalRetirementRecoveryReplaysInterruptedStateRepublish(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	statePath := filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID, "state.json")
	oldState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.targetPath, []byte("rollback target changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := vfs.RepublishSessionState(statePath); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the new checkpoint/catalog became durable but
	// before the new primary state file replaced its predecessor.
	if err := os.WriteFile(statePath, oldState, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
	if err != nil || result.Restored != 1 || result.Completed != 0 || result.Deferred != 0 {
		t.Fatalf("interrupted republish result=%#v err=%v", result, err)
	}
	assertRetirementRestored(t, fixture)
}

func TestCanonicalNativeFileFenceRejectsMetadataPreservingRewrite(t *testing.T) {
	nativeRoot := t.TempDir()
	path := filepath.Join(nativeRoot, "archived_sessions", "rollout.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Unix(1_700_000_000, 123_456_789)
	if err := os.Chtimes(path, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}
	expected, err := hashStableCanonicalNativeFile(nativeRoot, path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("BBBB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("fixture did not preserve metadata: before=%#v after=%#v", before, after)
	}
	if canonicalNativeFileStillMatches(nativeRoot, path, expected) {
		t.Fatal("exact cutover fence accepted rewritten bytes with preserved metadata")
	}
}

func TestCanonicalRetirementRecoveryRollsBackDataSyncedCopyOnWrite(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	directory := filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID)
	state, err := managedState(fixture.store, fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(directory, ".backing-retirementcrash.tmp")
	if err := os.WriteFile(temporary, []byte("durable copy-on-write temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := hashPath(temporary)
	if err != nil {
		t.Fatal(err)
	}
	appendRetirementJournalRecordForTest(t, directory, vfs.JournalRecord{
		OperationID: fmt.Sprintf("cow-%020d", state.Generation), SessionID: state.SessionID,
		Kind: "copy-on-write", Phase: "data-synced", TempPath: temporary, Native: identity,
	})

	result, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
	if err != nil || result.Completed != 1 || result.Restored != 0 || result.Deferred != 0 {
		t.Fatalf("data-synced COW retirement recovery result=%#v err=%v", result, err)
	}
	assertRetirementCompleted(t, fixture)
	retired, err := filepath.Glob(filepath.Join(fixture.store, "fs", "retired", fixture.sessionID+"-*"))
	if err != nil || len(retired) != 1 {
		t.Fatalf("retired state = %#v err=%v", retired, err)
	}
	if _, err := os.Stat(filepath.Join(retired[0], filepath.Base(temporary))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rolled-back backing temporary remains: %v", err)
	}
	if latest := latestRetirementJournalRecordForTest(t, retired[0]); latest.Phase != "rolled-back" {
		t.Fatalf("latest COW journal record = %#v", latest)
	}
}

func TestCanonicalRetirementRecoveryFinishesPublishedCopyOnWriteBeforeCompensation(t *testing.T) {
	fixture := newRetirementRecoveryFixture(t)
	directory := filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID)
	state, err := managedState(fixture.store, fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	visible, err := os.ReadFile(fixture.targetPath)
	if err != nil {
		t.Fatal(err)
	}
	backing := filepath.Join(directory, fmt.Sprintf("backing-%020d.jsonl", state.Generation+1))
	if err := os.WriteFile(backing, visible, 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := hashPath(backing)
	if err != nil {
		t.Fatal(err)
	}
	identity.Path = filepath.Join(directory, ".backing-retirementcrash.tmp")
	candidate := state
	candidate.Generation++
	candidate.BackingPath = backing
	appendRetirementJournalRecordForTest(t, directory, vfs.JournalRecord{
		OperationID: fmt.Sprintf("cow-%020d", state.Generation), SessionID: state.SessionID,
		Kind: "copy-on-write", Phase: "after-file-publish", Candidate: candidate,
		FinalPath: backing, Native: identity,
	})

	result, err := recoverCanonicalRetirements(context.Background(), fixture.home, fixture.store, fixture.mount, fixture.nativeRoot)
	if err != nil || result.Completed != 0 || result.Restored != 1 || result.Deferred != 0 {
		t.Fatalf("published COW retirement recovery result=%#v err=%v", result, err)
	}
	assertRetirementRestoredWithTarget(t, fixture, visible)
	restored, err := managedState(fixture.store, fixture.sessionID)
	if err != nil || restored != candidate {
		t.Fatalf("restored COW state=%#v want=%#v err=%v", restored, candidate, err)
	}
	if got, err := os.ReadFile(backing); err != nil || !bytes.Equal(got, visible) {
		t.Fatalf("published COW backing=%q err=%v", got, err)
	}
	if latest := latestRetirementJournalRecordForTest(t, directory); latest.Phase != "complete" {
		t.Fatalf("latest COW journal record = %#v", latest)
	}
}

func newRetirementRecoveryFixture(t *testing.T) retirementRecoveryFixture {
	t.Helper()
	home, store, originalPath := fsFixture(t, true)
	sessionID := "session"
	filename := "rollout-session.jsonl"
	nativeRoot := filepath.Join(home, "fold-native")
	sourcePath := filepath.Join(nativeRoot, "archived_sessions", filename)
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(originalPath, sourcePath); err != nil {
		t.Fatal(err)
	}
	routePath := filepath.Join(home, "sessions", "2026", "07", "25", filename)
	db := openRetirementRecoveryDB(t, home)
	if _, err := db.Exec(`update threads set rollout_path = ? where id = ?`, routePath, sessionID); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	manifest, err := fold.LoadManifest(store, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := pack.Open(store, pack.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	source, err := hashPath(sourcePath)
	if err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	sourceData, err := os.ReadFile(sourcePath)
	if err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	managed, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{
		Root: store, ManifestPath: fold.ManifestPath(store, sessionID), Manifest: manifest,
		Reader: resolver, NativeSnapshot: source,
	})
	if err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	writer, err := managed.OpenWriter()
	if err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	if _, err := writer.Append(context.Background(), []byte("{\"retirement_recovery\":true}\n")); err != nil {
		_ = writer.Close()
		_ = resolver.Close()
		t.Fatal(err)
	}
	if err := writer.Sync(); err != nil {
		_ = writer.Close()
		_ = resolver.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	targetPath := filepath.Join(nativeRoot, "sessions", "2026", "07", "25", filename)
	target, err := managed.MaterializeCurrent(context.Background(), targetPath, true)
	if err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	state := managed.State()
	mount := filepath.Join(home, "fold-fs")
	route := "/sessions/2026/07/25/" + filename
	request, err := createRetirementRequest(store, sessionID, state.Generation, route, target)
	if err != nil {
		_ = resolver.Close()
		t.Fatal(err)
	}
	if err := resolver.Close(); err != nil {
		t.Fatal(err)
	}
	return retirementRecoveryFixture{
		home: home, store: store, mount: mount, nativeRoot: nativeRoot, sessionID: sessionID,
		route: route, sourcePath: sourcePath, targetPath: targetPath, source: source, sourceData: sourceData, target: target, request: request,
	}
}

func retireRecoveryFixtureState(t *testing.T, fixture retirementRecoveryFixture) string {
	t.Helper()
	retired, err := retireManagedState(fixture.store, fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return retired
}

func retireRecoveryFixtureNative(t *testing.T, fixture retirementRecoveryFixture, retired string) string {
	t.Helper()
	path, err := retireCanonicalNativeSnapshot(fixture.store, fixture.nativeRoot, fixture.sessionID, fixture.sourcePath, fixture.targetPath, retired)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func assertRetirementCompleted(t *testing.T, fixture retirementRecoveryFixture) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed state remained active: %v", err)
	}
	retired, err := filepath.Glob(filepath.Join(fixture.store, "fs", "retired", fixture.sessionID+"-*"))
	if err != nil || len(retired) != 1 {
		t.Fatalf("retired state = %#v err=%v", retired, err)
	}
	for _, name := range []string{retirementRequestFilename, retirementAcknowledgementFilename} {
		if _, err := os.Stat(filepath.Join(retired[0], name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retirement control %s remains: %v", name, err)
		}
	}
	if got, err := os.ReadFile(fixture.sourcePath); err != nil || !bytes.Equal(got, fixture.sourceData) {
		t.Fatalf("legacy native snapshot was changed after committed retirement: %q err=%v", got, err)
	}
	retained := filepath.Join(retired[0], "retained-native", "archived_sessions", filepath.Base(fixture.sourcePath))
	if _, err := os.Stat(retained); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy native snapshot was moved into retired state: %v", err)
	}
	if identity, matches, err := hashPathPrefix(fixture.targetPath, fixture.request.Bytes, fixture.request.SHA256); err != nil || !matches {
		t.Fatalf("canonical rollback target = %#v prefix_matches=%t err=%v", identity, matches, err)
	}
}

func assertRetirementRestored(t *testing.T, fixture retirementRecoveryFixture) {
	t.Helper()
	assertRetirementRestoredWithTarget(t, fixture, []byte("rollback target changed"))
}

func assertRetirementRestoredWithTarget(t *testing.T, fixture retirementRecoveryFixture, expectedTarget []byte) {
	t.Helper()
	active := filepath.Join(fixture.store, "fs", "sessions", fixture.sessionID)
	state, err := vfs.LoadSessionState(filepath.Join(active, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if state.Generation != fixture.request.Generation+1 || state.SessionID != fixture.sessionID {
		t.Fatalf("restored state = %#v", state)
	}
	for _, name := range []string{retirementRequestFilename, retirementAcknowledgementFilename} {
		if _, err := os.Stat(filepath.Join(active, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("restored control %s remains: %v", name, err)
		}
	}
	if identity, err := hashPath(fixture.sourcePath); err != nil || identity.Bytes != fixture.source.Bytes || identity.SHA256 != fixture.source.SHA256 {
		t.Fatalf("restored native snapshot = %#v err=%v", identity, err)
	}
	if got, err := os.ReadFile(fixture.targetPath); err != nil || !bytes.Equal(got, expectedTarget) {
		t.Fatalf("failed rollback target changed during restore: %q err=%v", got, err)
	}
}

func openRetirementRecoveryDB(t *testing.T, home string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func appendRetirementJournalRecordForTest(t *testing.T, directory string, record vfs.JournalRecord) {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(directory, "journal.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write(append(data, '\n'))
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		t.Fatal(err)
	}
}

func latestRetirementJournalRecordForTest(t *testing.T, directory string) vfs.JournalRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
	if len(lines) == 0 || len(lines[0]) == 0 {
		t.Fatal("retirement journal is empty")
	}
	var record vfs.JournalRecord
	if err := json.Unmarshal(lines[len(lines)-1], &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func emptyCanonicalRetirementRecoveryResult(result canonicalRetirementRecoveryResult) bool {
	return result.Completed == 0 && result.Restored == 0 && result.Deferred == 0 &&
		len(result.CompletedSessionIDs) == 0 && len(result.RestoredSessionIDs) == 0 && len(result.DeferredSessionIDs) == 0
}

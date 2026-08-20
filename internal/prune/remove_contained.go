package prune

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/contain"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/storage"
	_ "modernc.org/sqlite"
)

type Options struct {
	Apply              bool
	IncludeSessionMeta bool
	WriterActive       func(context.Context, codex.Session) (bool, error)
	BeforeRename       func() error
	AfterCommit        func() error
	AfterPurgeStage    func() error
}

type Result struct {
	ContainedSessionID string `json:"contained_session_id"`
	ContainerSessionID string `json:"container_session_id"`
	DryRun             bool   `json:"dry_run"`
	Contained          bool   `json:"contained"`
	FoldVerified       bool   `json:"fold_verified"`
	UnfoldVerified     bool   `json:"unfold_verified"`
	Removed            bool   `json:"removed"`
	SourceBytes        int64  `json:"source_bytes"`
	SourceSHA256       string `json:"source_sha256"`
	TombstonePath      string `json:"tombstone_path,omitempty"`
}

type Tombstone struct {
	Version              int          `json:"version"`
	Kind                 string       `json:"kind"`
	Phase                removalPhase `json:"phase,omitempty"`
	PreparedAt           string       `json:"prepared_at,omitempty"`
	RemovedAt            string       `json:"removed_at,omitempty"`
	ContainedSessionID   string       `json:"contained_session_id"`
	ContainerSessionID   string       `json:"container_session_id"`
	OriginalRolloutPath  string       `json:"original_rollout_path"`
	PendingRolloutPath   string       `json:"pending_rollout_path,omitempty"`
	PurgeRolloutPath     string       `json:"purge_rollout_path,omitempty"`
	SourceBytes          int64        `json:"source_bytes"`
	SourceSHA256         string       `json:"source_sha256"`
	RecoveryManifestPath string       `json:"recovery_manifest_path"`
}

type removalPhase string

const (
	removalPhasePrepared      removalPhase = "prepared"
	removalPhaseIsolated      removalPhase = "isolated"
	removalPhaseCommitted     removalPhase = "committed"
	removalPhaseGlobalCleaned removalPhase = "global-cleaned"
	removalPhasePurgeStaged   removalPhase = "purge-staged"
	removalPhasePurged        removalPhase = "purged"
)

type RecoveryResult struct {
	SessionID   string `json:"session_id"`
	SourcePath  string `json:"source_path"`
	PendingPath string `json:"pending_path"`
	RolledBack  bool   `json:"rolled_back"`
	Finalized   bool   `json:"finalized"`
}

type sourceSnapshot struct {
	Bytes  int64
	SHA256 string
}

var errGlobalStateChanged = errors.New("Codex global state changed concurrently; refusing to overwrite it")

var commitContainedTransaction = func(tx *sql.Tx) error {
	return tx.Commit()
}

func TombstonePath(storeDir string, sessionID string) string {
	return filepath.Join(storeDir, "tombstones", sessionID+".json")
}

func RemoveContained(ctx context.Context, codexHome string, storeDir string, containedSession codex.Session, containerSession codex.Session, options Options) (Result, error) {
	result := Result{
		ContainedSessionID: containedSession.ID,
		ContainerSessionID: containerSession.ID,
		DryRun:             !options.Apply,
	}
	if containedSession.ID == "" || containerSession.ID == "" || containedSession.ID == containerSession.ID {
		return Result{}, errors.New("contained and container sessions must be distinct")
	}
	if err := validateSessionID(containedSession.ID); err != nil {
		return Result{}, err
	}
	if err := validateSessionID(containerSession.ID); err != nil {
		return Result{}, err
	}
	if !containedSession.Archived {
		return Result{}, errors.New("refusing to remove a non-archived contained session")
	}
	containment, err := contain.Check(ctx,
		contain.Input{ID: containedSession.ID, Path: containedSession.RolloutPath},
		contain.Input{ID: containerSession.ID, Path: containerSession.RolloutPath},
		contain.Options{IgnoreSessionMeta: !options.IncludeSessionMeta},
	)
	if err != nil {
		return Result{}, err
	}
	if !containment.Contained || !containment.VerifiedExact {
		return Result{}, errors.New("contained session is not an exact contiguous record sequence of the container")
	}
	result.Contained = true

	manifest, err := fold.VerifySession(ctx, storeDir, containedSession.ID)
	if err != nil {
		return Result{}, fmt.Errorf("verify recovery fold: %w", err)
	}
	if !manifest.Session.Archived || filepath.Clean(manifest.Session.RolloutPath) != filepath.Clean(containedSession.RolloutPath) {
		return Result{}, errors.New("recovery fold does not describe the selected archived rollout")
	}
	result.FoldVerified = true
	result.SourceBytes = manifest.Source.Bytes
	result.SourceSHA256 = manifest.Source.SHA256
	initialSource, err := stableSourceSnapshot(containedSession.RolloutPath)
	if err != nil {
		return Result{}, err
	}
	if initialSource.Bytes != manifest.Source.Bytes || initialSource.SHA256 != manifest.Source.SHA256 {
		return Result{}, errors.New("contained source differs from its recovery fold")
	}
	if options.Apply && options.WriterActive == nil {
		return Result{}, errors.New("contained-session removal requires a native writer probe")
	}
	if active, err := removalWriterActive(ctx, containedSession, options.WriterActive); err != nil {
		return Result{}, err
	} else if active {
		return Result{}, errors.New("cannot remove a contained session with an active writer")
	}

	proofDir, err := os.MkdirTemp("", "codexfold-remove-proof-")
	if err != nil {
		return Result{}, fmt.Errorf("create unfold proof directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(proofDir) }()
	proofPath := filepath.Join(proofDir, "rollout.jsonl")
	proof, err := fold.Unfold(ctx, storeDir, containedSession.ID, proofPath, false)
	if err != nil {
		return Result{}, fmt.Errorf("prove recovery unfold: %w", err)
	}
	if !proof.Verified || proof.Bytes != manifest.Source.Bytes || proof.SHA256 != manifest.Source.SHA256 {
		return Result{}, errors.New("recovery unfold proof does not match the source manifest")
	}
	result.UnfoldVerified = true
	if !options.Apply {
		return result, nil
	}
	operation, err := storage.AcquireOperationLock(storeDir, removalLockName(containedSession.ID))
	if err != nil {
		return Result{}, fmt.Errorf("serialize contained-session removal: %w", err)
	}
	defer func() { _ = operation.Close() }()

	pendingPath := containedSession.RolloutPath + ".codexfold-remove-pending"
	purgePath := pendingPath + ".purge"
	tombstone := Tombstone{
		Version: 2, Kind: "contained-session-removal-v2", Phase: removalPhasePrepared,
		PreparedAt:         time.Now().UTC().Format(time.RFC3339Nano),
		ContainedSessionID: containedSession.ID, ContainerSessionID: containerSession.ID,
		OriginalRolloutPath: containedSession.RolloutPath, PendingRolloutPath: pendingPath, PurgeRolloutPath: purgePath,
		SourceBytes:  manifest.Source.Bytes,
		SourceSHA256: manifest.Source.SHA256, RecoveryManifestPath: fold.ManifestPath(storeDir, containedSession.ID),
	}
	tombstonePath := TombstonePath(storeDir, containedSession.ID)
	if _, err := os.Stat(tombstonePath); err == nil {
		return Result{}, fmt.Errorf("removal tombstone already exists: %s", tombstonePath)
	} else if !os.IsNotExist(err) {
		return Result{}, err
	}
	if err := writeJSONAtomically(tombstonePath, tombstone, 0o600); err != nil {
		return Result{}, err
	}
	db, err := sql.Open("sqlite", filepath.Join(codexHome, "state_5.sqlite"))
	if err != nil {
		return Result{}, errors.Join(fmt.Errorf("open Codex state database for removal: %w", err), removeRemovalTombstone(tombstonePath))
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`pragma busy_timeout = 10000`); err != nil {
		return Result{}, errors.Join(err, removeRemovalTombstone(tombstonePath))
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, errors.Join(fmt.Errorf("begin Codex removal transaction: %w", err), removeRemovalTombstone(tombstonePath))
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var dbRolloutPath string
	var archived int
	if err := tx.QueryRowContext(ctx, `select rollout_path, archived from threads where id = ?`, containedSession.ID).Scan(&dbRolloutPath, &archived); err != nil {
		return Result{}, errors.Join(fmt.Errorf("revalidate contained thread: %w", err), removeRemovalTombstone(tombstonePath))
	}
	if archived == 0 || filepath.Clean(dbRolloutPath) != filepath.Clean(containedSession.RolloutPath) {
		return Result{}, errors.Join(errors.New("contained thread changed before removal"), removeRemovalTombstone(tombstonePath))
	}
	if options.BeforeRename != nil {
		if err := options.BeforeRename(); err != nil {
			return Result{}, errors.Join(err, removeRemovalTombstone(tombstonePath))
		}
	}
	currentSource, err := stableSourceSnapshot(containedSession.RolloutPath)
	if err != nil || currentSource != initialSource {
		if err == nil {
			err = errors.New("contained source changed before removal")
		}
		return Result{}, errors.Join(err, removeRemovalTombstone(tombstonePath))
	}
	if active, err := removalWriterActive(ctx, containedSession, options.WriterActive); err != nil {
		return Result{}, errors.Join(err, removeRemovalTombstone(tombstonePath))
	} else if active {
		return Result{}, errors.Join(errors.New("cannot remove a contained session with an active writer"), removeRemovalTombstone(tombstonePath))
	}
	if _, err := os.Lstat(pendingPath); err == nil {
		return Result{}, errors.Join(fmt.Errorf("pending removal path already exists: %s", pendingPath), removeRemovalTombstone(tombstonePath))
	} else if !os.IsNotExist(err) {
		return Result{}, errors.Join(err, removeRemovalTombstone(tombstonePath))
	}
	if err := renameRemovalNoReplace(containedSession.RolloutPath, pendingPath); err != nil {
		return Result{}, errors.Join(fmt.Errorf("isolate contained rollout: %w", err), removeRemovalTombstone(tombstonePath))
	}
	rollbackBeforeCommit := func(cause error) (Result, error) {
		rollbackErr := restoreIsolatedSource(ctx, containedSession, pendingPath, initialSource, options.WriterActive)
		if rollbackErr == nil {
			rollbackErr = removeRemovalTombstone(tombstonePath)
		}
		return Result{}, errors.Join(cause, rollbackErr)
	}
	if err := syncStateDirectory(filepath.Dir(pendingPath)); err != nil {
		return rollbackBeforeCommit(fmt.Errorf("synchronize isolated contained rollout: %w", err))
	}
	isolatedSource, err := stableSourceSnapshot(pendingPath)
	if err != nil || isolatedSource != initialSource {
		if err == nil {
			err = errors.New("contained source changed while it was isolated")
		}
		return rollbackBeforeCommit(err)
	}
	pendingSession := containedSession
	pendingSession.RolloutPath = pendingPath
	if active, err := removalWriterActive(ctx, pendingSession, options.WriterActive); err != nil {
		return rollbackBeforeCommit(err)
	} else if active {
		return rollbackBeforeCommit(errors.New("cannot remove an isolated contained session with an active writer"))
	}
	tombstone.Phase = removalPhaseIsolated
	if err := writeJSONAtomically(tombstonePath, tombstone, 0o600); err != nil {
		return rollbackBeforeCommit(fmt.Errorf("record isolated contained rollout: %w", err))
	}
	if err := cleanDatabaseReferences(ctx, tx, containedSession.ID); err != nil {
		return rollbackBeforeCommit(err)
	}
	deleteResult, err := tx.ExecContext(ctx, `delete from threads where id = ? and archived != 0 and rollout_path = ?`, containedSession.ID, containedSession.RolloutPath)
	if err != nil {
		return rollbackBeforeCommit(fmt.Errorf("delete contained thread: %w", err))
	}
	rows, err := deleteResult.RowsAffected()
	if err != nil {
		return rollbackBeforeCommit(fmt.Errorf("read contained thread delete count: %w", err))
	}
	if rows != 1 {
		return rollbackBeforeCommit(fmt.Errorf("delete contained thread affected %d rows", rows))
	}
	var commitWarning error
	if err := commitContainedTransaction(tx); err != nil {
		exists, path, stillArchived, stateErr := readContainedThreadState(ctx, db, containedSession.ID)
		switch {
		case stateErr != nil:
			return result, errors.Join(fmt.Errorf("contained-session commit outcome is unknown: %w", err), stateErr)
		case exists && stillArchived && filepath.Clean(path) == filepath.Clean(containedSession.RolloutPath):
			return rollbackBeforeCommit(fmt.Errorf("commit contained thread removal: %w", err))
		case !exists:
			commitWarning = fmt.Errorf("contained-session removal committed but commit acknowledgement failed: %w", err)
		default:
			return result, errors.Join(fmt.Errorf("contained-session commit outcome is ambiguous: %w", err), errors.New("Codex thread no longer matches either removal state"))
		}
	}
	committed = true
	result.Removed = true
	result.TombstonePath = tombstonePath
	if options.AfterCommit != nil {
		if err := options.AfterCommit(); err != nil {
			return result, errors.Join(err, commitWarning)
		}
	}
	tombstone.Phase = removalPhaseCommitted
	if err := writeJSONAtomically(tombstonePath, tombstone, 0o600); err != nil {
		return result, errors.Join(fmt.Errorf("record committed contained-session removal: %w", err), commitWarning)
	}
	finalConn, err := db.Conn(ctx)
	if err != nil {
		return result, errors.Join(err, commitWarning)
	}
	defer finalConn.Close()
	if _, err := finalConn.ExecContext(ctx, `begin immediate`); err != nil {
		return result, errors.Join(fmt.Errorf("begin contained-session finalization transaction: %w", err), commitWarning)
	}
	finalTransactionClosed := false
	defer func() {
		if !finalTransactionClosed {
			_, _ = finalConn.ExecContext(context.Background(), `rollback`)
		}
	}()
	if exists, _, _, err := readContainedThreadStateConn(ctx, finalConn, containedSession.ID); err != nil {
		return result, errors.Join(err, commitWarning)
	} else if exists {
		return result, errors.Join(errors.New("removed Codex thread reappeared before physical purge; preserving the isolated source"), commitWarning)
	}
	if err := removeGlobalStateReferences(ctx, filepath.Join(codexHome, ".codex-global-state.json"), containedSession.ID); err != nil {
		return result, errors.Join(err, commitWarning)
	}
	tombstone.Phase = removalPhaseGlobalCleaned
	if err := writeJSONAtomically(tombstonePath, tombstone, 0o600); err != nil {
		return result, errors.Join(fmt.Errorf("record cleaned Codex global state: %w", err), commitWarning)
	}
	purgeSession, err := stageIsolatedSourceForPurge(ctx, pendingSession, purgePath, initialSource, options.WriterActive)
	if err != nil {
		return result, errors.Join(err, commitWarning)
	}
	if err := syncStateDirectory(filepath.Dir(pendingPath)); err != nil {
		return result, errors.Join(fmt.Errorf("synchronize contained rollout purge staging: %w", err), commitWarning)
	}
	tombstone.Phase = removalPhasePurgeStaged
	if err := writeJSONAtomically(tombstonePath, tombstone, 0o600); err != nil {
		return result, errors.Join(fmt.Errorf("record staged contained rollout purge: %w", err), commitWarning)
	}
	if options.AfterPurgeStage != nil {
		if err := options.AfterPurgeStage(); err != nil {
			return result, errors.Join(err, commitWarning)
		}
	}
	if err := removeStagedSource(ctx, purgeSession, initialSource, options.WriterActive); err != nil {
		return result, errors.Join(err, commitWarning)
	}
	if err := syncStateDirectory(filepath.Dir(purgePath)); err != nil {
		return result, errors.Join(fmt.Errorf("synchronize contained rollout purge: %w", err), commitWarning)
	}
	if _, err := finalConn.ExecContext(ctx, `commit`); err != nil {
		return result, errors.Join(fmt.Errorf("commit contained-session finalization inspection: %w", err), commitWarning)
	}
	finalTransactionClosed = true
	tombstone.Phase = removalPhasePurged
	tombstone.RemovedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeJSONAtomically(tombstonePath, tombstone, 0o600); err != nil {
		return result, errors.Join(fmt.Errorf("record completed contained-session removal: %w", err), commitWarning)
	}
	return result, commitWarning
}

func RecoverContained(ctx context.Context, codexHome string, storeDir string, sessionID string, options Options) (RecoveryResult, error) {
	if !options.Apply {
		return RecoveryResult{}, errors.New("contained-session recovery requires explicit apply")
	}
	if err := validateSessionID(sessionID); err != nil {
		return RecoveryResult{}, err
	}
	if options.WriterActive == nil {
		return RecoveryResult{}, errors.New("contained-session recovery requires a native writer probe")
	}
	operation, err := storage.AcquireOperationLock(storeDir, removalLockName(sessionID))
	if err != nil {
		return RecoveryResult{}, fmt.Errorf("serialize contained-session recovery: %w", err)
	}
	defer func() { _ = operation.Close() }()

	tombstonePath := TombstonePath(storeDir, sessionID)
	tombstone, err := readRemovalTombstone(tombstonePath)
	if err != nil {
		return RecoveryResult{}, err
	}
	if tombstone.Version == 1 {
		if tombstone.Kind != "contained-session-removal-v1" {
			return RecoveryResult{}, errors.New("legacy removal tombstone has an unsupported kind")
		}
		tombstone.Version = 2
		tombstone.Kind = "contained-session-removal-v2"
		tombstone.Phase = removalPhasePrepared
		tombstone.PendingRolloutPath = tombstone.OriginalRolloutPath + ".codexfold-remove-pending"
		tombstone.PurgeRolloutPath = tombstone.PendingRolloutPath + ".purge"
		if tombstone.PreparedAt == "" {
			tombstone.PreparedAt = tombstone.RemovedAt
		}
		tombstone.RemovedAt = ""
	}
	if err := validateRemovalTombstone(storeDir, sessionID, tombstone); err != nil {
		return RecoveryResult{}, err
	}
	manifest, err := fold.VerifySession(ctx, storeDir, sessionID)
	if err != nil {
		return RecoveryResult{}, fmt.Errorf("verify retained recovery fold: %w", err)
	}
	if manifest.Source.Bytes != tombstone.SourceBytes || manifest.Source.SHA256 != tombstone.SourceSHA256 ||
		filepath.Clean(manifest.Session.RolloutPath) != filepath.Clean(tombstone.OriginalRolloutPath) || !manifest.Session.Archived {
		return RecoveryResult{}, errors.New("retained recovery fold no longer matches the removal tombstone")
	}

	result := RecoveryResult{
		SessionID: sessionID, SourcePath: tombstone.OriginalRolloutPath, PendingPath: tombstone.PendingRolloutPath,
	}
	db, err := sql.Open("sqlite", filepath.Join(codexHome, "state_5.sqlite"))
	if err != nil {
		return RecoveryResult{}, fmt.Errorf("open Codex state database for removal recovery: %w", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`pragma busy_timeout = 10000`); err != nil {
		return RecoveryResult{}, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return RecoveryResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `begin immediate`); err != nil {
		return RecoveryResult{}, fmt.Errorf("begin contained-session recovery transaction: %w", err)
	}
	transactionClosed := false
	defer func() {
		if !transactionClosed {
			_, _ = conn.ExecContext(context.Background(), `rollback`)
		}
	}()
	exists, rolloutPath, archived, err := readContainedThreadStateConn(ctx, conn, sessionID)
	if err != nil {
		return RecoveryResult{}, err
	}
	want := sourceSnapshot{Bytes: tombstone.SourceBytes, SHA256: tombstone.SourceSHA256}
	sourceExists, sourceMatches, err := inspectSourcePath(tombstone.OriginalRolloutPath, want)
	if err != nil {
		return RecoveryResult{}, err
	}
	pendingExists, pendingMatches, err := inspectSourcePath(tombstone.PendingRolloutPath, want)
	if err != nil {
		return RecoveryResult{}, err
	}
	purgeExists, purgeMatches, err := inspectSourcePath(tombstone.PurgeRolloutPath, want)
	if err != nil {
		return RecoveryResult{}, err
	}

	if exists {
		if !archived || filepath.Clean(rolloutPath) != filepath.Clean(tombstone.OriginalRolloutPath) {
			return RecoveryResult{}, errors.New("Codex thread no longer matches the pre-removal state")
		}
		switch {
		case sourceExists && sourceMatches && !pendingExists && !purgeExists:
		case !sourceExists && pendingExists && pendingMatches && !purgeExists:
			pendingSession := codex.Session{ID: sessionID, RolloutPath: tombstone.PendingRolloutPath, Archived: true}
			if active, err := removalWriterActive(ctx, pendingSession, options.WriterActive); err != nil {
				return RecoveryResult{}, err
			} else if active {
				return RecoveryResult{}, errors.New("cannot restore an interrupted removal while the pending rollout has an active writer")
			}
			if err := renameRemovalNoReplace(tombstone.PendingRolloutPath, tombstone.OriginalRolloutPath); err != nil {
				return RecoveryResult{}, fmt.Errorf("restore interrupted contained-session removal: %w", err)
			}
			if err := syncStateDirectory(filepath.Dir(tombstone.OriginalRolloutPath)); err != nil {
				return RecoveryResult{}, err
			}
		default:
			return RecoveryResult{}, errors.New("interrupted contained-session removal has ambiguous or changed rollout files")
		}
		if _, err := conn.ExecContext(ctx, `commit`); err != nil {
			return RecoveryResult{}, fmt.Errorf("commit contained-session rollback inspection: %w", err)
		}
		transactionClosed = true
		if err := removeRemovalTombstone(tombstonePath); err != nil {
			return RecoveryResult{}, err
		}
		result.RolledBack = true
		return result, nil
	}

	if sourceExists {
		return RecoveryResult{}, errors.New("removed Codex thread is absent but its original rollout path was recreated; preserving both states")
	}
	if pendingExists && !pendingMatches {
		return RecoveryResult{}, errors.New("pending contained rollout differs from its durable removal proof")
	}
	if purgeExists && !purgeMatches {
		return RecoveryResult{}, errors.New("staged contained rollout differs from its durable removal proof")
	}
	if pendingExists && purgeExists {
		return RecoveryResult{}, errors.New("contained-session recovery found both pending and purge-staged files; preserving both")
	}
	tombstone.Phase = removalPhaseCommitted
	if err := writeJSONAtomically(tombstonePath, tombstone, 0o600); err != nil {
		return RecoveryResult{}, err
	}
	if err := removeGlobalStateReferences(ctx, filepath.Join(codexHome, ".codex-global-state.json"), sessionID); err != nil {
		return RecoveryResult{}, err
	}
	tombstone.Phase = removalPhaseGlobalCleaned
	if err := writeJSONAtomically(tombstonePath, tombstone, 0o600); err != nil {
		return RecoveryResult{}, err
	}
	if pendingExists {
		pendingSession := codex.Session{ID: sessionID, RolloutPath: tombstone.PendingRolloutPath, Archived: true}
		purgeSession, err := stageIsolatedSourceForPurge(ctx, pendingSession, tombstone.PurgeRolloutPath, want, options.WriterActive)
		if err != nil {
			return RecoveryResult{}, err
		}
		if err := syncStateDirectory(filepath.Dir(tombstone.PendingRolloutPath)); err != nil {
			return RecoveryResult{}, err
		}
		tombstone.Phase = removalPhasePurgeStaged
		if err := writeJSONAtomically(tombstonePath, tombstone, 0o600); err != nil {
			return RecoveryResult{}, err
		}
		if err := removeStagedSource(ctx, purgeSession, want, options.WriterActive); err != nil {
			return RecoveryResult{}, err
		}
		if err := syncStateDirectory(filepath.Dir(tombstone.PurgeRolloutPath)); err != nil {
			return RecoveryResult{}, err
		}
	} else if purgeExists {
		purgeSession := codex.Session{ID: sessionID, RolloutPath: tombstone.PurgeRolloutPath, Archived: true}
		if err := removeStagedSource(ctx, purgeSession, want, options.WriterActive); err != nil {
			return RecoveryResult{}, err
		}
		if err := syncStateDirectory(filepath.Dir(tombstone.PurgeRolloutPath)); err != nil {
			return RecoveryResult{}, err
		}
	}
	if _, err := conn.ExecContext(ctx, `commit`); err != nil {
		return RecoveryResult{}, fmt.Errorf("commit contained-session finalization inspection: %w", err)
	}
	transactionClosed = true
	tombstone.Phase = removalPhasePurged
	if tombstone.RemovedAt == "" {
		tombstone.RemovedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if err := writeJSONAtomically(tombstonePath, tombstone, 0o600); err != nil {
		return RecoveryResult{}, err
	}
	result.Finalized = true
	return result, nil
}

func stableSourceSnapshot(path string) (sourceSnapshot, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return sourceSnapshot{}, fmt.Errorf("inspect contained source: %w", err)
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 {
		return sourceSnapshot{}, errors.New("contained source is not a plain regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return sourceSnapshot{}, fmt.Errorf("open contained source: %w", err)
	}
	openedBefore, err := file.Stat()
	if err != nil || !os.SameFile(pathInfo, openedBefore) {
		_ = file.Close()
		if err == nil {
			err = errors.New("contained source changed while it was opened")
		}
		return sourceSnapshot{}, err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(hasher, file)
	openedAfter, statErr := file.Stat()
	closeErr := file.Close()
	if copyErr != nil {
		return sourceSnapshot{}, copyErr
	}
	if statErr != nil {
		return sourceSnapshot{}, statErr
	}
	if closeErr != nil {
		return sourceSnapshot{}, closeErr
	}
	pathAfter, err := os.Lstat(path)
	if err != nil || !pathAfter.Mode().IsRegular() || pathAfter.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(openedBefore, openedAfter) || !os.SameFile(openedAfter, pathAfter) ||
		openedBefore.Size() != openedAfter.Size() || !openedBefore.ModTime().Equal(openedAfter.ModTime()) ||
		written != openedAfter.Size() {
		if err == nil {
			err = errors.New("contained source changed while it was hashed")
		}
		return sourceSnapshot{}, err
	}
	return sourceSnapshot{Bytes: written, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

func inspectSourcePath(path string, want sourceSnapshot) (bool, bool, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	} else if err != nil {
		return false, false, err
	}
	got, err := stableSourceSnapshot(path)
	if err != nil {
		return true, false, err
	}
	return true, got == want, nil
}

func removalWriterActive(ctx context.Context, session codex.Session, probe func(context.Context, codex.Session) (bool, error)) (bool, error) {
	if probe == nil {
		return false, nil
	}
	active, err := probe(ctx, session)
	if err != nil {
		return false, fmt.Errorf("probe contained-session writer: %w", err)
	}
	return active, nil
}

func restoreIsolatedSource(ctx context.Context, session codex.Session, pendingPath string, want sourceSnapshot, probe func(context.Context, codex.Session) (bool, error)) error {
	if _, err := os.Lstat(session.RolloutPath); err == nil {
		return errors.New("cannot restore isolated rollout because the original path exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	pendingSession := session
	pendingSession.RolloutPath = pendingPath
	if active, err := removalWriterActive(ctx, pendingSession, probe); err != nil {
		return err
	} else if active {
		return errors.New("cannot restore isolated rollout with an active writer")
	}
	got, err := stableSourceSnapshot(pendingPath)
	if err != nil || got != want {
		if err == nil {
			err = errors.New("isolated rollout changed before rollback")
		}
		return err
	}
	if err := renameRemovalNoReplace(pendingPath, session.RolloutPath); err != nil {
		return fmt.Errorf("restore contained rollout: %w", err)
	}
	return syncStateDirectory(filepath.Dir(session.RolloutPath))
}

func stageIsolatedSourceForPurge(ctx context.Context, session codex.Session, purgePath string, want sourceSnapshot, probe func(context.Context, codex.Session) (bool, error)) (codex.Session, error) {
	got, err := stableSourceSnapshot(session.RolloutPath)
	if err != nil || got != want {
		if err == nil {
			err = errors.New("isolated contained rollout changed before purge")
		}
		return codex.Session{}, err
	}
	if active, err := removalWriterActive(ctx, session, probe); err != nil {
		return codex.Session{}, err
	} else if active {
		return codex.Session{}, errors.New("cannot purge an isolated contained rollout with an active writer")
	}
	if _, err := os.Lstat(purgePath); err == nil {
		return codex.Session{}, errors.New("contained rollout purge staging path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return codex.Session{}, err
	}
	if err := renameRemovalNoReplace(session.RolloutPath, purgePath); err != nil {
		return codex.Session{}, fmt.Errorf("stage isolated contained rollout for exact purge: %w", err)
	}
	purgeSession := session
	purgeSession.RolloutPath = purgePath
	verified, err := stableSourceSnapshot(purgePath)
	if err != nil || verified != want {
		if err == nil {
			err = errors.New("purge-staged contained rollout differs from its durable proof; preserving it")
		}
		return codex.Session{}, err
	}
	return purgeSession, nil
}

func removeStagedSource(ctx context.Context, session codex.Session, want sourceSnapshot, probe func(context.Context, codex.Session) (bool, error)) error {
	if active, err := removalWriterActive(ctx, session, probe); err != nil {
		return err
	} else if active {
		return errors.New("cannot remove a purge-staged contained rollout with an active writer")
	}
	verified, err := stableSourceSnapshot(session.RolloutPath)
	if err != nil || verified != want {
		if err == nil {
			err = errors.New("purge-staged contained rollout changed at the remove boundary")
		}
		return err
	}
	if err := os.Remove(session.RolloutPath); err != nil {
		return fmt.Errorf("remove isolated contained rollout after database commit: %w", err)
	}
	return nil
}

func removalLockName(sessionID string) string {
	digest := sha256.Sum256([]byte(sessionID))
	return "remove-contained-" + hex.EncodeToString(digest[:])
}

func readContainedThreadState(ctx context.Context, db *sql.DB, sessionID string) (bool, string, bool, error) {
	var path string
	var archived int
	err := db.QueryRowContext(ctx, `select rollout_path, archived from threads where id = ?`, sessionID).Scan(&path, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", false, nil
	}
	if err != nil {
		return false, "", false, err
	}
	return true, path, archived != 0, nil
}

func readContainedThreadStateConn(ctx context.Context, conn *sql.Conn, sessionID string) (bool, string, bool, error) {
	var path string
	var archived int
	err := conn.QueryRowContext(ctx, `select rollout_path, archived from threads where id = ?`, sessionID).Scan(&path, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", false, nil
	}
	if err != nil {
		return false, "", false, err
	}
	return true, path, archived != 0, nil
}

func removeGlobalStateReferences(ctx context.Context, path string, sessionID string) error {
	for attempt := 0; attempt < 5; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		original, cleaned, mode, exists, err := cleanGlobalState(path, sessionID)
		if err != nil {
			return err
		}
		if !exists || bytes.Equal(original, cleaned) {
			return nil
		}
		if err := replaceBytesIfUnchanged(path, original, cleaned, mode); err == nil {
			return nil
		} else if !errors.Is(err, errGlobalStateChanged) {
			return err
		}
	}
	return errors.New("Codex global state kept changing; removal remains recoverable and pending")
}

func removeRemovalTombstone(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncStateDirectory(filepath.Dir(path))
}

func readRemovalTombstone(path string) (Tombstone, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Tombstone{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 1<<20 {
		return Tombstone{}, errors.New("removal tombstone is not a bounded plain regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Tombstone{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var tombstone Tombstone
	if err := decoder.Decode(&tombstone); err != nil {
		return Tombstone{}, fmt.Errorf("decode removal tombstone: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Tombstone{}, fmt.Errorf("decode removal tombstone: %w", err)
	}
	return tombstone, nil
}

func validateRemovalTombstone(storeDir string, sessionID string, tombstone Tombstone) error {
	if tombstone.Version != 2 || tombstone.Kind != "contained-session-removal-v2" ||
		tombstone.ContainedSessionID != sessionID || tombstone.ContainerSessionID == "" ||
		tombstone.ContainerSessionID == sessionID {
		return errors.New("removal tombstone does not match the recovery request")
	}
	if err := validateSessionID(tombstone.ContainerSessionID); err != nil {
		return err
	}
	if !filepath.IsAbs(tombstone.OriginalRolloutPath) ||
		tombstone.PendingRolloutPath != tombstone.OriginalRolloutPath+".codexfold-remove-pending" ||
		tombstone.PurgeRolloutPath != tombstone.PendingRolloutPath+".purge" ||
		tombstone.RecoveryManifestPath != fold.ManifestPath(storeDir, sessionID) ||
		tombstone.PreparedAt == "" || tombstone.SourceBytes < 0 || len(tombstone.SourceSHA256) != 64 {
		return errors.New("removal tombstone contains invalid paths or source identity")
	}
	switch tombstone.Phase {
	case removalPhasePrepared, removalPhaseIsolated, removalPhaseCommitted, removalPhaseGlobalCleaned, removalPhasePurgeStaged, removalPhasePurged:
	default:
		return errors.New("removal tombstone has an invalid phase")
	}
	if tombstone.Phase == removalPhasePurged && tombstone.RemovedAt == "" {
		return errors.New("completed removal tombstone has no completion time")
	}
	if _, err := hex.DecodeString(tombstone.SourceSHA256); err != nil {
		return errors.New("removal tombstone has an invalid source digest")
	}
	return nil
}

func cleanDatabaseReferences(ctx context.Context, tx *sql.Tx, sessionID string) error {
	for _, operation := range []struct {
		table string
		query string
		args  []any
	}{
		{table: "thread_dynamic_tools", query: `delete from thread_dynamic_tools where thread_id = ?`, args: []any{sessionID}},
		{table: "thread_spawn_edges", query: `delete from thread_spawn_edges where parent_thread_id = ? or child_thread_id = ?`, args: []any{sessionID, sessionID}},
		{table: "agent_job_items", query: `update agent_job_items set assigned_thread_id = null where assigned_thread_id = ?`, args: []any{sessionID}},
	} {
		exists, err := tableExists(ctx, tx, operation.table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := tx.ExecContext(ctx, operation.query, operation.args...); err != nil {
			return fmt.Errorf("clean %s references: %w", operation.table, err)
		}
	}
	return nil
}

func tableExists(ctx context.Context, tx *sql.Tx, table string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `select count(*) from sqlite_master where type = 'table' and name = ?`, table).Scan(&count); err != nil {
		return false, err
	}
	return count != 0, nil
}

func cleanGlobalState(path string, sessionID string) ([]byte, []byte, os.FileMode, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil, 0, false, nil
	}
	if err != nil {
		return nil, nil, 0, false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, 0, false, err
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, nil, 0, false, fmt.Errorf("decode Codex global state: %w", err)
	}
	cleaned, _ := removeIDReferences(value, sessionID)
	if hasIDReference(cleaned, sessionID) {
		return nil, nil, 0, false, errors.New("Codex global state still contains the removed session id")
	}
	encoded, err := json.MarshalIndent(cleaned, "", "  ")
	if err != nil {
		return nil, nil, 0, false, err
	}
	encoded = append(encoded, '\n')
	return data, encoded, info.Mode().Perm(), true, nil
}

func removeIDReferences(value any, sessionID string) (any, bool) {
	switch typed := value.(type) {
	case string:
		return typed, typed == sessionID
	case []any:
		cleaned := make([]any, 0, len(typed))
		for _, item := range typed {
			value, remove := removeIDReferences(item, sessionID)
			if !remove {
				cleaned = append(cleaned, value)
			}
		}
		return cleaned, false
	case map[string]any:
		for key, item := range typed {
			if key == sessionID {
				delete(typed, key)
				continue
			}
			value, remove := removeIDReferences(item, sessionID)
			if remove {
				delete(typed, key)
			} else {
				typed[key] = value
			}
		}
		return typed, false
	default:
		return value, false
	}
}

func hasIDReference(value any, sessionID string) bool {
	switch typed := value.(type) {
	case string:
		return typed == sessionID
	case []any:
		for _, item := range typed {
			if hasIDReference(item, sessionID) {
				return true
			}
		}
	case map[string]any:
		for key, item := range typed {
			if key == sessionID || hasIDReference(item, sessionID) {
				return true
			}
		}
	}
	return false
}

func writeJSONAtomically(path string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeBytesAtomically(path, append(data, '\n'), mode)
}

func writeBytesAtomically(path string, data []byte, mode os.FileMode) error {
	if mode == 0 {
		mode = 0o600
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".codexfold-state-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := replaceStateFile(temporaryPath, path); err != nil {
		return err
	}
	return syncStateDirectory(filepath.Dir(path))
}

func replaceBytesIfUnchanged(path string, expected []byte, replacement []byte, mode os.FileMode) error {
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, expected) {
		return errGlobalStateChanged
	}
	return writeBytesAtomically(path, replacement, mode)
}

func validateSessionID(sessionID string) error {
	if sessionID == "" || sessionID == "." || sessionID == ".." || strings.ContainsAny(sessionID, "/\\\x00") {
		return fmt.Errorf("unsafe session id %q", sessionID)
	}
	return nil
}

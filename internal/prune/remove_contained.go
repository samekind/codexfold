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

	"github.com/jstar0/codexfold/internal/codex"
	"github.com/jstar0/codexfold/internal/contain"
	"github.com/jstar0/codexfold/internal/fold"
	_ "modernc.org/sqlite"
)

type Options struct {
	Apply              bool
	IncludeSessionMeta bool
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
	Version              int    `json:"version"`
	Kind                 string `json:"kind"`
	RemovedAt            string `json:"removed_at"`
	ContainedSessionID   string `json:"contained_session_id"`
	ContainerSessionID   string `json:"container_session_id"`
	OriginalRolloutPath  string `json:"original_rollout_path"`
	SourceBytes          int64  `json:"source_bytes"`
	SourceSHA256         string `json:"source_sha256"`
	RecoveryManifestPath string `json:"recovery_manifest_path"`
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
	if err := verifySourceFile(containedSession.RolloutPath, manifest.Source.Bytes, manifest.Source.SHA256); err != nil {
		return Result{}, err
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

	tombstone := Tombstone{
		Version: 1, Kind: "contained-session-removal-v1", RemovedAt: time.Now().UTC().Format(time.RFC3339Nano),
		ContainedSessionID: containedSession.ID, ContainerSessionID: containerSession.ID,
		OriginalRolloutPath: containedSession.RolloutPath, SourceBytes: manifest.Source.Bytes,
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
	removeTombstone := true
	defer func() {
		if removeTombstone {
			_ = os.Remove(tombstonePath)
		}
	}()

	globalPath := filepath.Join(codexHome, ".codex-global-state.json")
	originalGlobal, cleanedGlobal, globalMode, globalExists, err := cleanGlobalState(globalPath, containedSession.ID)
	if err != nil {
		return Result{}, err
	}
	db, err := sql.Open("sqlite", filepath.Join(codexHome, "state_5.sqlite"))
	if err != nil {
		return Result{}, fmt.Errorf("open Codex state database for removal: %w", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`pragma busy_timeout = 10000`); err != nil {
		return Result{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("begin Codex removal transaction: %w", err)
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
		return Result{}, fmt.Errorf("revalidate contained thread: %w", err)
	}
	if archived == 0 || filepath.Clean(dbRolloutPath) != filepath.Clean(containedSession.RolloutPath) {
		return Result{}, errors.New("contained thread changed before removal")
	}
	pendingPath := containedSession.RolloutPath + ".codexfold-remove-pending"
	if _, err := os.Stat(pendingPath); err == nil {
		return Result{}, fmt.Errorf("pending removal path already exists: %s", pendingPath)
	} else if !os.IsNotExist(err) {
		return Result{}, err
	}
	if err := os.Rename(containedSession.RolloutPath, pendingPath); err != nil {
		return Result{}, fmt.Errorf("isolate contained rollout: %w", err)
	}
	sourceIsolated := true
	globalChanged := false
	rollbackFiles := func() error {
		var rollbackErrors []error
		if globalChanged {
			if err := replaceBytesIfUnchanged(globalPath, cleanedGlobal, originalGlobal, globalMode); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("restore Codex global state: %w", err))
			} else {
				globalChanged = false
			}
		}
		if sourceIsolated {
			if err := os.Rename(pendingPath, containedSession.RolloutPath); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("restore contained rollout: %w", err))
			} else {
				sourceIsolated = false
			}
		}
		return errors.Join(rollbackErrors...)
	}
	if globalExists && !bytes.Equal(originalGlobal, cleanedGlobal) {
		if err := replaceBytesIfUnchanged(globalPath, originalGlobal, cleanedGlobal, globalMode); err != nil {
			return Result{}, errors.Join(err, rollbackFiles())
		}
		globalChanged = true
	}
	if err := cleanDatabaseReferences(ctx, tx, containedSession.ID); err != nil {
		return Result{}, errors.Join(err, rollbackFiles())
	}
	deleteResult, err := tx.ExecContext(ctx, `delete from threads where id = ? and archived != 0`, containedSession.ID)
	if err != nil {
		return Result{}, errors.Join(fmt.Errorf("delete contained thread: %w", err), rollbackFiles())
	}
	rows, err := deleteResult.RowsAffected()
	if err != nil {
		return Result{}, errors.Join(fmt.Errorf("read contained thread delete count: %w", err), rollbackFiles())
	}
	if rows != 1 {
		return Result{}, errors.Join(fmt.Errorf("delete contained thread affected %d rows", rows), rollbackFiles())
	}
	if err := tx.Commit(); err != nil {
		return Result{}, errors.Join(fmt.Errorf("commit contained thread removal: %w", err), rollbackFiles())
	}
	committed = true
	removeTombstone = false
	result.Removed = true
	result.TombstonePath = tombstonePath
	if err := os.Remove(pendingPath); err != nil {
		return result, fmt.Errorf("remove isolated contained rollout after database commit: %w", err)
	}
	sourceIsolated = false
	return result, nil
}

func verifySourceFile(path string, wantBytes int64, wantSHA string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open contained source: %w", err)
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written != wantBytes || hex.EncodeToString(hasher.Sum(nil)) != wantSHA {
		return errors.New("contained source differs from its recovery fold")
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
		return errors.New("Codex global state changed concurrently; refusing to overwrite it")
	}
	return writeBytesAtomically(path, replacement, mode)
}

func validateSessionID(sessionID string) error {
	if sessionID == "" || sessionID == "." || sessionID == ".." || strings.ContainsAny(sessionID, "/\\\x00") {
		return fmt.Errorf("unsafe session id %q", sessionID)
	}
	return nil
}

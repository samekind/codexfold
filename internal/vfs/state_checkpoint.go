package vfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/storage"
)

const (
	sessionStateCheckpointVersion = 1
	sessionStateCatalogVersion    = 1
	stateGenerationsDirectoryName = "state-generations"
	stateCatalogFilename          = "state-catalog.json"
	maximumStateCheckpointBytes   = 1 << 20
)

type sessionStateFileIdentity struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type sessionStateCheckpoint struct {
	Version                  int                       `json:"version"`
	SessionID                string                    `json:"session_id"`
	Sequence                 uint64                    `json:"sequence"`
	PreviousCheckpointSHA256 string                    `json:"previous_checkpoint_sha256,omitempty"`
	State                    SessionState              `json:"state"`
	StateSHA256              string                    `json:"state_sha256"`
	Manifest                 sessionStateFileIdentity  `json:"manifest"`
	Delta                    sessionStateFileIdentity  `json:"delta"`
	Backing                  *sessionStateFileIdentity `json:"backing,omitempty"`
	NativeSnapshot           *sessionStateFileIdentity `json:"native_snapshot,omitempty"`
}

type sessionStateCatalog struct {
	Version          int    `json:"version"`
	SessionID        string `json:"session_id"`
	Generation       uint64 `json:"generation"`
	Sequence         uint64 `json:"sequence"`
	StateSHA256      string `json:"state_sha256"`
	Checkpoint       string `json:"checkpoint"`
	CheckpointSHA256 string `json:"checkpoint_sha256"`
}

type stateCheckpointChain struct {
	catalog    sessionStateCatalog
	checkpoint sessionStateCheckpoint
	lineage    map[string]sessionStateCheckpoint
}

var stateCheckpointBytesHashed func(int64)
var stateCheckpointCandidateSynced func(string)
var sessionCheckpointLockTimeout = 10 * time.Second

func publishSessionState(path string, state SessionState) error {
	return publishSessionStateAt(path, "", state, filepath.Dir(path), nil, false)
}

func publishSessionStateWithTemporary(path string, temporaryPath string, state SessionState) error {
	return publishSessionStateAt(path, temporaryPath, state, filepath.Dir(path), nil, false)
}

func publishInitialSessionState(path string, state SessionState, finalDirectory string, stagedDeltaPath string) error {
	return publishSessionStateAt(path, "", state, finalDirectory, map[string]string{
		filepath.Clean(state.DeltaPath): filepath.Clean(stagedDeltaPath),
	}, true)
}

func publishSessionStateAt(path string, temporaryPath string, state SessionState, sessionDirectory string, sources map[string]string, allowOrphanAdoption bool) error {
	sessionDirectory = filepath.Clean(sessionDirectory)
	if err := validateSessionStateForDirectory(state, sessionDirectory); err != nil {
		return err
	}
	if allowOrphanAdoption {
		return publishSessionStateAtLocked(path, temporaryPath, state, sessionDirectory, sources, true)
	}
	checkpointLock, err := acquireSessionCheckpointLock(sessionDirectory, state.SessionID)
	if err != nil {
		return err
	}
	defer checkpointLock.Close()
	return publishSessionStateAtLocked(path, temporaryPath, state, sessionDirectory, sources, allowOrphanAdoption)
}

func publishSessionStateAtLocked(path string, temporaryPath string, state SessionState, sessionDirectory string, sources map[string]string, allowOrphanAdoption bool) error {
	metadataDirectory := filepath.Clean(filepath.Dir(path))
	stateData, err := encodeSessionState(state)
	if err != nil {
		return err
	}
	stateSHA256 := digestStateBytes(stateData)

	chain, chainErr := loadStateCheckpointChain(metadataDirectory, state.SessionID, true)
	if chainErr != nil {
		if catalogPresent, err := regularStateCatalogPresent(metadataDirectory); err != nil {
			return err
		} else if catalogPresent {
			if err := reconcileStateCheckpointHistory(metadataDirectory, state.SessionID); err != nil {
				return fmt.Errorf("reconcile session state checkpoint history: %w", err)
			}
			chain, chainErr = loadStateCheckpointChain(metadataDirectory, state.SessionID, true)
		}
	}
	var previousSHA256 string
	var sequence uint64 = 1
	switch {
	case chainErr == nil:
		sequence = chain.catalog.Sequence + 1
		if chain.catalog.Generation > state.Generation {
			return errors.New("session state checkpoint generation cannot move backwards")
		}
		if chain.catalog.Generation == state.Generation && chain.catalog.StateSHA256 != stateSHA256 {
			return errors.New("session state checkpoint generation conflicts with its catalog")
		}
		if chain.catalog.Generation == state.Generation && chain.checkpoint.State == state {
			if err := verifyStateCheckpointArtifactsForChain(sessionDirectory, chain); err == nil {
				return commitPublishedSessionState(path, temporaryPath, state)
			}
		}
		if chain.catalog.Generation == state.Generation && chain.checkpoint.State != state {
			if chain.catalog.StateSHA256 != stateSHA256 {
				return errors.New("session state checkpoint generation conflicts with its catalog")
			}
		}
		previousSHA256 = chain.catalog.CheckpointSHA256
	case errors.Is(chainErr, os.ErrNotExist):
	default:
		return fmt.Errorf("load current session state checkpoint: %w", chainErr)
	}

	checkpoint, err := buildStateCheckpoint(state, stateSHA256, previousSHA256, sequence, sessionDirectory, sources)
	if err != nil {
		return err
	}
	checkpointData, err := encodeStateCheckpoint(checkpoint)
	if err != nil {
		return err
	}
	checkpointSHA256 := digestStateBytes(checkpointData)
	checkpointName := stateCheckpointFilename(state.Generation, checkpoint.Sequence, checkpointSHA256)
	if previousSHA256 == "" {
		if err := validateInitialCheckpointPublication(metadataDirectory, checkpointName, checkpointData, allowOrphanAdoption); err != nil {
			return err
		}
	}
	if err := writeImmutableStateCheckpoint(metadataDirectory, checkpointName, checkpointData); err != nil {
		return err
	}
	catalog := sessionStateCatalog{
		Version: sessionStateCatalogVersion, SessionID: state.SessionID, Generation: state.Generation,
		Sequence: checkpoint.Sequence, StateSHA256: stateSHA256, Checkpoint: checkpointName, CheckpointSHA256: checkpointSHA256,
	}
	if err := writeStateCatalog(metadataDirectory, catalog); err != nil {
		return err
	}
	return commitPublishedSessionState(path, temporaryPath, state)
}

func commitPublishedSessionState(path string, temporaryPath string, state SessionState) error {
	if temporaryPath != "" {
		return writeSessionStateWithTemporary(path, temporaryPath, state)
	}
	return writeSessionState(path, state)
}

func refreshSessionStateCheckpoint(path string, state SessionState) error {
	return refreshSessionStateCheckpointWithActiveIdentity(path, state, nil)
}

func refreshSessionStateCheckpointWithActiveIdentity(path string, state SessionState, activeIdentity *sessionStateFileIdentity) error {
	path = filepath.Clean(path)
	directory := filepath.Dir(path)
	if err := validateSessionStateForDirectory(state, directory); err != nil {
		return err
	}
	checkpointLock, err := acquireSessionCheckpointLock(directory, state.SessionID)
	if err != nil {
		return err
	}
	defer checkpointLock.Close()
	return refreshSessionStateCheckpointWithActiveIdentityLocked(path, state, activeIdentity)
}

func refreshSessionStateCheckpointWithActiveIdentityLocked(path string, state SessionState, activeIdentity *sessionStateFileIdentity) error {
	path = filepath.Clean(path)
	directory := filepath.Dir(path)
	persisted, data, err := readSessionState(path)
	if err != nil {
		return fmt.Errorf("load session state before checkpoint refresh: %w", err)
	}
	if persisted != state {
		return errors.New("persisted session state changed before checkpoint refresh")
	}
	chain, err := loadCatalogStateCheckpoint(directory, state.SessionID)
	if err != nil {
		return fmt.Errorf("load session checkpoint before refresh: %w", err)
	}
	stateSHA256 := digestStateBytes(data)
	if chain.catalog.StateSHA256 != stateSHA256 || chain.checkpoint.State != state {
		return errors.New("session checkpoint catalog does not match persisted state")
	}
	previousCheckpointSHA256 := chain.checkpoint.PreviousCheckpointSHA256
	removeSupersededCheckpoint := true
	pinnedCheckpointSHA256, pinned, err := retirementCheckpointPin(directory)
	if err != nil {
		return err
	}
	if pinned {
		lineageChain, lineageErr := loadStateCheckpointChain(directory, state.SessionID, true)
		if lineageErr != nil {
			return fmt.Errorf("load pinned retirement checkpoint lineage: %w", lineageErr)
		}
		if _, exists := lineageChain.lineage[pinnedCheckpointSHA256]; !exists {
			return errors.New("retirement request checkpoint is not in the current state lineage")
		}
		previousCheckpointSHA256 = chain.catalog.CheckpointSHA256
		removeSupersededCheckpoint = false
	}
	checkpoint, err := buildRefreshedStateCheckpoint(state, stateSHA256, previousCheckpointSHA256, chain.catalog.Sequence+1, directory, chain.checkpoint, activeIdentity)
	if err != nil {
		return err
	}
	if sameStateCheckpointArtifacts(checkpoint, chain.checkpoint) {
		return nil
	}
	checkpointData, err := encodeStateCheckpoint(checkpoint)
	if err != nil {
		return err
	}
	checkpointSHA256 := digestStateBytes(checkpointData)
	checkpointName := stateCheckpointFilename(state.Generation, checkpoint.Sequence, checkpointSHA256)
	if err := writeImmutableStateCheckpoint(directory, checkpointName, checkpointData); err != nil {
		return err
	}
	if err := writeStateCatalog(directory, sessionStateCatalog{
		Version: sessionStateCatalogVersion, SessionID: state.SessionID,
		Generation: state.Generation, Sequence: checkpoint.Sequence, StateSHA256: stateSHA256,
		Checkpoint: checkpointName, CheckpointSHA256: checkpointSHA256,
	}); err != nil {
		return err
	}
	oldCheckpointPath := filepath.Join(directory, stateGenerationsDirectoryName, chain.catalog.Checkpoint)
	if removeSupersededCheckpoint && oldCheckpointPath != filepath.Join(directory, stateGenerationsDirectoryName, checkpointName) {
		if err := os.Remove(oldCheckpointPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove superseded session state checkpoint: %w", err)
		}
		if err := syncStateDirectory(filepath.Join(directory, stateGenerationsDirectoryName)); err != nil {
			return err
		}
	}
	return reconcileStateCheckpointHistory(directory, state.SessionID)
}

func sessionDeletionCheckpointLockName(sessionID string) string {
	return "session-checkpoint-" + sessionID
}

func acquireSessionCheckpointLock(directory string, sessionID string) (*storage.OperationLock, error) {
	storeRoot := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Clean(directory))))
	return storage.AcquireOperationLockWithin(storeRoot, sessionDeletionCheckpointLockName(sessionID), sessionCheckpointLockTimeout)
}

func retirementCheckpointPin(directory string) (string, bool, error) {
	deletionPin, deletionPinned, err := sessionDeletionCheckpointPin(directory)
	if err != nil {
		return "", false, err
	}
	if deletionPinned {
		return deletionPin, true, nil
	}
	path := filepath.Join(filepath.Clean(directory), "retire.request.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return deletionPin, deletionPinned, nil
	}
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return "", false, errors.New("retirement request checkpoint pin is not a bounded regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	var request struct {
		CheckpointSHA256 string `json:"checkpoint_sha256"`
	}
	if err := json.Unmarshal(data, &request); err != nil || !validStateSHA256(request.CheckpointSHA256) {
		return "", false, errors.New("retirement request contains an invalid checkpoint pin")
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(info, after) || after.Size() != int64(len(data)) {
		if err == nil {
			err = errors.New("retirement request checkpoint pin changed while it was read")
		}
		return "", false, err
	}
	return request.CheckpointSHA256, true, nil
}

func sessionDeletionCheckpointPin(directory string) (string, bool, error) {
	directory = filepath.Clean(directory)
	sessionID := filepath.Base(directory)
	if !safeSessionID(sessionID) || filepath.Base(filepath.Dir(directory)) != "sessions" || filepath.Base(filepath.Dir(filepath.Dir(directory))) != "fs" {
		return "", false, nil
	}
	storeRoot := filepath.Dir(filepath.Dir(filepath.Dir(directory)))
	tombstone, err := LoadSessionDeletion(storeRoot, sessionID)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if tombstone.Version < sessionDeletionVersion {
		return "", false, nil
	}
	if tombstone.SessionPath != directory || tombstone.InitialCheckpointSequence == 0 || !validStateSHA256(tombstone.InitialCheckpointSHA256) {
		return "", false, errors.New("session deletion contains an invalid checkpoint pin")
	}
	return tombstone.InitialCheckpointSHA256, true, nil
}

func buildRefreshedStateCheckpoint(state SessionState, stateSHA256 string, previousSHA256 string, sequence uint64, directory string, current sessionStateCheckpoint, activeIdentity *sessionStateFileIdentity) (sessionStateCheckpoint, error) {
	if current.State != state || current.StateSHA256 != stateSHA256 {
		return sessionStateCheckpoint{}, errors.New("current checkpoint does not match state refresh")
	}
	checkpoint := current
	checkpoint.Sequence = sequence
	checkpoint.PreviousCheckpointSHA256 = previousSHA256
	if state.BackingPath == "" {
		identity, err := refreshedActiveIdentity(state.DeltaPath, activeIdentity)
		if err != nil {
			return sessionStateCheckpoint{}, err
		}
		checkpoint.Delta = identity
	} else {
		identity, err := refreshedActiveIdentity(state.BackingPath, activeIdentity)
		if err != nil {
			return sessionStateCheckpoint{}, err
		}
		checkpoint.Backing = &identity
	}
	if err := validateStateCheckpoint(checkpoint, directory); err != nil {
		return sessionStateCheckpoint{}, err
	}
	return checkpoint, nil
}

func refreshedActiveIdentity(path string, provided *sessionStateFileIdentity) (sessionStateFileIdentity, error) {
	path = filepath.Clean(path)
	if provided != nil {
		if provided.Path != path || validateCheckpointFileIdentity(*provided) != nil {
			return sessionStateFileIdentity{}, errors.New("provided active session identity is invalid")
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != provided.Bytes {
			return sessionStateFileIdentity{}, errors.New("provided active session identity no longer matches its file")
		}
		return *provided, nil
	}
	return captureRegularFileIdentity(path)
}

func sameStateCheckpointArtifacts(left sessionStateCheckpoint, right sessionStateCheckpoint) bool {
	return left.State == right.State &&
		left.StateSHA256 == right.StateSHA256 &&
		left.Manifest == right.Manifest &&
		left.Delta == right.Delta &&
		equalCheckpointIdentity(left.Backing, right.Backing) &&
		equalCheckpointIdentity(left.NativeSnapshot, right.NativeSnapshot)
}

func equalCheckpointIdentity(left *sessionStateFileIdentity, right *sessionStateFileIdentity) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func migrateLegacySessionState(path string) (SessionState, error) {
	path = filepath.Clean(path)
	directory := filepath.Dir(path)
	sessionID := filepath.Base(directory)
	if filepath.Base(path) != "state.json" || !safeSessionID(sessionID) || filepath.Base(filepath.Dir(directory)) != "sessions" {
		return SessionState{}, errors.New("legacy session state path is not canonical")
	}
	lease, err := acquireWriterLease(filepath.Join(directory, "writer.lease"))
	if err != nil {
		return SessionState{}, fmt.Errorf("acquire legacy state migration lease: %w", err)
	}
	defer func() {
		_ = unlockWriterFile(lease)
		_ = lease.Close()
	}()

	legacy, data, err := readSessionState(path)
	if err == nil {
		return legacy, nil
	}
	if !errors.Is(err, errLegacySessionState) {
		return SessionState{}, err
	}
	var strictLegacy SessionState
	if err := decodeStrictJSON(data, &strictLegacy); err != nil || strictLegacy != legacy {
		return SessionState{}, errors.New("legacy session state does not use the recognized schema")
	}
	upgraded, err := upgradeLegacySessionStateValue(legacy, directory)
	if err != nil {
		return SessionState{}, err
	}
	stateData, err := encodeSessionState(upgraded)
	if err != nil {
		return SessionState{}, err
	}
	checkpoint, err := buildStateCheckpoint(upgraded, digestStateBytes(stateData), "", 1, directory, nil)
	if err != nil {
		return SessionState{}, fmt.Errorf("capture legacy session recovery evidence: %w", err)
	}
	allowedJournalData, err := legacyMigrationJournalData(directory, legacy.SessionID)
	if err != nil {
		return SessionState{}, err
	}
	if err := verifyNoUnknownNonemptySessionDataAllowed(directory, checkpoint, allowedJournalData); err != nil {
		return SessionState{}, err
	}
	if err := publishSessionStateAt(path, "", upgraded, directory, nil, true); err != nil {
		return SessionState{}, fmt.Errorf("publish migrated session state: %w", err)
	}
	return upgraded, nil
}

func upgradeLegacySessionStateValue(legacy SessionState, directory string) (SessionState, error) {
	if legacy.Version != legacySessionStateVersion || legacy.ManifestSHA256 != "" {
		return SessionState{}, errors.New("session state is not a recognized legacy value")
	}
	manifest, _, err := captureManifestIdentity(legacy.ManifestPath, legacy.SessionID, legacy.BaseBytes, legacy.BaseSHA256)
	if err != nil {
		return SessionState{}, fmt.Errorf("verify legacy session manifest: %w", err)
	}
	upgraded := legacy
	upgraded.Version = sessionStateVersion
	upgraded.ManifestSHA256 = manifest.SHA256
	if err := validateSessionStateForDirectory(upgraded, directory); err != nil {
		return SessionState{}, err
	}
	return upgraded, nil
}

func legacyMigrationJournalData(directory string, sessionID string) (map[string]struct{}, error) {
	records, err := readJournal(directory)
	if err != nil {
		return nil, err
	}
	latest, err := latestJournalRecords(records)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{})
	for _, record := range latest {
		if record.Phase == "complete" || record.Phase == "rolled-back" {
			continue
		}
		switch record.Phase {
		case "prepared", "data-synced", "after-file-publish", "state-publishing", "state-published":
		default:
			return nil, fmt.Errorf("legacy session journal operation %s has unknown phase %q", record.OperationID, record.Phase)
		}
		if record.SessionID != "" && record.SessionID != sessionID {
			return nil, errors.New("legacy session journal belongs to another session")
		}
		for _, candidate := range []string{record.TempPath, record.FinalPath, record.Native.Path} {
			if candidate == "" {
				continue
			}
			candidate = filepath.Clean(candidate)
			if pathWithin(directory, candidate) && managedSessionDataName(filepath.Base(candidate)) {
				allowed[candidate] = struct{}{}
			}
		}
	}
	return allowed, nil
}

func buildStateCheckpoint(state SessionState, stateSHA256 string, previousSHA256 string, sequence uint64, sessionDirectory string, sources map[string]string) (sessionStateCheckpoint, error) {
	manifest, _, err := captureManifestIdentity(state.ManifestPath, state.SessionID, state.BaseBytes, state.BaseSHA256)
	if err != nil {
		return sessionStateCheckpoint{}, fmt.Errorf("capture session manifest identity: %w", err)
	}
	if manifest.SHA256 != state.ManifestSHA256 {
		return sessionStateCheckpoint{}, errors.New("session manifest bytes differ from state")
	}
	delta, err := captureCheckpointFile(state.DeltaPath, sources)
	if err != nil {
		return sessionStateCheckpoint{}, fmt.Errorf("capture session delta identity: %w", err)
	}
	checkpoint := sessionStateCheckpoint{
		Version: sessionStateCheckpointVersion, SessionID: state.SessionID, Sequence: sequence,
		PreviousCheckpointSHA256: previousSHA256, State: state, StateSHA256: stateSHA256,
		Manifest: manifest, Delta: delta,
	}
	if state.BackingPath != "" {
		backing, err := captureCheckpointFile(state.BackingPath, sources)
		if err != nil {
			return sessionStateCheckpoint{}, fmt.Errorf("capture session backing identity: %w", err)
		}
		checkpoint.Backing = &backing
	}
	if state.NativeSnapshot.Path != "" {
		native, err := captureCheckpointFile(state.NativeSnapshot.Path, sources)
		if err != nil {
			return sessionStateCheckpoint{}, fmt.Errorf("capture native snapshot identity: %w", err)
		}
		if native.Bytes != state.NativeSnapshot.Bytes || native.SHA256 != state.NativeSnapshot.SHA256 {
			return sessionStateCheckpoint{}, errors.New("native snapshot bytes differ from state")
		}
		checkpoint.NativeSnapshot = &native
	}
	if err := validateStateCheckpoint(checkpoint, sessionDirectory); err != nil {
		return sessionStateCheckpoint{}, err
	}
	return checkpoint, nil
}

func captureCheckpointFile(path string, sources map[string]string) (sessionStateFileIdentity, error) {
	recordedPath := filepath.Clean(path)
	actualPath := recordedPath
	if source, ok := sources[recordedPath]; ok {
		actualPath = filepath.Clean(source)
	}
	identity, err := captureRegularFileIdentity(actualPath)
	if err != nil {
		return sessionStateFileIdentity{}, err
	}
	identity.Path = recordedPath
	return identity, nil
}

func captureManifestIdentity(path string, sessionID string, baseBytes int64, baseSHA256 string) (sessionStateFileIdentity, fold.Manifest, error) {
	path = filepath.Clean(path)
	manifestRoot := filepath.Dir(path)
	for filepath.Base(manifestRoot) != "manifests" {
		parent := filepath.Dir(manifestRoot)
		if parent == manifestRoot {
			return sessionStateFileIdentity{}, fold.Manifest{}, errors.New("session manifest path is not canonical")
		}
		manifestRoot = parent
	}
	storeRoot := filepath.Dir(manifestRoot)
	if !filepath.IsAbs(storeRoot) {
		return sessionStateFileIdentity{}, fold.Manifest{}, errors.New("session manifest path is not canonical")
	}
	root, err := os.OpenRoot(storeRoot)
	if err != nil {
		return sessionStateFileIdentity{}, fold.Manifest{}, fmt.Errorf("open session manifest store root: %w", err)
	}
	defer root.Close()
	relative, err := filepath.Rel(storeRoot, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return sessionStateFileIdentity{}, fold.Manifest{}, errors.New("session manifest path is outside its store")
	}
	before, err := root.Lstat(relative)
	if err != nil {
		return sessionStateFileIdentity{}, fold.Manifest{}, err
	}
	if !before.Mode().IsRegular() {
		return sessionStateFileIdentity{}, fold.Manifest{}, errors.New("session manifest is not a regular file")
	}
	file, err := root.Open(relative)
	if err != nil {
		return sessionStateFileIdentity{}, fold.Manifest{}, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return sessionStateFileIdentity{}, fold.Manifest{}, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		return sessionStateFileIdentity{}, fold.Manifest{}, errors.New("session manifest changed while it was opened")
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return sessionStateFileIdentity{}, fold.Manifest{}, readErr
	}
	if closeErr != nil {
		return sessionStateFileIdentity{}, fold.Manifest{}, closeErr
	}
	after, err := root.Lstat(relative)
	if err != nil {
		return sessionStateFileIdentity{}, fold.Manifest{}, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(opened, after) || after.Size() != int64(len(data)) {
		return sessionStateFileIdentity{}, fold.Manifest{}, errors.New("session manifest changed while its identity was captured")
	}
	manifest, err := fold.DecodeManifest(data)
	if err != nil {
		return sessionStateFileIdentity{}, fold.Manifest{}, err
	}
	if manifest.Session.ID != sessionID || manifest.Source.Bytes != baseBytes || manifest.Source.SHA256 != baseSHA256 {
		return sessionStateFileIdentity{}, fold.Manifest{}, errors.New("session manifest does not match state source identity")
	}
	digest := sha256.Sum256(data)
	return sessionStateFileIdentity{Path: path, Bytes: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}, manifest, nil
}

func captureRegularFileIdentity(path string) (sessionStateFileIdentity, error) {
	path = filepath.Clean(path)
	before, err := os.Lstat(path)
	if err != nil {
		return sessionStateFileIdentity{}, err
	}
	if !before.Mode().IsRegular() {
		return sessionStateFileIdentity{}, errors.New("checkpoint artifact is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return sessionStateFileIdentity{}, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return sessionStateFileIdentity{}, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		return sessionStateFileIdentity{}, errors.New("checkpoint artifact changed while it was opened")
	}
	hasher := sha256.New()
	bytesRead, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil {
		return sessionStateFileIdentity{}, copyErr
	}
	if closeErr != nil {
		return sessionStateFileIdentity{}, closeErr
	}
	if stateCheckpointBytesHashed != nil {
		stateCheckpointBytesHashed(bytesRead)
	}
	after, err := os.Lstat(path)
	if err != nil {
		return sessionStateFileIdentity{}, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(opened, after) || after.Size() != bytesRead {
		return sessionStateFileIdentity{}, errors.New("checkpoint artifact changed while it was hashed")
	}
	return sessionStateFileIdentity{Path: path, Bytes: bytesRead, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

func writeImmutableStateCheckpoint(metadataDirectory string, name string, data []byte) error {
	directory := filepath.Join(metadataDirectory, stateGenerationsDirectoryName)
	if err := ensureStateCheckpointDirectory(metadataDirectory, directory); err != nil {
		return err
	}
	path := filepath.Join(directory, name)
	if existing, err := os.ReadFile(path); err == nil {
		if !bytes.Equal(existing, data) {
			return errors.New("immutable session state checkpoint already exists with different contents")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read existing session state checkpoint: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".state-checkpoint-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary session state checkpoint: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporaryPath != "" {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write session state checkpoint: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync session state checkpoint: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close session state checkpoint: %w", err)
	}
	if err := os.Link(temporaryPath, path); errors.Is(err, os.ErrExist) {
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read concurrently published session state checkpoint: %w", readErr)
		}
		if !bytes.Equal(existing, data) {
			return errors.New("immutable session state checkpoint was concurrently published with different contents")
		}
	} else if err != nil {
		return fmt.Errorf("publish immutable session state checkpoint: %w", err)
	}
	if err := syncStateDirectory(directory); err != nil {
		return fmt.Errorf("sync session state checkpoint directory: %w", err)
	}
	if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove published session state checkpoint temporary: %w", err)
	}
	temporaryPath = ""
	return syncStateDirectory(directory)
}

func validateStateCheckpointTemporaries(metadataDirectory string) error {
	directory := filepath.Join(filepath.Clean(metadataDirectory), stateGenerationsDirectoryName)
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("session state checkpoint path is not a directory")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".state-checkpoint-") || !strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		info, err := os.Lstat(filepath.Join(directory, entry.Name()))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > maximumStateCheckpointBytes {
			return errors.New("session state checkpoint temporary is not a bounded regular file")
		}
	}
	return nil
}

func cleanupStateCheckpointTemporaries(metadataDirectory string) error {
	if err := validateStateCheckpointTemporaries(metadataDirectory); err != nil {
		return err
	}
	directory := filepath.Join(filepath.Clean(metadataDirectory), stateGenerationsDirectoryName)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	removed := false
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".state-checkpoint-") || !strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
			return fmt.Errorf("remove superseded session state checkpoint temporary: %w", err)
		}
		removed = true
	}
	if removed {
		return syncStateDirectory(directory)
	}
	return nil
}

type interruptedStateCheckpointCandidate struct {
	checkpoint     sessionStateCheckpoint
	data           []byte
	digest         string
	temporaryPaths []string
	final          bool
}

func recoverPinnedStateCheckpointPublication(metadataDirectory string, sessionID string) error {
	pinnedSHA256, pinned, err := retirementCheckpointPin(metadataDirectory)
	if err != nil || !pinned {
		return err
	}
	chain, err := loadStateCheckpointChain(metadataDirectory, sessionID, false)
	if err != nil {
		return err
	}
	if _, exists := chain.lineage[pinnedSHA256]; !exists {
		return errors.New("retirement request checkpoint is not in the current state lineage")
	}
	persisted, stateData, err := readSessionState(filepath.Join(metadataDirectory, "state.json"))
	if err != nil || persisted != chain.checkpoint.State || digestStateBytes(stateData) != chain.catalog.StateSHA256 {
		return errors.New("primary session state changed before interrupted checkpoint recovery")
	}
	directory := filepath.Join(filepath.Clean(metadataDirectory), stateGenerationsDirectoryName)
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("session state checkpoint path is not a directory")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	lineageNames := make(map[string]struct{}, len(chain.lineage))
	for digest, checkpoint := range chain.lineage {
		lineageNames[stateCheckpointFilename(checkpoint.State.Generation, checkpoint.Sequence, digest)] = struct{}{}
	}
	candidates := make(map[string]*interruptedStateCheckpointCandidate)
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		if strings.HasPrefix(entry.Name(), ".state-checkpoint-") && strings.HasSuffix(entry.Name(), ".tmp") {
			data, err := readBoundedStateCheckpointFile(path)
			if err != nil {
				return err
			}
			checkpoint, decodeErr := decodeStateCheckpoint(data)
			if decodeErr != nil {
				continue
			}
			digest := digestStateBytes(data)
			if _, exists := chain.lineage[digest]; exists {
				continue
			}
			if !isDirectStateCheckpointChild(chain, checkpoint) {
				continue
			}
			candidate := candidates[digest]
			if candidate == nil {
				candidate = &interruptedStateCheckpointCandidate{checkpoint: checkpoint, data: data, digest: digest}
				candidates[digest] = candidate
			}
			candidate.temporaryPaths = append(candidate.temporaryPaths, path)
			continue
		}
		if _, exists := lineageNames[entry.Name()]; exists {
			continue
		}
		data, err := readBoundedStateCheckpointFile(path)
		if err != nil {
			return err
		}
		checkpoint, err := decodeStateCheckpoint(data)
		if err != nil {
			return fmt.Errorf("unrecognized session state checkpoint evidence %s: %w", entry.Name(), err)
		}
		digest := digestStateBytes(data)
		if entry.Name() != stateCheckpointFilename(checkpoint.State.Generation, checkpoint.Sequence, digest) || !isDirectStateCheckpointChild(chain, checkpoint) {
			return fmt.Errorf("unresolved session state checkpoint branch prevents recovery: %s", entry.Name())
		}
		candidate := candidates[digest]
		if candidate == nil {
			candidate = &interruptedStateCheckpointCandidate{checkpoint: checkpoint, data: data, digest: digest}
			candidates[digest] = candidate
		}
		candidate.final = true
	}

	currentMatches := verifyStateCheckpointArtifactsForChain(metadataDirectory, chain) == nil
	if len(candidates) > 1 {
		return errors.New("multiple interrupted direct-child state checkpoints prevent recovery")
	}
	for _, candidate := range candidates {
		allowed, allowedErr := verifiedSupersededStateCheckpointData(metadataDirectory, chain.lineage, candidate.checkpoint)
		if allowedErr != nil {
			return allowedErr
		}
		if err := verifyStateCheckpointArtifactsAllowed(metadataDirectory, candidate.checkpoint, allowed); err == nil {
			return adoptInterruptedStateCheckpoint(metadataDirectory, chain, candidate)
		}
		if candidate.final {
			return errors.New("linked direct-child state checkpoint does not match current artifacts")
		}
	}
	if currentMatches {
		return cleanupStateCheckpointTemporaries(metadataDirectory)
	}
	return errors.New("current session artifacts have no complete direct-child checkpoint proof")
}

func isDirectStateCheckpointChild(chain stateCheckpointChain, checkpoint sessionStateCheckpoint) bool {
	if checkpoint.SessionID != chain.catalog.SessionID || checkpoint.Sequence != chain.catalog.Sequence+1 || checkpoint.PreviousCheckpointSHA256 != chain.catalog.CheckpointSHA256 {
		return false
	}
	if checkpoint.State == chain.checkpoint.State && checkpoint.StateSHA256 == chain.catalog.StateSHA256 && checkpoint.State.Generation == chain.catalog.Generation {
		return true
	}
	return checkpoint.State.Generation == chain.catalog.Generation+1
}

func adoptInterruptedStateCheckpoint(metadataDirectory string, chain stateCheckpointChain, candidate *interruptedStateCheckpointCandidate) error {
	directory := filepath.Join(filepath.Clean(metadataDirectory), stateGenerationsDirectoryName)
	name := stateCheckpointFilename(candidate.checkpoint.State.Generation, candidate.checkpoint.Sequence, candidate.digest)
	path := filepath.Join(directory, name)
	if !candidate.final {
		if len(candidate.temporaryPaths) == 0 {
			return errors.New("interrupted state checkpoint has no durable publication source")
		}
		if err := syncAndVerifyStateCheckpointCandidate(candidate.temporaryPaths[0], candidate.data); err != nil {
			return fmt.Errorf("sync interrupted state checkpoint temporary before adoption: %w", err)
		}
		if err := os.Link(candidate.temporaryPaths[0], path); errors.Is(err, os.ErrExist) {
			existing, readErr := readBoundedStateCheckpointFile(path)
			if readErr != nil || !bytes.Equal(existing, candidate.data) {
				return errors.New("interrupted state checkpoint final conflicts during adoption")
			}
		} else if err != nil {
			return fmt.Errorf("publish interrupted state checkpoint: %w", err)
		}
		if err := syncStateDirectory(directory); err != nil {
			return err
		}
	} else if err := syncAndVerifyStateCheckpointCandidate(path, candidate.data); err != nil {
		return fmt.Errorf("sync interrupted state checkpoint final before adoption: %w", err)
	}
	data, err := readBoundedStateCheckpointFile(path)
	if err != nil || !bytes.Equal(data, candidate.data) {
		return errors.New("interrupted state checkpoint final changed before catalog adoption")
	}
	if err := writeStateCatalog(metadataDirectory, sessionStateCatalog{
		Version: sessionStateCatalogVersion, SessionID: chain.catalog.SessionID,
		Generation: candidate.checkpoint.State.Generation, Sequence: candidate.checkpoint.Sequence,
		StateSHA256: candidate.checkpoint.StateSHA256, Checkpoint: name, CheckpointSHA256: candidate.digest,
	}); err != nil {
		return err
	}
	if candidate.checkpoint.State != chain.checkpoint.State {
		if err := writeSessionState(filepath.Join(metadataDirectory, "state.json"), candidate.checkpoint.State); err != nil {
			return fmt.Errorf("publish adopted session state: %w", err)
		}
	}
	recovered, err := loadStateCheckpointChain(metadataDirectory, chain.catalog.SessionID, true)
	if err != nil {
		return fmt.Errorf("validate adopted state checkpoint lineage: %w", err)
	}
	if recovered.catalog.CheckpointSHA256 != candidate.digest {
		return errors.New("adopted state checkpoint is not the catalog head")
	}
	return cleanupStateCheckpointTemporaries(metadataDirectory)
}

func syncAndVerifyStateCheckpointCandidate(path string, expected []byte) error {
	path = filepath.Clean(path)
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Size() > maximumStateCheckpointBytes {
		return errors.New("session state checkpoint candidate is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		if err == nil {
			err = errors.New("session state checkpoint candidate changed while it was opened")
		}
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return err
	}
	if stateCheckpointCandidateSynced != nil {
		stateCheckpointCandidateSynced(path)
	}
	data, err := readBoundedStateCheckpointFile(path)
	if err != nil || !bytes.Equal(data, expected) {
		if err == nil {
			err = errors.New("session state checkpoint candidate changed after sync")
		}
		return err
	}
	return nil
}

func readBoundedStateCheckpointFile(path string) ([]byte, error) {
	path = filepath.Clean(path)
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > maximumStateCheckpointBytes {
		return nil, errors.New("session state checkpoint evidence is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, errors.New("session state checkpoint evidence changed while it was opened")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maximumStateCheckpointBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if len(data) > maximumStateCheckpointBytes {
		return nil, errors.New("session state checkpoint evidence exceeds its size limit")
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) || after.Size() != int64(len(data)) || !after.ModTime().Equal(opened.ModTime()) {
		if err == nil {
			err = errors.New("session state checkpoint evidence changed while it was read")
		}
		return nil, err
	}
	return data, nil
}

func ensureStateCheckpointDirectory(parent string, directory string) error {
	if err := os.Mkdir(directory, 0o700); err == nil {
		return syncStateDirectory(parent)
	} else if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create session state checkpoint directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("session state checkpoint path is not a directory")
	}
	return nil
}

func writeStateCatalog(metadataDirectory string, catalog sessionStateCatalog) error {
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session state catalog: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(metadataDirectory, ".state-catalog-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary session state catalog: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write session state catalog: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync session state catalog: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close session state catalog: %w", err)
	}
	if err := replaceStateFile(temporaryPath, filepath.Join(metadataDirectory, stateCatalogFilename)); err != nil {
		return fmt.Errorf("commit session state catalog: %w", err)
	}
	return syncStateDirectory(metadataDirectory)
}

func loadPublishedSessionState(path string) (SessionState, error) {
	state, data, stateErr := readSessionState(path)
	metadataDirectory := filepath.Clean(filepath.Dir(path))
	catalogPresent, metadataErr := regularStateCatalogPresent(metadataDirectory)
	if metadataErr != nil {
		return SessionState{}, metadataErr
	}
	if !catalogPresent {
		if errors.Is(stateErr, errLegacySessionState) {
			return migrateLegacySessionState(path)
		}
		return state, stateErr
	}
	chain, chainErr := loadCatalogStateCheckpoint(metadataDirectory, filepath.Base(metadataDirectory))
	if chainErr != nil {
		return SessionState{}, fmt.Errorf("load session state checkpoint metadata: %w", chainErr)
	}
	if stateErr == nil && digestStateBytes(data) == chain.catalog.StateSHA256 && state == chain.checkpoint.State {
		return state, nil
	}
	return recoverSessionStateFromCheckpoint(path)
}

func inspectPublishedSessionState(path string) (SessionState, error) {
	state, data, stateErr := readSessionState(path)
	metadataDirectory := filepath.Clean(filepath.Dir(path))
	catalogPresent, metadataErr := regularStateCatalogPresent(metadataDirectory)
	if metadataErr != nil {
		return SessionState{}, metadataErr
	}
	if !catalogPresent {
		return state, stateErr
	}
	chain, chainErr := loadCatalogStateCheckpoint(metadataDirectory, filepath.Base(metadataDirectory))
	if chainErr != nil {
		return SessionState{}, fmt.Errorf("load session state checkpoint metadata: %w", chainErr)
	}
	if stateErr == nil && digestStateBytes(data) == chain.catalog.StateSHA256 && state == chain.checkpoint.State {
		return state, nil
	}
	if stateErr != nil {
		return SessionState{}, fmt.Errorf("primary session state requires checkpoint recovery: %w", stateErr)
	}
	return SessionState{}, errors.New("primary session state differs from its published checkpoint")
}

func recoverSessionStateFromCheckpoint(path string) (SessionState, error) {
	path = filepath.Clean(path)
	directory := filepath.Dir(path)
	sessionID := filepath.Base(directory)
	if filepath.Base(path) != "state.json" || !safeSessionID(sessionID) || filepath.Base(filepath.Dir(directory)) != "sessions" {
		return SessionState{}, errors.New("session state recovery path is not canonical")
	}
	lease, err := acquireWriterLease(filepath.Join(directory, "writer.lease"))
	if err != nil {
		return SessionState{}, fmt.Errorf("acquire session recovery lease: %w", err)
	}
	defer func() {
		_ = unlockWriterFile(lease)
		_ = lease.Close()
	}()
	if info, inspectErr := os.Lstat(path); inspectErr == nil {
		if !info.Mode().IsRegular() {
			return SessionState{}, errors.New("refusing to replace a non-regular primary session state")
		}
	} else if !errors.Is(inspectErr, os.ErrNotExist) {
		return SessionState{}, fmt.Errorf("inspect primary session state before recovery: %w", inspectErr)
	}

	chain, err := loadStateCheckpointChain(directory, sessionID, true)
	if err != nil {
		return SessionState{}, fmt.Errorf("load recoverable session state checkpoint: %w", err)
	}
	if err := verifyStateCheckpointArtifactsForChain(directory, chain); err != nil {
		return SessionState{}, fmt.Errorf("verify recoverable session state checkpoint: %w", err)
	}
	if err := verifyStateCheckpointArtifactsForChain(directory, chain); err != nil {
		return SessionState{}, fmt.Errorf("reverify recoverable session state checkpoint: %w", err)
	}
	if err := writeSessionState(path, chain.checkpoint.State); err != nil {
		return SessionState{}, fmt.Errorf("restore session state from checkpoint: %w", err)
	}
	recovered, data, err := readSessionState(path)
	if err != nil {
		return SessionState{}, err
	}
	if recovered != chain.checkpoint.State || digestStateBytes(data) != chain.catalog.StateSHA256 {
		return SessionState{}, errors.New("restored session state does not match its checkpoint")
	}
	return recovered, nil
}

func loadStateCheckpointChain(metadataDirectory string, sessionID string, strict bool) (stateCheckpointChain, error) {
	catalog, err := loadStateCatalog(metadataDirectory)
	if err != nil {
		return stateCheckpointChain{}, err
	}
	if catalog.SessionID != sessionID {
		return stateCheckpointChain{}, errors.New("session state catalog belongs to another session")
	}
	directory := filepath.Join(metadataDirectory, stateGenerationsDirectoryName)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return stateCheckpointChain{}, err
	}
	checkpoints := make(map[string]sessionStateCheckpoint, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".state-checkpoint-") && strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			if strict || entry.Name() == catalog.Checkpoint {
				return stateCheckpointChain{}, errors.New("session state checkpoint directory contains a non-file entry")
			}
			continue
		}
		path := filepath.Join(directory, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return stateCheckpointChain{}, err
		}
		checkpoint, err := decodeStateCheckpoint(data)
		if err != nil {
			if strict || entry.Name() == catalog.Checkpoint {
				return stateCheckpointChain{}, fmt.Errorf("decode session state checkpoint %s: %w", entry.Name(), err)
			}
			continue
		}
		digest := digestStateBytes(data)
		if entry.Name() != stateCheckpointFilename(checkpoint.State.Generation, checkpoint.Sequence, digest) {
			if strict || entry.Name() == catalog.Checkpoint {
				return stateCheckpointChain{}, errors.New("session state checkpoint filename does not match its contents")
			}
			continue
		}
		if checkpoint.SessionID != sessionID {
			if strict || entry.Name() == catalog.Checkpoint {
				return stateCheckpointChain{}, errors.New("session state checkpoint belongs to another session")
			}
			continue
		}
		if _, exists := checkpoints[digest]; exists {
			return stateCheckpointChain{}, errors.New("duplicate session state checkpoint identity")
		}
		checkpoints[digest] = checkpoint
	}
	target, ok := checkpoints[catalog.CheckpointSHA256]
	if !ok || catalog.Checkpoint != stateCheckpointFilename(target.State.Generation, target.Sequence, catalog.CheckpointSHA256) {
		return stateCheckpointChain{}, errors.New("session state catalog target is missing")
	}
	if target.State.Generation != catalog.Generation || target.Sequence != catalog.Sequence || target.StateSHA256 != catalog.StateSHA256 {
		return stateCheckpointChain{}, errors.New("session state catalog target metadata conflicts")
	}
	visited := make(map[string]struct{}, len(checkpoints))
	digest := catalog.CheckpointSHA256
	for {
		checkpoint, ok := checkpoints[digest]
		if !ok {
			return stateCheckpointChain{}, errors.New("session state checkpoint lineage is incomplete")
		}
		if _, duplicate := visited[digest]; duplicate {
			return stateCheckpointChain{}, errors.New("session state checkpoint lineage contains a cycle")
		}
		visited[digest] = struct{}{}
		previous := checkpoint.PreviousCheckpointSHA256
		if previous == "" {
			break
		}
		parent, ok := checkpoints[previous]
		if !ok || parent.Sequence >= checkpoint.Sequence || parent.State.Generation > checkpoint.State.Generation {
			return stateCheckpointChain{}, errors.New("session state checkpoint lineage is invalid")
		}
		digest = previous
	}
	if strict && len(visited) != len(checkpoints) {
		return stateCheckpointChain{}, errors.New("session state checkpoint history is ambiguous")
	}
	lineage := make(map[string]sessionStateCheckpoint, len(visited))
	for digest := range visited {
		lineage[digest] = checkpoints[digest]
	}
	return stateCheckpointChain{catalog: catalog, checkpoint: target, lineage: lineage}, nil
}

func loadCatalogStateCheckpoint(metadataDirectory string, sessionID string) (stateCheckpointChain, error) {
	catalog, err := loadStateCatalog(metadataDirectory)
	if err != nil {
		return stateCheckpointChain{}, err
	}
	if catalog.SessionID != sessionID {
		return stateCheckpointChain{}, errors.New("session state catalog belongs to another session")
	}
	path := filepath.Join(metadataDirectory, stateGenerationsDirectoryName, catalog.Checkpoint)
	info, err := os.Lstat(path)
	if err != nil {
		return stateCheckpointChain{}, err
	}
	if !info.Mode().IsRegular() {
		return stateCheckpointChain{}, errors.New("catalog session state checkpoint is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return stateCheckpointChain{}, err
	}
	checkpoint, err := decodeStateCheckpoint(data)
	if err != nil {
		return stateCheckpointChain{}, err
	}
	digest := digestStateBytes(data)
	if digest != catalog.CheckpointSHA256 || catalog.Checkpoint != stateCheckpointFilename(checkpoint.State.Generation, checkpoint.Sequence, digest) || checkpoint.SessionID != sessionID || checkpoint.State.Generation != catalog.Generation || checkpoint.Sequence != catalog.Sequence || checkpoint.StateSHA256 != catalog.StateSHA256 {
		return stateCheckpointChain{}, errors.New("session state catalog target metadata conflicts")
	}
	return stateCheckpointChain{catalog: catalog, checkpoint: checkpoint}, nil
}

func reconcileStateCheckpointHistory(metadataDirectory string, sessionID string) error {
	chain, err := loadStateCheckpointChain(metadataDirectory, sessionID, false)
	if err != nil {
		return err
	}
	persisted, stateData, err := readSessionState(filepath.Join(metadataDirectory, "state.json"))
	if err != nil || persisted != chain.checkpoint.State || digestStateBytes(stateData) != chain.catalog.StateSHA256 {
		return errors.New("primary session state is not authoritative enough to reconcile checkpoint history")
	}

	directory := filepath.Join(metadataDirectory, stateGenerationsDirectoryName)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	type checkpointEntry struct {
		name       string
		checkpoint sessionStateCheckpoint
	}
	checkpoints := make(map[string]checkpointEntry)
	quarantine := make(map[string]struct{})
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".state-checkpoint-") && strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("cannot reconcile non-file session state checkpoint metadata")
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return err
		}
		checkpoint, decodeErr := decodeStateCheckpoint(data)
		digest := digestStateBytes(data)
		if decodeErr != nil || entry.Name() != stateCheckpointFilename(checkpoint.State.Generation, checkpoint.Sequence, digest) || checkpoint.SessionID != sessionID {
			quarantine[entry.Name()] = struct{}{}
			continue
		}
		checkpoints[digest] = checkpointEntry{name: entry.Name(), checkpoint: checkpoint}
	}

	lineage := make(map[string]struct{})
	digest := chain.catalog.CheckpointSHA256
	for {
		entry, ok := checkpoints[digest]
		if !ok {
			return errors.New("catalog checkpoint lineage disappeared during reconciliation")
		}
		lineage[entry.name] = struct{}{}
		if entry.checkpoint.PreviousCheckpointSHA256 == "" {
			break
		}
		digest = entry.checkpoint.PreviousCheckpointSHA256
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".state-checkpoint-") && strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		if _, ok := lineage[entry.Name()]; !ok {
			quarantine[entry.Name()] = struct{}{}
		}
	}
	if len(quarantine) == 0 {
		return nil
	}
	orphanDirectory := filepath.Join(metadataDirectory, "state-orphans")
	if err := os.Mkdir(orphanDirectory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create session state orphan directory: %w", err)
	}
	info, err := os.Lstat(orphanDirectory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("session state orphan path is not a directory")
	}
	for name := range quarantine {
		source := filepath.Join(directory, name)
		target := filepath.Join(orphanDirectory, name)
		if _, err := os.Lstat(target); err == nil {
			return fmt.Errorf("session state orphan already exists: %s", name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(source, target); err != nil {
			return fmt.Errorf("quarantine session state checkpoint %s: %w", name, err)
		}
	}
	if err := syncStateDirectory(directory); err != nil {
		return err
	}
	if err := syncStateDirectory(orphanDirectory); err != nil {
		return err
	}
	return syncStateDirectory(metadataDirectory)
}

func loadStateCatalog(metadataDirectory string) (sessionStateCatalog, error) {
	path := filepath.Join(metadataDirectory, stateCatalogFilename)
	info, err := os.Lstat(path)
	if err != nil {
		return sessionStateCatalog{}, err
	}
	if !info.Mode().IsRegular() {
		return sessionStateCatalog{}, errors.New("session state catalog is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return sessionStateCatalog{}, err
	}
	var catalog sessionStateCatalog
	if err := decodeStrictJSON(data, &catalog); err != nil {
		return sessionStateCatalog{}, fmt.Errorf("decode session state catalog: %w", err)
	}
	if catalog.Version != sessionStateCatalogVersion || !safeSessionID(catalog.SessionID) || catalog.Generation == 0 || catalog.Sequence == 0 || !validStateSHA256(catalog.StateSHA256) || !validStateSHA256(catalog.CheckpointSHA256) || filepath.Base(catalog.Checkpoint) != catalog.Checkpoint {
		return sessionStateCatalog{}, errors.New("invalid session state catalog")
	}
	return catalog, nil
}

func decodeStateCheckpoint(data []byte) (sessionStateCheckpoint, error) {
	var checkpoint sessionStateCheckpoint
	if err := decodeStrictJSON(data, &checkpoint); err != nil {
		return sessionStateCheckpoint{}, err
	}
	if err := validateStateCheckpoint(checkpoint, filepath.Dir(checkpoint.State.DeltaPath)); err != nil {
		return sessionStateCheckpoint{}, err
	}
	return checkpoint, nil
}

func validateStateCheckpoint(checkpoint sessionStateCheckpoint, sessionDirectory string) error {
	if checkpoint.Version != sessionStateCheckpointVersion || checkpoint.SessionID != checkpoint.State.SessionID || checkpoint.Sequence == 0 || !validStateSHA256(checkpoint.StateSHA256) {
		return errors.New("invalid session state checkpoint")
	}
	if checkpoint.PreviousCheckpointSHA256 != "" && !validStateSHA256(checkpoint.PreviousCheckpointSHA256) {
		return errors.New("invalid previous session state checkpoint identity")
	}
	if err := validateSessionStateForDirectory(checkpoint.State, sessionDirectory); err != nil {
		return err
	}
	stateData, err := encodeSessionState(checkpoint.State)
	if err != nil || digestStateBytes(stateData) != checkpoint.StateSHA256 {
		return errors.New("session state checkpoint state hash differs from its contents")
	}
	if err := validateCheckpointFileIdentity(checkpoint.Manifest); err != nil || checkpoint.Manifest.Path != filepath.Clean(checkpoint.State.ManifestPath) || checkpoint.Manifest.SHA256 != checkpoint.State.ManifestSHA256 {
		return errors.New("invalid session manifest checkpoint identity")
	}
	if err := validateCheckpointFileIdentity(checkpoint.Delta); err != nil || checkpoint.Delta.Path != filepath.Clean(checkpoint.State.DeltaPath) {
		return errors.New("invalid session delta checkpoint identity")
	}
	if checkpoint.State.BackingPath == "" {
		if checkpoint.Backing != nil {
			return errors.New("unexpected session backing checkpoint identity")
		}
	} else if checkpoint.Backing == nil || validateCheckpointFileIdentity(*checkpoint.Backing) != nil || checkpoint.Backing.Path != filepath.Clean(checkpoint.State.BackingPath) {
		return errors.New("invalid session backing checkpoint identity")
	}
	if checkpoint.State.NativeSnapshot.Path == "" {
		if checkpoint.NativeSnapshot != nil {
			return errors.New("unexpected native snapshot checkpoint identity")
		}
	} else if checkpoint.NativeSnapshot == nil || validateCheckpointFileIdentity(*checkpoint.NativeSnapshot) != nil || checkpoint.NativeSnapshot.Path != filepath.Clean(checkpoint.State.NativeSnapshot.Path) || checkpoint.NativeSnapshot.Bytes != checkpoint.State.NativeSnapshot.Bytes || checkpoint.NativeSnapshot.SHA256 != checkpoint.State.NativeSnapshot.SHA256 {
		return errors.New("invalid native snapshot checkpoint identity")
	}
	return nil
}

func validateCheckpointFileIdentity(identity sessionStateFileIdentity) error {
	if identity.Path == "" || identity.Path != filepath.Clean(identity.Path) || identity.Bytes < 0 || !validStateSHA256(identity.SHA256) {
		return errors.New("invalid checkpoint file identity")
	}
	return nil
}

func verifyStateCheckpointArtifacts(sessionDirectory string, checkpoint sessionStateCheckpoint) error {
	return verifyStateCheckpointArtifactsAllowed(sessionDirectory, checkpoint, nil)
}

func verifyStateCheckpointArtifactsForChain(sessionDirectory string, chain stateCheckpointChain) error {
	allowed, err := verifiedSupersededStateCheckpointData(sessionDirectory, chain.lineage, chain.checkpoint)
	if err != nil {
		return err
	}
	return verifyStateCheckpointArtifactsAllowed(sessionDirectory, chain.checkpoint, allowed)
}

func verifyStateCheckpointArtifactsAllowed(sessionDirectory string, checkpoint sessionStateCheckpoint, allowed map[string]struct{}) error {
	if err := validateStateCheckpoint(checkpoint, sessionDirectory); err != nil {
		return err
	}
	manifest, _, err := captureManifestIdentity(checkpoint.Manifest.Path, checkpoint.State.SessionID, checkpoint.State.BaseBytes, checkpoint.State.BaseSHA256)
	if err != nil || manifest != checkpoint.Manifest {
		return errors.New("session manifest no longer matches its checkpoint")
	}
	for label, expected := range map[string]sessionStateFileIdentity{
		"delta": checkpoint.Delta,
	} {
		actual, err := captureRegularFileIdentity(expected.Path)
		if err != nil || actual != expected {
			return fmt.Errorf("session %s no longer matches its checkpoint", label)
		}
	}
	if checkpoint.Backing != nil {
		actual, err := captureRegularFileIdentity(checkpoint.Backing.Path)
		if err != nil || actual != *checkpoint.Backing {
			return errors.New("session backing no longer matches its checkpoint")
		}
	}
	if checkpoint.NativeSnapshot != nil {
		actual, err := captureRegularFileIdentity(checkpoint.NativeSnapshot.Path)
		if err != nil || actual != *checkpoint.NativeSnapshot {
			return errors.New("native snapshot no longer matches its checkpoint")
		}
	}
	return verifyNoUnknownNonemptySessionDataAllowed(sessionDirectory, checkpoint, allowed)
}

func verifiedSupersededStateCheckpointData(sessionDirectory string, lineage map[string]sessionStateCheckpoint, active sessionStateCheckpoint) (map[string]struct{}, error) {
	activePaths := map[string]struct{}{filepath.Clean(active.Delta.Path): {}}
	if active.Backing != nil {
		activePaths[filepath.Clean(active.Backing.Path)] = struct{}{}
	}
	expected := make(map[string][]sessionStateFileIdentity)
	for _, checkpoint := range lineage {
		for _, identity := range []sessionStateFileIdentity{checkpoint.Delta} {
			path := filepath.Clean(identity.Path)
			if _, active := activePaths[path]; !active {
				expected[path] = append(expected[path], identity)
			}
		}
		if checkpoint.Backing != nil {
			path := filepath.Clean(checkpoint.Backing.Path)
			if _, active := activePaths[path]; !active {
				expected[path] = append(expected[path], *checkpoint.Backing)
			}
		}
	}
	allowed := make(map[string]struct{}, len(expected))
	for path, identities := range expected {
		if !pathWithin(sessionDirectory, path) || filepath.Clean(path) == filepath.Clean(sessionDirectory) {
			return nil, errors.New("superseded checkpoint contains an unsafe session data path")
		}
		actual, err := captureRegularFileIdentity(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("verify superseded checkpoint artifact: %w", err)
		}
		matched := false
		for _, identity := range identities {
			if actual == identity {
				matched = true
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("superseded checkpoint artifact no longer matches its lineage: %s", filepath.Base(path))
		}
		allowed[path] = struct{}{}
	}
	return allowed, nil
}

func verifyNoUnknownNonemptySessionData(sessionDirectory string, checkpoint sessionStateCheckpoint) error {
	return verifyNoUnknownNonemptySessionDataAllowed(sessionDirectory, checkpoint, nil)
}

func verifyNoUnknownNonemptySessionDataAllowed(sessionDirectory string, checkpoint sessionStateCheckpoint, allowed map[string]struct{}) error {
	known := map[string]struct{}{filepath.Clean(checkpoint.Delta.Path): {}}
	if checkpoint.Backing != nil {
		known[filepath.Clean(checkpoint.Backing.Path)] = struct{}{}
	}
	for path := range allowed {
		known[filepath.Clean(path)] = struct{}{}
	}
	entries, err := os.ReadDir(sessionDirectory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !managedSessionDataName(name) {
			continue
		}
		path := filepath.Join(sessionDirectory, name)
		if _, ok := known[path]; ok {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() != 0 {
			return fmt.Errorf("unrecognized session data prevents state recovery: %s", name)
		}
	}
	return nil
}

func managedSessionDataName(name string) bool {
	return name == "delta.jsonl" ||
		(strings.HasPrefix(name, "delta-") && strings.HasSuffix(name, ".jsonl")) ||
		(strings.HasPrefix(name, "backing-") && strings.HasSuffix(name, ".jsonl")) ||
		(strings.HasPrefix(name, ".compact-") && strings.HasSuffix(name, ".jsonl"))
}

func validateSessionStateForDirectory(state SessionState, sessionDirectory string) error {
	if state.Version != sessionStateVersion || !safeSessionID(state.SessionID) || state.Generation == 0 || state.BaseBytes < 0 || !validStateSHA256(state.BaseSHA256) || !validStateSHA256(state.ManifestSHA256) || state.DeltaPath == "" {
		return errors.New("invalid virtual session state")
	}
	sessionDirectory = filepath.Clean(sessionDirectory)
	if filepath.Base(sessionDirectory) != state.SessionID || filepath.Base(filepath.Dir(sessionDirectory)) != "sessions" || filepath.Base(filepath.Dir(filepath.Dir(sessionDirectory))) != "fs" {
		return errors.New("session state directory does not match its session ID")
	}
	storeRoot := filepath.Dir(filepath.Dir(filepath.Dir(sessionDirectory)))
	manifestRoot := filepath.Join(storeRoot, "manifests")
	if !pathWithin(manifestRoot, filepath.Clean(state.ManifestPath)) || filepath.Clean(state.ManifestPath) == manifestRoot {
		return errors.New("session state contains an unsafe manifest path")
	}
	if !pathWithin(sessionDirectory, filepath.Clean(state.DeltaPath)) || filepath.Clean(state.DeltaPath) == sessionDirectory || (state.BackingPath != "" && (!pathWithin(sessionDirectory, filepath.Clean(state.BackingPath)) || filepath.Clean(state.BackingPath) == sessionDirectory)) {
		return errors.New("session state contains an unsafe data path")
	}
	native := state.NativeSnapshot
	if native.Path == "" {
		if native.Bytes != 0 || native.SHA256 != "" {
			return errors.New("session state contains partial native snapshot metadata")
		}
	} else if native.Bytes < 0 || !validStateSHA256(native.SHA256) {
		return errors.New("session state contains invalid native snapshot metadata")
	}
	return nil
}

func regularStateCatalogPresent(metadataDirectory string) (bool, error) {
	info, err := os.Lstat(filepath.Join(metadataDirectory, stateCatalogFilename))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("session state catalog is not a regular file")
	}
	return true, nil
}

func validateInitialCheckpointPublication(metadataDirectory string, expectedName string, expectedData []byte, allowOrphanAdoption bool) error {
	directory := filepath.Join(metadataDirectory, stateGenerationsDirectoryName)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".state-checkpoint-") && strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		if entry.Name() != expectedName || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("session state checkpoint history is ambiguous before initial catalog publication")
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return err
		}
		if !bytes.Equal(data, expectedData) {
			return errors.New("orphaned initial session state checkpoint conflicts with current evidence")
		}
		if !allowOrphanAdoption {
			return errors.New("orphaned initial session state checkpoint requires a protected migration lease")
		}
	}
	return nil
}

func stateCheckpointFilename(generation uint64, sequence uint64, digest string) string {
	return fmt.Sprintf("%020d-%020d-%s.json", generation, sequence, digest)
}

func encodeStateCheckpoint(checkpoint sessionStateCheckpoint) ([]byte, error) {
	data, err := json.MarshalIndent(checkpoint, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode session state checkpoint: %w", err)
	}
	return append(data, '\n'), nil
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func digestStateBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func validStateSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

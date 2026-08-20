package vfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const initialSessionStagingPrefix = ".codexfold-initial-"

func publishInitialSession(ctx context.Context, options SessionOptions, view *View, directory string, reserveWriter bool) (SessionState, *os.File, error) {
	parent := filepath.Dir(directory)
	initializationLease, err := acquireInitialSessionLease(ctx, filepath.Join(parent, initialSessionLockName(options.Manifest.Session.ID)))
	if err != nil {
		return SessionState{}, nil, fmt.Errorf("acquire session initialization lease: %w", err)
	}
	defer func() {
		_ = unlockWriterFile(initializationLease)
		_ = initializationLease.Close()
	}()

	statePath := filepath.Join(directory, "state.json")
	state, err := LoadSessionState(statePath)
	if err == nil {
		return state, nil, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return SessionState{}, nil, err
	}
	if err := verifyNativeFile(options.NativeSnapshot); err != nil {
		return SessionState{}, nil, err
	}
	manifestIdentity, _, err := captureManifestIdentity(options.ManifestPath, options.Manifest.Session.ID, view.Size(), options.Manifest.Source.SHA256)
	if err != nil {
		return SessionState{}, nil, fmt.Errorf("verify initial session manifest: %w", err)
	}
	state = SessionState{
		Version: sessionStateVersion, SessionID: options.Manifest.Session.ID, Generation: 1,
		ManifestPath: filepath.Clean(options.ManifestPath), ManifestSHA256: manifestIdentity.SHA256,
		BaseBytes: view.Size(), BaseSHA256: options.Manifest.Source.SHA256,
		DeltaPath: filepath.Join(directory, "delta.jsonl"), NativeSnapshot: options.NativeSnapshot,
	}
	if err := cleanupInitialSessionStaging(parent, options.Manifest.Session.ID, state); err != nil {
		return SessionState{}, nil, err
	}
	removed, err := removeAbandonedLegacyInitialSession(directory)
	if err != nil {
		return SessionState{}, nil, err
	}
	if !removed {
		if _, statErr := os.Lstat(directory); statErr == nil {
			return SessionState{}, nil, fmt.Errorf("managed session %s has no state and is not a recognized initial publication", options.Manifest.Session.ID)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return SessionState{}, nil, fmt.Errorf("inspect managed session directory: %w", statErr)
		}
	}

	staging, err := os.MkdirTemp(parent, initialSessionStagingNamePrefix(options.Manifest.Session.ID)+"*")
	if err != nil {
		return SessionState{}, nil, fmt.Errorf("create initial session staging directory: %w", err)
	}
	stagingPublished := false
	defer func() {
		if !stagingPublished {
			_ = os.RemoveAll(staging)
		}
	}()

	stagedDeltaPath := filepath.Join(staging, "delta.jsonl")
	delta, err := os.OpenFile(stagedDeltaPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return SessionState{}, nil, fmt.Errorf("create staged session delta: %w", err)
	}
	if err := delta.Sync(); err != nil {
		_ = delta.Close()
		return SessionState{}, nil, fmt.Errorf("sync staged session delta: %w", err)
	}
	if err := delta.Close(); err != nil {
		return SessionState{}, nil, fmt.Errorf("close staged session delta: %w", err)
	}

	var reservedLease *os.File
	if reserveWriter {
		reservedLease, err = acquireWriterLease(filepath.Join(staging, "writer.lease"))
		if err != nil {
			return SessionState{}, nil, err
		}
	}
	cleanupReservedLease := func() {
		if reservedLease == nil {
			return
		}
		_ = unlockWriterFile(reservedLease)
		_ = reservedLease.Close()
		reservedLease = nil
	}

	if err := publishInitialSessionState(filepath.Join(staging, "state.json"), state, directory, stagedDeltaPath); err != nil {
		cleanupReservedLease()
		return SessionState{}, nil, err
	}
	if err := syncStateDirectory(staging); err != nil {
		cleanupReservedLease()
		return SessionState{}, nil, fmt.Errorf("sync initial session staging directory: %w", err)
	}
	if _, err := os.Lstat(directory); err == nil {
		cleanupReservedLease()
		return SessionState{}, nil, errors.New("managed session directory appeared during initial publication")
	} else if !errors.Is(err, os.ErrNotExist) {
		cleanupReservedLease()
		return SessionState{}, nil, fmt.Errorf("inspect initial session publication target: %w", err)
	}
	if err := os.Rename(staging, directory); err != nil {
		cleanupReservedLease()
		return SessionState{}, nil, fmt.Errorf("publish initial session directory: %w", err)
	}
	stagingPublished = true
	if err := syncStateDirectory(parent); err != nil {
		cleanupReservedLease()
		return SessionState{}, nil, fmt.Errorf("sync virtual sessions directory: %w", err)
	}
	return state, reservedLease, nil
}

func cleanupInitialSessionStaging(parent string, sessionID string, expectedState SessionState) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return fmt.Errorf("read initial session staging directories: %w", err)
	}
	prefix := initialSessionStagingNamePrefix(sessionID)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		path := filepath.Join(parent, entry.Name())
		removable, err := removableInitialSessionDirectory(path, &expectedState)
		if err != nil {
			return err
		}
		if !removable {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove abandoned initial session staging directory %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func removeAbandonedLegacyInitialSession(directory string) (bool, error) {
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect legacy initial session directory: %w", err)
	}
	if !info.IsDir() {
		return false, nil
	}
	removable, err := removableInitialSessionDirectory(directory, nil)
	if err != nil {
		return false, err
	}
	if !removable {
		return false, nil
	}

	parent := filepath.Dir(directory)
	quarantine, err := os.MkdirTemp(parent, initialSessionStagingNamePrefix(filepath.Base(directory))+"legacy-*")
	if err != nil {
		return false, fmt.Errorf("reserve abandoned session cleanup path: %w", err)
	}
	if err := os.Remove(quarantine); err != nil {
		return false, fmt.Errorf("prepare abandoned session cleanup path: %w", err)
	}
	if err := os.Rename(directory, quarantine); err != nil {
		return false, fmt.Errorf("isolate abandoned initial session directory: %w", err)
	}
	if err := syncStateDirectory(parent); err != nil {
		return false, fmt.Errorf("sync abandoned session isolation: %w", err)
	}
	if err := os.RemoveAll(quarantine); err != nil {
		return false, fmt.Errorf("remove abandoned initial session directory: %w", err)
	}
	return true, nil
}

func removableInitialSessionDirectory(directory string, expectedState *SessionState) (bool, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return false, fmt.Errorf("read initial session directory: %w", err)
	}
	hasCatalog := false
	hasGenerations := false
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		entryInfo, err := entry.Info()
		if err != nil {
			return false, fmt.Errorf("inspect initial session artifact %s: %w", entry.Name(), err)
		}
		switch {
		case expectedState != nil && entry.Name() == stateGenerationsDirectoryName:
			if !entryInfo.IsDir() || entryInfo.Mode()&os.ModeSymlink != 0 {
				return false, nil
			}
			hasGenerations = true
			continue
		case expectedState != nil && entry.Name() == stateCatalogFilename:
			if !entryInfo.Mode().IsRegular() {
				return false, nil
			}
			hasCatalog = true
			continue
		case !entryInfo.Mode().IsRegular():
			return false, nil
		case entry.Name() == "delta.jsonl":
			if entryInfo.Size() != 0 {
				return false, nil
			}
		case entry.Name() == "writer.lease":
			available, err := abandonedWriterLease(path)
			if err != nil {
				return false, err
			}
			if !available {
				return false, nil
			}
		case expectedState != nil && entry.Name() == "state.json":
			state, err := loadSessionState(path)
			if err != nil || state != *expectedState {
				return false, nil
			}
		case strings.HasPrefix(entry.Name(), ".state-") && strings.HasSuffix(entry.Name(), ".tmp"):
		default:
			return false, nil
		}
	}
	if hasCatalog != hasGenerations {
		return false, nil
	}
	if hasCatalog {
		valid, err := validInitialStateCheckpoint(directory, *expectedState)
		if err != nil {
			return false, err
		}
		if !valid {
			return false, nil
		}
	}
	return true, nil
}

func validInitialStateCheckpoint(stagingDirectory string, expectedState SessionState) (bool, error) {
	chain, err := loadStateCheckpointChain(stagingDirectory, expectedState.SessionID, true)
	if err != nil {
		return false, nil
	}
	stateData, err := encodeSessionState(expectedState)
	if err != nil {
		return false, err
	}
	candidate, err := buildStateCheckpoint(
		expectedState,
		digestStateBytes(stateData),
		chain.checkpoint.PreviousCheckpointSHA256,
		chain.checkpoint.Sequence,
		filepath.Dir(expectedState.DeltaPath),
		map[string]string{filepath.Clean(expectedState.DeltaPath): filepath.Join(stagingDirectory, "delta.jsonl")},
	)
	if err != nil {
		return false, nil
	}
	return candidate.Version == chain.checkpoint.Version &&
		candidate.SessionID == chain.checkpoint.SessionID &&
		candidate.Sequence == chain.checkpoint.Sequence &&
		candidate.PreviousCheckpointSHA256 == chain.checkpoint.PreviousCheckpointSHA256 &&
		sameStateCheckpointArtifacts(candidate, chain.checkpoint), nil
}

func abandonedWriterLease(path string) (bool, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return false, fmt.Errorf("open legacy initial writer lease: %w", err)
	}
	locked, err := tryLockWriterFile(file)
	if err != nil {
		_ = file.Close()
		return false, fmt.Errorf("inspect legacy initial writer lease: %w", err)
	}
	if !locked {
		_ = file.Close()
		return false, nil
	}
	unlockErr := unlockWriterFile(file)
	closeErr := file.Close()
	if err := errors.Join(unlockErr, closeErr); err != nil {
		return false, fmt.Errorf("release legacy initial writer lease: %w", err)
	}
	return true, nil
}

func acquireInitialSessionLease(ctx context.Context, path string) (*os.File, error) {
	for {
		lease, err := acquireWriterLease(path)
		if err == nil {
			return lease, nil
		}
		if !errors.Is(err, ErrWriterBusy) {
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func initialSessionLockName(sessionID string) string {
	return initialSessionStagingPrefix + sessionIDHash(sessionID) + ".lease"
}

func initialSessionStagingNamePrefix(sessionID string) string {
	return initialSessionStagingPrefix + strconv.Itoa(len(sessionID)) + "-" + sessionID + "-"
}

func sessionIDHash(sessionID string) string {
	digest := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(digest[:])
}

func isInitialSessionStagingName(name string) bool {
	_, ok := initialSessionStagingID(name)
	return ok
}

func initialSessionStagingID(name string) (string, bool) {
	if !strings.HasPrefix(name, initialSessionStagingPrefix) {
		return "", false
	}
	remainder := strings.TrimPrefix(name, initialSessionStagingPrefix)
	separator := strings.IndexByte(remainder, '-')
	if separator <= 0 {
		return "", false
	}
	length, err := strconv.Atoi(remainder[:separator])
	if err != nil || length <= 0 {
		return "", false
	}
	start := separator + 1
	end := start + length
	if end >= len(remainder) || remainder[end] != '-' || end == len(remainder)-1 {
		return "", false
	}
	sessionID := remainder[start:end]
	return sessionID, safeSessionID(sessionID)
}

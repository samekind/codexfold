package vfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func (s *Session) recover(ctx context.Context, writerLeaseHeld bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	records, err := readJournal(s.directory)
	if err != nil {
		return err
	}
	ordered, err := latestJournalRecords(records)
	if err != nil {
		return err
	}
	var recoveryLease *os.File
	recoveryLeaseHeld := writerLeaseHeld
	if !writerLeaseHeld {
		recoveryLease, err = acquireWriterLease(filepath.Join(s.directory, "writer.lease"))
		if errors.Is(err, ErrWriterBusy) && terminalJournalRecords(ordered) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("acquire session journal recovery lease: %w", err)
		}
		recoveryLeaseHeld = true
		defer func() {
			_ = unlockWriterFile(recoveryLease)
			_ = recoveryLease.Close()
		}()
		records, err = readJournal(s.directory)
		if err != nil {
			return err
		}
		ordered, err = latestJournalRecords(records)
		if err != nil {
			return err
		}
	}
	for _, record := range ordered {
		if record.SessionID != "" && record.SessionID != s.state.SessionID {
			return fmt.Errorf("journal operation %s belongs to another session", record.OperationID)
		}
		if record.Candidate.Version == legacySessionStateVersion && record.Candidate.ManifestSHA256 == "" {
			candidate, err := upgradeLegacySessionStateValue(record.Candidate, s.directory)
			if err != nil {
				return fmt.Errorf("upgrade legacy journal candidate %s: %w", record.OperationID, err)
			}
			record.Candidate = candidate
		}
		switch record.Phase {
		case "complete", "rolled-back":
			switch record.Kind {
			case "compact":
				state, err := s.loadJournalReplayState(recoveryLeaseHeld)
				if err != nil {
					return err
				}
				if err := s.validateCompactArtifactsInactive(record, state, record.Phase == "rolled-back"); err != nil {
					return err
				}
				if err := s.cleanupCompactArtifacts(record, record.Phase == "rolled-back"); err != nil {
					return err
				}
			case "copy-on-write":
				if record.Phase == "rolled-back" {
					if err := s.cleanupCOWTemporary(record); err != nil {
						return err
					}
				}
			default:
				return fmt.Errorf("journal operation %s has unknown kind %q", record.OperationID, record.Kind)
			}
			continue
		case "after-file-publish", "state-publishing", "state-published":
			if record.Candidate.SessionID != s.state.SessionID || record.Candidate.Generation == 0 || !pathWithin(s.directory, record.Candidate.DeltaPath) || (record.Candidate.BackingPath != "" && !pathWithin(s.directory, record.Candidate.BackingPath)) {
				return fmt.Errorf("journal operation %s has unsafe candidate state", record.OperationID)
			}
			state, err := s.loadJournalReplayState(recoveryLeaseHeld)
			if err != nil {
				return err
			}
			if record.Kind == "compact" {
				if state.Generation < record.Candidate.Generation {
					recovered, adopted, err := s.recoverPinnedCompactPublication(record, state)
					if err != nil {
						return err
					}
					if adopted {
						state = recovered
					} else {
						if err := s.cleanupCompactArtifacts(record, true); err != nil {
							return err
						}
						resolved := record
						resolved.Phase = "rolled-back"
						if err := appendJournal(s.directory, resolved); err != nil {
							return err
						}
						continue
					}
				}
				if state != record.Candidate {
					return fmt.Errorf("journal operation %s conflicts with current compacted state", record.OperationID)
				}
				s.state = state
			} else if record.Kind == "copy-on-write" {
				if err := s.validateCOWPublishedRecord(record, state); err != nil {
					return err
				}
				if state.Generation < record.Candidate.Generation {
					recovered, adopted, err := s.recoverPinnedCOWPublication(record, state)
					if err != nil {
						return err
					}
					if adopted {
						state = recovered
						s.state = recovered
					}
				}
				if state.Generation < record.Candidate.Generation {
					if err := publishSessionState(s.statePath, record.Candidate); err != nil {
						return err
					}
					s.state = record.Candidate
				}
			} else {
				return fmt.Errorf("journal operation %s has unknown kind %q", record.OperationID, record.Kind)
			}
			resolved := record
			resolved.Phase = "complete"
			if err := appendJournal(s.directory, resolved); err != nil {
				return err
			}
			if record.Kind == "compact" {
				if err := s.cleanupCompactArtifacts(resolved, false); err != nil {
					return err
				}
			}
		case "prepared", "data-synced":
			if record.Kind == "copy-on-write" {
				if err := s.cleanupCOWTemporary(record); err != nil {
					return err
				}
			} else if record.Kind == "compact" {
				state, err := s.loadJournalReplayState(recoveryLeaseHeld)
				if err != nil {
					return err
				}
				if err := s.validateCompactSuccessor(record, state); err != nil {
					return err
				}
				if err := s.cleanupCompactArtifacts(record, true); err != nil {
					return err
				}
			} else {
				return fmt.Errorf("journal operation %s has unknown kind %q", record.OperationID, record.Kind)
			}
			resolved := record
			resolved.Phase = "rolled-back"
			if err := appendJournal(s.directory, resolved); err != nil {
				return err
			}
		default:
			return fmt.Errorf("journal operation %s has unknown phase %q", record.OperationID, record.Phase)
		}
	}
	var persisted SessionState
	if recoveryLeaseHeld {
		persisted, err = loadSessionStateForJournalReplayWithWriterLease(s.statePath)
	} else {
		persisted, err = LoadSessionState(s.statePath)
	}
	if err != nil {
		return err
	}
	s.state = persisted
	if err := refreshSessionStateCheckpoint(s.statePath, persisted); err != nil {
		if !recoveryLeaseHeld {
			return err
		}
		recovered, recoverErr := LoadSessionStateWithWriterLease(s.statePath)
		if recoverErr != nil {
			return errors.Join(err, recoverErr)
		}
		s.state = recovered
		return refreshSessionStateCheckpoint(s.statePath, recovered)
	}
	return nil
}

func (s *Session) loadJournalReplayState(writerLeaseHeld bool) (SessionState, error) {
	if !writerLeaseHeld {
		return LoadSessionState(s.statePath)
	}
	return loadSessionStateForJournalReplayWithWriterLease(s.statePath)
}

func loadSessionStateForJournalReplayWithWriterLease(path string) (SessionState, error) {
	path = filepath.Clean(path)
	directory := filepath.Dir(path)
	sessionID := filepath.Base(directory)
	if filepath.Base(path) != "state.json" || !safeSessionID(sessionID) || filepath.Base(filepath.Dir(directory)) != "sessions" {
		return SessionState{}, errors.New("session journal recovery path is not canonical")
	}
	state, data, stateErr := readSessionState(path)
	catalogPresent, err := regularStateCatalogPresent(directory)
	if err != nil {
		return SessionState{}, err
	}
	if !catalogPresent {
		if stateErr != nil {
			return SessionState{}, stateErr
		}
		if err := validateSessionStateForDirectory(state, directory); err != nil {
			return SessionState{}, err
		}
		return state, nil
	}
	chain, err := loadCatalogStateCheckpoint(directory, sessionID)
	if err != nil {
		return SessionState{}, err
	}
	if stateErr == nil && state == chain.checkpoint.State && digestStateBytes(data) == chain.catalog.StateSHA256 {
		return state, nil
	}
	if err := validateSessionStateForDirectory(chain.checkpoint.State, directory); err != nil {
		return SessionState{}, err
	}
	return chain.checkpoint.State, nil
}

func (s *Session) validateCOWPublishedRecord(record JournalRecord, current SessionState) error {
	operationGeneration, validOperation := cowOperationGeneration(record.OperationID)
	if record.Candidate.Generation == 0 || !validOperation || operationGeneration != record.Candidate.Generation-1 {
		return fmt.Errorf("journal operation %s has an invalid copy-on-write identity", record.OperationID)
	}
	finalPath := filepath.Clean(record.FinalPath)
	if record.FinalPath == "" || record.Candidate.BackingPath == "" || finalPath != filepath.Clean(record.Candidate.BackingPath) {
		return fmt.Errorf("journal operation %s has incomplete published state", record.OperationID)
	}
	if err := validateSessionStateForDirectory(record.Candidate, s.directory); err != nil {
		return fmt.Errorf("journal operation %s has unsafe candidate state: %w", record.OperationID, err)
	}
	expectedName := fmt.Sprintf("backing-%020d.jsonl", record.Candidate.Generation)
	if filepath.Dir(finalPath) != filepath.Clean(s.directory) || filepath.Base(finalPath) != expectedName {
		return fmt.Errorf("journal operation %s has unsafe published backing %q", record.OperationID, record.FinalPath)
	}
	if !validCOWTemporaryPath(s.directory, record.Native.Path) || record.Native.Bytes < 0 || !validStateSHA256(record.Native.SHA256) {
		return fmt.Errorf("journal operation %s has invalid backing identity", record.OperationID)
	}
	if current.Generation == record.Candidate.Generation {
		if current != record.Candidate {
			return fmt.Errorf("journal operation %s conflicts with current session state", record.OperationID)
		}
	} else {
		expected := current
		if expected.Generation == ^uint64(0) {
			return fmt.Errorf("journal operation %s cannot advance the maximum generation", record.OperationID)
		}
		expected.Generation++
		expected.BackingPath = finalPath
		if expected != record.Candidate {
			return fmt.Errorf("journal operation %s is not an exact copy-on-write successor", record.OperationID)
		}
	}
	identity, err := captureRegularFileIdentity(finalPath)
	if err != nil || identity.Bytes != record.Native.Bytes || identity.SHA256 != record.Native.SHA256 {
		if err == nil {
			err = errors.New("published backing identity differs from its journal")
		}
		return fmt.Errorf("journal operation %s published backing cannot be verified: %w", record.OperationID, err)
	}
	return nil
}

func (s *Session) cleanupCOWTemporary(record JournalRecord) error {
	if _, validOperation := cowOperationGeneration(record.OperationID); !validOperation {
		return fmt.Errorf("journal operation %s has an invalid copy-on-write identity", record.OperationID)
	}
	if !validCOWTemporaryPath(s.directory, record.TempPath) || filepath.Clean(record.Native.Path) != filepath.Clean(record.TempPath) || record.Native.Bytes < 0 || !validStateSHA256(record.Native.SHA256) {
		return fmt.Errorf("journal operation %s has unsafe copy-on-write temporary", record.OperationID)
	}
	temporaryPath := filepath.Clean(record.TempPath)
	identity, err := captureRegularFileIdentity(temporaryPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify journal-owned copy-on-write temporary: %w", err)
	}
	if identity.Bytes != record.Native.Bytes || identity.SHA256 != record.Native.SHA256 {
		return fmt.Errorf("journal operation %s copy-on-write temporary differs from its durable identity", record.OperationID)
	}
	if err := os.Remove(temporaryPath); err != nil {
		return fmt.Errorf("remove journal-owned copy-on-write temporary: %w", err)
	}
	return syncStateDirectory(s.directory)
}

func cowOperationGeneration(operationID string) (uint64, bool) {
	const prefix = "cow-"
	if !strings.HasPrefix(operationID, prefix) {
		return 0, false
	}
	token := strings.TrimPrefix(operationID, prefix)
	if len(token) != 20 {
		return 0, false
	}
	for _, character := range token {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	generation, err := strconv.ParseUint(token, 10, 64)
	return generation, err == nil
}

func validCOWTemporaryPath(directory string, path string) bool {
	if path == "" {
		return false
	}
	path = filepath.Clean(path)
	name := filepath.Base(path)
	if filepath.Dir(path) != filepath.Clean(directory) || !strings.HasPrefix(name, ".backing-") || !strings.HasSuffix(name, ".tmp") {
		return false
	}
	token := strings.TrimSuffix(strings.TrimPrefix(name, ".backing-"), ".tmp")
	return safeStateTemporaryToken(token)
}

func (s *Session) recoverPinnedCompactPublication(record JournalRecord, current SessionState) (SessionState, bool, error) {
	if err := s.validateCompactJournalRecord(record); err != nil {
		return SessionState{}, false, err
	}
	operationGeneration, _ := compactOperationGeneration(record.OperationID)
	if current.Generation != operationGeneration {
		return SessionState{}, false, fmt.Errorf("journal operation %s is not a successor of the current compact state", record.OperationID)
	}
	if err := s.validateCompactSuccessor(record, current); err != nil {
		return SessionState{}, false, err
	}
	_, pinned, err := retirementCheckpointPin(s.directory)
	if err != nil || !pinned {
		return current, false, err
	}
	// A pinned direct-child checkpoint is the durable publication evidence. The
	// scratch and primary-state temporary are not checkpoint artifacts, so make
	// their cleanup durable before asking checkpoint recovery to adopt or reject
	// the candidate. The candidate delta remains untouched until that decision.
	if err := s.cleanupCompactArtifacts(record, false); err != nil {
		return SessionState{}, false, err
	}
	recovered, err := LoadSessionStateWithWriterLease(s.statePath)
	if err != nil {
		return SessionState{}, false, err
	}
	switch recovered {
	case record.Candidate:
		return recovered, true, nil
	case current:
		return recovered, false, nil
	default:
		return SessionState{}, false, fmt.Errorf("journal operation %s conflicts with recovered compact state", record.OperationID)
	}
}

func (s *Session) recoverPinnedCOWPublication(record JournalRecord, current SessionState) (SessionState, bool, error) {
	_, pinned, err := retirementCheckpointPin(s.directory)
	if err != nil || !pinned {
		return current, false, err
	}
	evidence, err := hasInterruptedStateCheckpointEvidence(s.directory, current.SessionID)
	if err != nil || !evidence {
		return current, false, err
	}
	recovered, err := LoadSessionStateWithWriterLease(s.statePath)
	if err != nil {
		return SessionState{}, false, err
	}
	switch recovered {
	case record.Candidate:
		return recovered, true, nil
	case current:
		return recovered, false, nil
	default:
		return SessionState{}, false, fmt.Errorf("journal operation %s conflicts with recovered copy-on-write state", record.OperationID)
	}
}

func hasInterruptedStateCheckpointEvidence(directory string, sessionID string) (bool, error) {
	chain, err := loadStateCheckpointChain(directory, sessionID, false)
	if err != nil {
		return false, err
	}
	lineageNames := make(map[string]struct{}, len(chain.lineage))
	for digest, checkpoint := range chain.lineage {
		lineageNames[stateCheckpointFilename(checkpoint.State.Generation, checkpoint.Sequence, digest)] = struct{}{}
	}
	generations := filepath.Join(directory, stateGenerationsDirectoryName)
	entries, err := os.ReadDir(generations)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if _, exists := lineageNames[entry.Name()]; exists {
			continue
		}
		path := filepath.Join(generations, entry.Name())
		if strings.HasPrefix(entry.Name(), ".state-checkpoint-") && strings.HasSuffix(entry.Name(), ".tmp") {
			data, err := readBoundedStateCheckpointFile(path)
			if err != nil {
				return false, err
			}
			checkpoint, err := decodeStateCheckpoint(data)
			if err == nil && isDirectStateCheckpointChild(chain, checkpoint) {
				return true, nil
			}
			continue
		}
		// Any non-lineage final must go through the pinned recovery validator;
		// publishing around it would quarantine evidence before deciding authority.
		return true, nil
	}
	return false, nil
}

// RecoverSessionJournalWithWriterLease replays a canonical session journal
// while the caller owns writer.lease. It never acquires or releases that lease.
func RecoverSessionJournalWithWriterLease(ctx context.Context, statePath string) (SessionState, error) {
	if err := ctx.Err(); err != nil {
		return SessionState{}, err
	}
	state, err := loadSessionStateForJournalReplayWithWriterLease(statePath)
	if err != nil {
		return SessionState{}, err
	}
	statePath = filepath.Clean(statePath)
	session := &Session{state: state, statePath: statePath, directory: filepath.Dir(statePath)}
	if err := session.recover(ctx, true); err != nil {
		return SessionState{}, err
	}
	return LoadSessionStateWithWriterLease(statePath)
}

func latestJournalRecords(records []JournalRecord) ([]JournalRecord, error) {
	latest := make(map[string]JournalRecord)
	lastPosition := make(map[string]int)
	for index, record := range records {
		if record.OperationID == "" {
			return nil, errors.New("session journal record has no operation ID")
		}
		latest[record.OperationID] = record
		lastPosition[record.OperationID] = index
	}
	ordered := make([]JournalRecord, 0, len(latest))
	for _, record := range latest {
		ordered = append(ordered, record)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return lastPosition[ordered[i].OperationID] < lastPosition[ordered[j].OperationID]
	})
	return ordered, nil
}

func terminalJournalRecords(records []JournalRecord) bool {
	for _, record := range records {
		if record.Phase != "complete" && record.Phase != "rolled-back" {
			return false
		}
	}
	return true
}

func (s *Session) cleanupCompactArtifacts(record JournalRecord, rollback bool) error {
	if err := s.validateCompactJournalRecord(record); err != nil {
		return err
	}
	removed := false
	removeOwned := func(candidate string, validate func(os.FileInfo) error) error {
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("journal operation %s compact artifact is not a regular file: %s", record.OperationID, filepath.Base(candidate))
		}
		if validate != nil {
			if err := validate(info); err != nil {
				return err
			}
		}
		if err := os.Remove(candidate); err != nil {
			return err
		}
		removed = true
		return nil
	}
	if err := removeOwned(filepath.Clean(record.Native.Path), func(os.FileInfo) error {
		identity, err := captureRegularFileIdentity(filepath.Clean(record.Native.Path))
		if err != nil {
			return err
		}
		if identity.Bytes != record.Native.Bytes || identity.SHA256 != record.Native.SHA256 {
			return fmt.Errorf("journal operation %s compact scratch differs from its durable identity", record.OperationID)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := removeOwned(filepath.Clean(record.TempPath), func(info os.FileInfo) error {
		if info.Size() > maximumStateCheckpointBytes {
			return fmt.Errorf("journal operation %s compact state temporary is too large", record.OperationID)
		}
		return nil
	}); err != nil {
		return err
	}
	if rollback {
		if err := removeOwned(filepath.Clean(record.FinalPath), func(info os.FileInfo) error {
			if info.Size() != 0 {
				return fmt.Errorf("journal operation %s compact rollback delta is not empty", record.OperationID)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if removed {
		return syncStateDirectory(s.directory)
	}
	return nil
}

func (s *Session) validateCompactJournalRecord(record JournalRecord) error {
	operationGeneration, validOperation := compactOperationGeneration(record.OperationID)
	if !validOperation || operationGeneration == ^uint64(0) || record.Candidate.Generation != operationGeneration+1 {
		return fmt.Errorf("journal operation %s has an invalid compact identity", record.OperationID)
	}
	if err := validateSessionStateForDirectory(record.Candidate, s.directory); err != nil {
		return fmt.Errorf("journal operation %s has an unsafe compact candidate: %w", record.OperationID, err)
	}
	expectedScratch := filepath.Join(s.directory, fmt.Sprintf(".compact-%020d.jsonl", operationGeneration))
	expectedTemporary := filepath.Join(s.directory, fmt.Sprintf(".state-compact-%020d.tmp", record.Candidate.Generation))
	expectedDelta := filepath.Join(s.directory, fmt.Sprintf("delta-%020d.jsonl", record.Candidate.Generation))
	if filepath.Clean(record.Native.Path) != expectedScratch || filepath.Clean(record.TempPath) != expectedTemporary || filepath.Clean(record.FinalPath) != expectedDelta || filepath.Clean(record.Candidate.DeltaPath) != expectedDelta || record.Candidate.BackingPath != "" {
		return fmt.Errorf("journal operation %s has unsafe compact artifact paths", record.OperationID)
	}
	if record.Native.Bytes < 0 || !validStateSHA256(record.Native.SHA256) || record.Candidate.BaseBytes != record.Native.Bytes || record.Candidate.BaseSHA256 != record.Native.SHA256 {
		return fmt.Errorf("journal operation %s has an invalid compact source identity", record.OperationID)
	}
	return nil
}

func (s *Session) validateCompactSuccessor(record JournalRecord, current SessionState) error {
	if err := s.validateCompactJournalRecord(record); err != nil {
		return err
	}
	operationGeneration, _ := compactOperationGeneration(record.OperationID)
	if current.SessionID != record.Candidate.SessionID || current.Generation != operationGeneration || record.Candidate.Generation != current.Generation+1 || record.Candidate.Version != current.Version || record.Candidate.NativeSnapshot != current.NativeSnapshot {
		return fmt.Errorf("journal operation %s is not an exact compact successor", record.OperationID)
	}
	return s.validateCompactArtifactsInactive(record, current, true)
}

func (s *Session) validateCompactArtifactsInactive(record JournalRecord, current SessionState, rollback bool) error {
	for label, path := range map[string]string{
		"scratch":   record.Native.Path,
		"temporary": record.TempPath,
	} {
		path = filepath.Clean(path)
		if path == filepath.Clean(current.DeltaPath) || current.BackingPath != "" && path == filepath.Clean(current.BackingPath) {
			return fmt.Errorf("journal operation %s compact %s overlaps current session data", record.OperationID, label)
		}
	}
	if rollback {
		finalPath := filepath.Clean(record.FinalPath)
		if finalPath == filepath.Clean(current.DeltaPath) || current.BackingPath != "" && finalPath == filepath.Clean(current.BackingPath) {
			return fmt.Errorf("journal operation %s compact rollback overlaps current session data", record.OperationID)
		}
	}
	return nil
}

func compactOperationGeneration(operationID string) (uint64, bool) {
	const prefix = "compact-"
	if !strings.HasPrefix(operationID, prefix) {
		return 0, false
	}
	token := strings.TrimPrefix(operationID, prefix)
	if len(token) != 20 {
		return 0, false
	}
	for _, character := range token {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	generation, err := strconv.ParseUint(token, 10, 64)
	return generation, err == nil
}

func (s *Session) Recover(ctx context.Context) error {
	if s.recoveryDeferred {
		return ErrSessionRecoveryDeferred
	}
	return s.recover(ctx, false)
}

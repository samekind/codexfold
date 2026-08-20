package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// LoadSessionStateWithWriterLease validates or recovers canonical state while
// the caller already owns writer.lease. It avoids recursively acquiring the
// same lock when a higher-level crash transaction must keep that lease held
// across a directory rename and state replay.
func LoadSessionStateWithWriterLease(path string) (SessionState, error) {
	path = filepath.Clean(path)
	directory := filepath.Dir(path)
	sessionID := filepath.Base(directory)
	if filepath.Base(path) != "state.json" || !safeSessionID(sessionID) || filepath.Base(filepath.Dir(directory)) != "sessions" {
		return SessionState{}, errors.New("session state recovery path is not canonical")
	}
	state, data, stateErr := readSessionState(path)
	catalogPresent, metadataErr := regularStateCatalogPresent(directory)
	if metadataErr != nil {
		return SessionState{}, metadataErr
	}
	if !catalogPresent {
		return state, stateErr
	}
	chain, chainErr := loadCatalogStateCheckpoint(directory, sessionID)
	if chainErr != nil {
		return SessionState{}, chainErr
	}
	if stateErr == nil && digestStateBytes(data) == chain.catalog.StateSHA256 && state == chain.checkpoint.State {
		if err := recoverPinnedStateCheckpointPublication(directory, sessionID); err != nil {
			return SessionState{}, err
		}
		recovered, recoveredData, err := readSessionState(path)
		if err != nil {
			return SessionState{}, err
		}
		updated, err := loadStateCheckpointChain(directory, sessionID, true)
		if err != nil || recovered != updated.checkpoint.State || digestStateBytes(recoveredData) != updated.catalog.StateSHA256 {
			if err == nil {
				err = errors.New("interrupted state checkpoint recovery did not publish its primary state")
			}
			return SessionState{}, err
		}
		if err := cleanupSupersededStateMetadataTemporaries(directory, updated); err != nil {
			return SessionState{}, err
		}
		return recovered, nil
	}
	chain, err := loadStateCheckpointChain(directory, sessionID, true)
	if err != nil {
		return SessionState{}, err
	}
	if err := verifyStateCheckpointArtifactsForChain(directory, chain); err != nil {
		return SessionState{}, err
	}
	if err := verifyStateCheckpointArtifactsForChain(directory, chain); err != nil {
		return SessionState{}, err
	}
	if err := writeSessionState(path, chain.checkpoint.State); err != nil {
		return SessionState{}, err
	}
	recovered, recoveredData, err := readSessionState(path)
	if err != nil {
		return SessionState{}, err
	}
	if recovered != chain.checkpoint.State || digestStateBytes(recoveredData) != chain.catalog.StateSHA256 {
		return SessionState{}, errors.New("restored session state does not match its checkpoint")
	}
	if err := cleanupSupersededStateMetadataTemporaries(directory, chain); err != nil {
		return SessionState{}, err
	}
	return recovered, nil
}

func cleanupSupersededStateMetadataTemporaries(directory string, chain stateCheckpointChain) error {
	persisted, data, err := readSessionState(filepath.Join(directory, "state.json"))
	if err != nil || persisted != chain.checkpoint.State || digestStateBytes(data) != chain.catalog.StateSHA256 {
		return errors.New("primary session state is not authoritative enough to clean metadata temporaries")
	}
	if err := verifyStateCheckpointArtifactsForChain(directory, chain); err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	paths := make([]string, 0)
	for _, entry := range entries {
		if !recoverableStateMetadataTemporaryName(entry.Name()) {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > maximumStateCheckpointBytes {
			return errors.New("session state metadata temporary is not a bounded regular file")
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return nil
	}
	refreshed, err := loadStateCheckpointChain(directory, chain.catalog.SessionID, true)
	if err != nil || refreshed.catalog != chain.catalog {
		return errors.New("session state checkpoint changed before metadata temporary cleanup")
	}
	persisted, data, err = readSessionState(filepath.Join(directory, "state.json"))
	if err != nil || persisted != chain.checkpoint.State || digestStateBytes(data) != chain.catalog.StateSHA256 {
		return errors.New("primary session state changed before metadata temporary cleanup")
	}
	if err := verifyStateCheckpointArtifactsForChain(directory, chain); err != nil {
		return err
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return syncStateDirectory(directory)
}

func recoverableStateMetadataTemporaryName(name string) bool {
	for _, prefix := range []string{".state-primary-", ".state-catalog-"} {
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".tmp") {
			return safeStateTemporaryToken(strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".tmp"))
		}
	}
	if !strings.HasPrefix(name, ".state-") || !strings.HasSuffix(name, ".tmp") {
		return false
	}
	token := strings.TrimSuffix(strings.TrimPrefix(name, ".state-"), ".tmp")
	if token == "" {
		return false
	}
	for _, character := range token {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func safeStateTemporaryToken(token string) bool {
	if token == "" {
		return false
	}
	for _, character := range token {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' {
			continue
		}
		return false
	}
	return true
}

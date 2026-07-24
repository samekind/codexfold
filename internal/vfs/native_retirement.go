package vfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const nativeRetirementVersion = 1
const NativeRetirementFilename = "native-retirement.json"

type NativeRetirementProof struct {
	Version         int        `json:"version"`
	SessionID       string     `json:"session_id"`
	StateGeneration uint64     `json:"state_generation"`
	RetiredAt       string     `json:"retired_at"`
	Snapshot        NativeFile `json:"snapshot"`
	Visible         NativeFile `json:"visible"`
}

func (s *Session) RetireNativeSnapshot(snapshot NativeFile, visible NativeFile) (NativeRetirementProof, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writerOpen {
		return NativeRetirementProof{}, errors.New("native snapshot retirement requires a writer lease")
	}
	if snapshot.Path == "" || s.state.NativeSnapshot != snapshot {
		return NativeRetirementProof{}, errors.New("native snapshot retirement does not match session state")
	}
	storeRoot := filepath.Dir(filepath.Dir(filepath.Dir(s.directory)))
	expectedSnapshot := filepath.Join(storeRoot, "fs", "snapshots", s.state.SessionID, "native.jsonl")
	if filepath.Clean(snapshot.Path) != filepath.Clean(expectedSnapshot) {
		return NativeRetirementProof{}, errors.New("only the managed canonical snapshot can be retired")
	}
	if visible.Path == "" || visible.Bytes < s.state.BaseBytes || len(visible.SHA256) != 64 {
		return NativeRetirementProof{}, errors.New("verified current materialization is required")
	}
	proof := NativeRetirementProof{
		Version: nativeRetirementVersion, SessionID: s.state.SessionID,
		StateGeneration: s.state.Generation, RetiredAt: time.Now().UTC().Format(time.RFC3339Nano),
		Snapshot: snapshot, Visible: visible,
	}
	proofPath := filepath.Join(s.directory, NativeRetirementFilename)
	if err := writeNativeRetirementProof(proofPath, proof); err != nil {
		return NativeRetirementProof{}, err
	}
	next := s.state
	next.NativeSnapshot = NativeFile{}
	if err := writeSessionState(s.statePath, next); err != nil {
		_ = os.Remove(proofPath)
		return NativeRetirementProof{}, err
	}
	s.state = next
	if err := CompleteNativeSnapshotRetirement(storeRoot, proof); err != nil {
		return proof, err
	}
	return proof, nil
}

func CompleteNativeSnapshotRetirement(storeRoot string, proof NativeRetirementProof) error {
	expectedSnapshot := filepath.Join(filepath.Clean(storeRoot), "fs", "snapshots", proof.SessionID, "native.jsonl")
	if filepath.Clean(proof.Snapshot.Path) != expectedSnapshot {
		return errors.New("native retirement proof does not reference the canonical snapshot")
	}
	if _, err := os.Stat(proof.Snapshot.Path); err == nil {
		if err := verifyNativeFile(proof.Snapshot); err != nil {
			return fmt.Errorf("verify retired native snapshot before deletion: %w", err)
		}
		if err := os.Remove(proof.Snapshot.Path); err != nil {
			return fmt.Errorf("remove retired native snapshot: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat retired native snapshot: %w", err)
	}
	sidecar := filepath.Join(filepath.Dir(proof.Snapshot.Path), "._"+filepath.Base(proof.Snapshot.Path))
	if err := os.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove retired native snapshot sidecar: %w", err)
	}
	_ = os.Remove(filepath.Dir(proof.Snapshot.Path))
	if err := syncStateDirectory(filepath.Dir(filepath.Dir(proof.Snapshot.Path))); err != nil {
		return err
	}
	return nil
}

func LoadNativeRetirementProof(path string) (NativeRetirementProof, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return NativeRetirementProof{}, err
	}
	var proof NativeRetirementProof
	if err := json.Unmarshal(data, &proof); err != nil {
		return NativeRetirementProof{}, fmt.Errorf("decode native retirement proof: %w", err)
	}
	if proof.Version != nativeRetirementVersion || !safeSessionID(proof.SessionID) || proof.StateGeneration == 0 || proof.RetiredAt == "" || proof.Snapshot.Path == "" || proof.Snapshot.Bytes < 0 || len(proof.Snapshot.SHA256) != 64 || proof.Visible.Path == "" || proof.Visible.Bytes < 0 || len(proof.Visible.SHA256) != 64 {
		return NativeRetirementProof{}, errors.New("invalid native retirement proof")
	}
	return proof, nil
}

func writeNativeRetirementProof(path string, proof NativeRetirementProof) error {
	data, err := json.MarshalIndent(proof, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".native-retirement-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
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

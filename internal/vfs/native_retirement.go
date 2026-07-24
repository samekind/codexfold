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
	if s.state.Generation == ^uint64(0) {
		return NativeRetirementProof{}, errors.New("session generation cannot advance")
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
	next.Generation++
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

// refreshNativeRetirementLocked absorbs the one external state transition that
// is safe for a still-serving session: retirement of its managed native
// snapshot. The caller must already hold the session mutex and writer lease.
// That lease ensures a retirement command cannot race a new COW or compact
// commit after the persisted state has been observed.
func (s *Session) refreshNativeRetirementLocked() error {
	persisted, err := loadSessionState(s.statePath)
	if err != nil {
		return fmt.Errorf("reload session state before write: %w", err)
	}
	if persisted == s.state {
		return nil
	}
	current := s.state
	if current.Generation == ^uint64(0) || persisted.Generation != current.Generation+1 || current.NativeSnapshot.Path == "" || persisted.NativeSnapshot != (NativeFile{}) || !sameSessionStateExceptRetiredNative(current, persisted) {
		return errors.New("persisted session state changed outside the serving session")
	}
	proofPath := filepath.Join(s.directory, NativeRetirementFilename)
	proof, err := LoadNativeRetirementProof(proofPath)
	if err != nil {
		return fmt.Errorf("load native retirement proof before write: %w", err)
	}
	if proof.SessionID != current.SessionID || proof.StateGeneration != current.Generation || proof.Snapshot != current.NativeSnapshot {
		return errors.New("native retirement proof does not match the serving session")
	}
	s.state = persisted
	return nil
}

func sameSessionStateExceptRetiredNative(before SessionState, after SessionState) bool {
	return before.Version == after.Version &&
		before.SessionID == after.SessionID &&
		before.ManifestPath == after.ManifestPath &&
		before.BaseBytes == after.BaseBytes &&
		before.BaseSHA256 == after.BaseSHA256 &&
		before.DeltaPath == after.DeltaPath &&
		before.BackingPath == after.BackingPath
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

// NativeSnapshotAlreadyRetired distinguishes a deliberately removed managed
// snapshot from an unexplained missing fallback. It is intentionally strict:
// rollback may skip moving a missing snapshot only when the durable proof
// names the exact snapshot still referenced by state.
func NativeSnapshotAlreadyRetired(storeRoot string, state SessionState) (bool, error) {
	if state.NativeSnapshot.Path == "" {
		return false, nil
	}
	if _, err := os.Stat(state.NativeSnapshot.Path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("stat managed native snapshot: %w", err)
	}
	proofPath := filepath.Join(filepath.Clean(storeRoot), "fs", "sessions", state.SessionID, NativeRetirementFilename)
	proof, err := LoadNativeRetirementProof(proofPath)
	if err != nil {
		return false, fmt.Errorf("load proof for missing native snapshot: %w", err)
	}
	expectedSnapshot := filepath.Join(filepath.Clean(storeRoot), "fs", "snapshots", state.SessionID, "native.jsonl")
	if filepath.Clean(state.NativeSnapshot.Path) != expectedSnapshot || proof.SessionID != state.SessionID || proof.Snapshot != state.NativeSnapshot || proof.StateGeneration > state.Generation {
		return false, errors.New("missing native snapshot does not match its retirement proof")
	}
	return true, nil
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

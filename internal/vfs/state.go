package vfs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const sessionStateVersion = 1

type NativeFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type SessionState struct {
	Version        int        `json:"version"`
	SessionID      string     `json:"session_id"`
	Generation     uint64     `json:"generation"`
	ManifestPath   string     `json:"manifest_path"`
	BaseBytes      int64      `json:"base_bytes"`
	BaseSHA256     string     `json:"base_sha256"`
	DeltaPath      string     `json:"delta_path"`
	BackingPath    string     `json:"backing_path,omitempty"`
	NativeSnapshot NativeFile `json:"native_snapshot"`
}

func loadSessionState(path string) (SessionState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionState{}, err
	}
	var state SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return SessionState{}, fmt.Errorf("decode session state: %w", err)
	}
	if state.Version != sessionStateVersion || !safeSessionID(state.SessionID) || state.Generation == 0 || state.BaseBytes < 0 || len(state.BaseSHA256) != 64 || state.DeltaPath == "" {
		return SessionState{}, errors.New("invalid virtual session state")
	}
	return state, nil
}

func LoadSessionState(path string) (SessionState, error) {
	state, err := loadSessionState(path)
	if err != nil {
		return SessionState{}, err
	}
	directory := filepath.Dir(filepath.Clean(path))
	if filepath.Base(directory) != state.SessionID || filepath.Base(filepath.Dir(directory)) != "sessions" {
		return SessionState{}, errors.New("session state path does not match its session ID")
	}
	if !pathWithin(directory, state.DeltaPath) || (state.BackingPath != "" && !pathWithin(directory, state.BackingPath)) {
		return SessionState{}, errors.New("session state contains an unsafe data path")
	}
	return state, nil
}

func RepublishSessionState(path string) (SessionState, error) {
	state, err := LoadSessionState(path)
	if err != nil {
		return SessionState{}, err
	}
	if state.Generation == ^uint64(0) {
		return SessionState{}, errors.New("session generation cannot advance")
	}
	state.Generation++
	if err := writeSessionState(path, state); err != nil {
		return SessionState{}, err
	}
	return state, nil
}

func DiscoverSessionStates(root string) ([]SessionState, error) {
	directory := filepath.Join(root, "fs", "sessions")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read managed session states: %w", err)
	}
	states := make([]SessionState, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		state, err := LoadSessionState(filepath.Join(directory, entry.Name(), "state.json"))
		if err != nil {
			return nil, fmt.Errorf("load managed session %s: %w", entry.Name(), err)
		}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].SessionID < states[j].SessionID })
	return states, nil
}

func writeSessionState(path string, state SessionState) error {
	data, err := encodeSessionState(state)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary session state: %w", err)
	}
	return commitSessionState(path, data, temporary)
}

func writeSessionStateWithTemporary(path string, temporaryPath string, state SessionState) error {
	directory := filepath.Clean(filepath.Dir(path))
	temporaryPath = filepath.Clean(temporaryPath)
	if filepath.Dir(temporaryPath) != directory {
		return errors.New("temporary session state must be in the state directory")
	}
	data, err := encodeSessionState(state)
	if err != nil {
		return err
	}
	temporary, err := os.OpenFile(temporaryPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary session state: %w", err)
	}
	return commitSessionState(path, data, temporary)
}

func encodeSessionState(state SessionState) ([]byte, error) {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode session state: %w", err)
	}
	return append(data, '\n'), nil
}

func commitSessionState(path string, data []byte, temporary *os.File) error {
	directory := filepath.Dir(path)
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("chmod temporary session state: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary session state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary session state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary session state: %w", err)
	}
	if err := replaceStateFile(temporaryPath, path); err != nil {
		return fmt.Errorf("commit session state: %w", err)
	}
	return syncStateDirectory(directory)
}

func verifyNativeFile(file NativeFile) error {
	if file.Path == "" || file.Bytes < 0 || len(file.SHA256) != 64 {
		return errors.New("native snapshot metadata is incomplete")
	}
	opened, err := os.Open(file.Path)
	if err != nil {
		return fmt.Errorf("open native snapshot: %w", err)
	}
	hasher := sha256.New()
	bytesRead, copyErr := io.Copy(hasher, opened)
	closeErr := opened.Close()
	if copyErr != nil {
		return fmt.Errorf("hash native snapshot: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close native snapshot: %w", closeErr)
	}
	if bytesRead != file.Bytes || hex.EncodeToString(hasher.Sum(nil)) != file.SHA256 {
		return errors.New("native snapshot bytes or SHA-256 differ from metadata")
	}
	return nil
}

func safeSessionID(sessionID string) bool {
	return sessionID != "" && sessionID != "." && sessionID != ".." && !strings.ContainsAny(sessionID, "/\\\x00")
}

func pathWithin(directory string, path string) bool {
	relative, err := filepath.Rel(directory, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

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

const (
	sessionStateVersion       = 2
	legacySessionStateVersion = 1
)

var errLegacySessionState = errors.New("legacy virtual session state requires migration")

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
	ManifestSHA256 string     `json:"manifest_sha256"`
	BaseBytes      int64      `json:"base_bytes"`
	BaseSHA256     string     `json:"base_sha256"`
	DeltaPath      string     `json:"delta_path"`
	BackingPath    string     `json:"backing_path,omitempty"`
	NativeSnapshot NativeFile `json:"native_snapshot"`
}

type SessionStateIssueKind string

const (
	SessionStateIssueStaging      SessionStateIssueKind = "incomplete-initial-publication"
	SessionStateIssueMissingState SessionStateIssueKind = "missing-state"
	SessionStateIssueInvalidState SessionStateIssueKind = "invalid-state"
)

type SessionStateIssue struct {
	SessionID string
	Path      string
	Kind      SessionStateIssueKind
	Err       error
}

type SessionStateIssuesError struct {
	Issues []SessionStateIssue
}

func (e *SessionStateIssuesError) Error() string {
	return fmt.Sprintf("managed session discovery found %d isolated state issue(s)", len(e.Issues))
}

func (e *SessionStateIssuesError) Unwrap() []error {
	errors := make([]error, 0, len(e.Issues))
	for _, issue := range e.Issues {
		if issue.Err != nil {
			errors = append(errors, issue.Err)
		}
	}
	return errors
}

func loadSessionState(path string) (SessionState, error) {
	state, _, err := readSessionState(path)
	return state, err
}

func readSessionState(path string) (SessionState, []byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return SessionState{}, nil, err
	}
	if !info.Mode().IsRegular() {
		return SessionState{}, nil, errors.New("session state is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionState{}, nil, err
	}
	var state SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return SessionState{}, nil, fmt.Errorf("decode session state: %w", err)
	}
	if state.NativeSnapshot.Path == "" {
		if state.NativeSnapshot.Bytes != 0 || state.NativeSnapshot.SHA256 != "" {
			return SessionState{}, nil, errors.New("session state contains partial native snapshot metadata")
		}
	} else if state.NativeSnapshot.Bytes < 0 || !validStateSHA256(state.NativeSnapshot.SHA256) {
		return SessionState{}, nil, errors.New("session state contains invalid native snapshot metadata")
	}
	if state.Version == legacySessionStateVersion && state.ManifestSHA256 == "" {
		if !safeSessionID(state.SessionID) || state.Generation == 0 || state.BaseBytes < 0 || !validStateSHA256(state.BaseSHA256) || state.DeltaPath == "" {
			return SessionState{}, nil, errors.New("invalid legacy virtual session state")
		}
		return state, data, errLegacySessionState
	}
	if state.Version != sessionStateVersion || !safeSessionID(state.SessionID) || state.Generation == 0 || state.BaseBytes < 0 || !validStateSHA256(state.BaseSHA256) || !validStateSHA256(state.ManifestSHA256) || state.DeltaPath == "" {
		return SessionState{}, nil, errors.New("invalid virtual session state")
	}
	return state, data, nil
}

func LoadSessionState(path string) (SessionState, error) {
	state, err := loadPublishedSessionState(path)
	if err != nil {
		return SessionState{}, err
	}
	directory := filepath.Dir(filepath.Clean(path))
	if err := validateSessionStateForDirectory(state, directory); err != nil {
		return SessionState{}, err
	}
	return state, nil
}

// InspectSessionState validates the published state and checkpoint metadata
// without repairing, migrating, or otherwise changing the session directory.
func InspectSessionState(path string) (SessionState, error) {
	state, err := inspectPublishedSessionState(path)
	if err != nil {
		return SessionState{}, err
	}
	directory := filepath.Dir(filepath.Clean(path))
	if err := validateSessionStateForDirectory(state, directory); err != nil {
		return SessionState{}, err
	}
	return state, nil
}

func RepublishSessionState(path string) (SessionState, error) {
	lease, err := acquireWriterLease(filepath.Join(filepath.Dir(path), "writer.lease"))
	if err != nil {
		return SessionState{}, err
	}
	defer func() {
		_ = unlockWriterFile(lease)
		_ = lease.Close()
	}()
	return republishSessionStateWithHeldLease(path)
}

// RepublishSessionStateWithWriterLease is for recovery code that already owns
// the session's cross-process writer lease. Calling it without that lease is unsafe.
func RepublishSessionStateWithWriterLease(path string) (SessionState, error) {
	return republishSessionStateWithHeldLease(path)
}

func republishSessionStateWithHeldLease(path string) (SessionState, error) {
	state, err := LoadSessionState(path)
	if err != nil {
		return SessionState{}, err
	}
	if state.Generation == ^uint64(0) {
		return SessionState{}, errors.New("session generation cannot advance")
	}
	manifest, _, err := captureManifestIdentity(state.ManifestPath, state.SessionID, state.BaseBytes, state.BaseSHA256)
	if err != nil {
		return SessionState{}, fmt.Errorf("verify manifest before republishing session state: %w", err)
	}
	state.ManifestSHA256 = manifest.SHA256
	state.Generation++
	if err := publishSessionState(path, state); err != nil {
		return SessionState{}, err
	}
	return state, nil
}

func DiscoverSessionStates(root string) ([]SessionState, error) {
	states, issues, err := DiscoverSessionStatesDetailed(root)
	if err != nil || len(issues) == 0 {
		return states, err
	}
	return states, &SessionStateIssuesError{Issues: issues}
}

func DiscoverSessionStatesDetailed(root string) ([]SessionState, []SessionStateIssue, error) {
	return discoverSessionStatesDetailed(root, LoadSessionState)
}

// DiscoverSessionStatesDetailedReadOnly never repairs or migrates session
// metadata. Callers that may mutate storage only after external proofs are
// available can inspect first, establish those proofs, and then recover.
func DiscoverSessionStatesDetailedReadOnly(root string) ([]SessionState, []SessionStateIssue, error) {
	return discoverSessionStatesDetailed(root, InspectSessionState)
}

func discoverSessionStatesDetailed(root string, load func(string) (SessionState, error)) ([]SessionState, []SessionStateIssue, error) {
	root = filepath.Clean(root)
	storeRoot, err := os.OpenRoot(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("open managed session store: %w", err)
	}
	defer storeRoot.Close()
	directory := filepath.Join(root, "fs", "sessions")
	sessionsRootInfo, err := storeRoot.Lstat(filepath.Join("fs", "sessions"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("inspect managed session state root: %w", err)
	}
	if !sessionsRootInfo.IsDir() || sessionsRootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("managed session state root is not a real directory")
	}
	sessionsDirectory, err := storeRoot.Open(filepath.Join("fs", "sessions"))
	if err != nil {
		return nil, nil, fmt.Errorf("open managed session state root: %w", err)
	}
	defer sessionsDirectory.Close()
	entries, err := sessionsDirectory.ReadDir(-1)
	if err != nil {
		return nil, nil, fmt.Errorf("read managed session states: %w", err)
	}
	states := make([]SessionState, 0, len(entries))
	issues := make([]SessionStateIssue, 0)
	for _, entry := range entries {
		entryRelative := filepath.Join("fs", "sessions", entry.Name())
		entryInfo, entryErr := storeRoot.Lstat(entryRelative)
		if entryErr != nil {
			issues = append(issues, SessionStateIssue{
				SessionID: entry.Name(), Path: filepath.Join(directory, entry.Name()), Kind: SessionStateIssueInvalidState,
				Err: fmt.Errorf("inspect managed session entry: %w", entryErr),
			})
			continue
		}
		if !entry.IsDir() {
			if safeSessionID(entry.Name()) && !strings.HasPrefix(entry.Name(), initialSessionStagingPrefix) {
				issues = append(issues, SessionStateIssue{
					SessionID: entry.Name(), Path: filepath.Join(directory, entry.Name()), Kind: SessionStateIssueInvalidState,
					Err: errors.New("managed session entry is not a directory"),
				})
			}
			continue
		}
		entryPath := filepath.Join(directory, entry.Name())
		if entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.IsDir() {
			issues = append(issues, SessionStateIssue{
				SessionID: entry.Name(), Path: entryPath, Kind: SessionStateIssueInvalidState,
				Err: errors.New("managed session entry is not a real directory"),
			})
			continue
		}
		if sessionID, ok := initialSessionStagingID(entry.Name()); ok {
			issues = append(issues, SessionStateIssue{
				SessionID: sessionID, Path: entryPath, Kind: SessionStateIssueStaging,
				Err: errors.New("initial session publication did not complete"),
			})
			continue
		}
		statePath := filepath.Join(entryPath, "state.json")
		stateInfo, stateInfoErr := storeRoot.Lstat(filepath.Join(entryRelative, "state.json"))
		if errors.Is(stateInfoErr, os.ErrNotExist) {
			issues = append(issues, SessionStateIssue{
				SessionID: entry.Name(), Path: statePath, Kind: SessionStateIssueMissingState,
				Err: fmt.Errorf("load managed session %s: %w", entry.Name(), stateInfoErr),
			})
			continue
		}
		if stateInfoErr != nil {
			issues = append(issues, SessionStateIssue{
				SessionID: entry.Name(), Path: statePath, Kind: SessionStateIssueInvalidState,
				Err: fmt.Errorf("inspect managed session state %s: %w", entry.Name(), stateInfoErr),
			})
			continue
		}
		if stateInfo.Mode()&os.ModeSymlink != 0 || !stateInfo.Mode().IsRegular() {
			issues = append(issues, SessionStateIssue{
				SessionID: entry.Name(), Path: statePath, Kind: SessionStateIssueInvalidState,
				Err: errors.New("managed session state is not a regular file"),
			})
			continue
		}
		state, err := load(statePath)
		if err != nil {
			kind := SessionStateIssueInvalidState
			if errors.Is(err, os.ErrNotExist) {
				kind = SessionStateIssueMissingState
			}
			issues = append(issues, SessionStateIssue{
				SessionID: entry.Name(), Path: statePath, Kind: kind,
				Err: fmt.Errorf("load managed session %s: %w", entry.Name(), err),
			})
			continue
		}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].SessionID < states[j].SessionID })
	sort.Slice(issues, func(i, j int) bool { return issues[i].Path < issues[j].Path })
	return states, issues, nil
}

func writeSessionState(path string, state SessionState) error {
	data, err := encodeSessionState(state)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".state-primary-*.tmp")
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
	info, err := os.Lstat(file.Path)
	if err != nil {
		return fmt.Errorf("inspect native snapshot: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("native snapshot is not a regular file")
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

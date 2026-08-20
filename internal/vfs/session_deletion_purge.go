package vfs

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	sessionDeletionPurgeVersion = 3
	sessionDeletionPurgeKind    = "managed-session-deletion-purge"

	sessionDeletionObjectProofDarwin = "darwin-stat-generation-birthtime-v1"
	sessionDeletionObjectProofLinux  = "linux-statx-btime-fs-ioc-getversion-v1"

	sessionDeletionGenerationSourceDarwin = "stat.st_gen"
	sessionDeletionBirthtimeSourceDarwin  = "stat.st_birthtimespec"
	sessionDeletionGenerationSourceLinux  = "FS_IOC_GETVERSION"
	sessionDeletionBirthtimeSourceLinux   = "statx.AT_EMPTY_PATH.STATX_BTIME"

	sessionDeletionPurgePrepared = "prepared"
	sessionDeletionPurgeRenamed  = "renamed"
	sessionDeletionPurgeComplete = "complete"
)

type SessionDeletionPurge struct {
	Version              int                         `json:"version"`
	Kind                 string                      `json:"kind"`
	SessionID            string                      `json:"session_id"`
	OperationToken       string                      `json:"operation_token"`
	TombstoneSHA256      string                      `json:"tombstone_sha256"`
	QuarantineTreeSHA256 string                      `json:"quarantine_tree_sha256"`
	QuarantineBytes      int64                       `json:"quarantine_bytes"`
	QuarantineEntries    int                         `json:"quarantine_entries"`
	QuarantineRootMode   uint32                      `json:"quarantine_root_mode"`
	QuarantineRootObject SessionDeletionPurgeObject  `json:"quarantine_root_object"`
	QuarantineRootXattrs SessionDeletionPurgeXattrs  `json:"quarantine_root_xattrs"`
	Entries              []SessionDeletionPurgeEntry `json:"entries"`
	StagedEntries        []string                    `json:"staged_entries,omitempty"`
	RemovedEntries       []string                    `json:"removed_entries,omitempty"`
	StagingPath          string                      `json:"staging_path"`
	Phase                string                      `json:"phase"`
	PreparedAt           string                      `json:"prepared_at"`
	CompletedAt          string                      `json:"completed_at,omitempty"`
}

type SessionDeletionPurgeObject struct {
	IdentityProof     string `json:"identity_proof"`
	GenerationSource  string `json:"generation_source"`
	BirthtimeSource   string `json:"birthtime_source"`
	Device            uint64 `json:"device"`
	Inode             uint64 `json:"inode"`
	Generation        uint64 `json:"generation"`
	BirthtimeUnixNano int64  `json:"birthtime_unix_nano"`
	UID               uint32 `json:"uid"`
	GID               uint32 `json:"gid"`
}

type SessionDeletionPurgeXattrs struct {
	Count  int    `json:"count"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type SessionDeletionPurgeEntry struct {
	Path   string                     `json:"path"`
	Kind   string                     `json:"kind"`
	Mode   uint32                     `json:"mode"`
	Object SessionDeletionPurgeObject `json:"object"`
	Xattrs SessionDeletionPurgeXattrs `json:"xattrs"`
	Bytes  int64                      `json:"bytes,omitempty"`
	SHA256 string                     `json:"sha256,omitempty"`
}

type deletionPurgeTreeIdentity struct {
	SHA256     string
	Bytes      int64
	Entries    int
	RootMode   uint32
	RootObject SessionDeletionPurgeObject
	RootXattrs SessionDeletionPurgeXattrs
	Tree       []SessionDeletionPurgeEntry
}

var sessionDeletionPurgeHook func(string) error

func SessionDeletionPurgePath(root string, sessionID string) string {
	return filepath.Join(filepath.Clean(root), "fs", "deletion-purges", sessionID+".json")
}

func LoadSessionDeletionPurge(root string, sessionID string) (SessionDeletionPurge, error) {
	if root == "" || !safeSessionID(sessionID) {
		return SessionDeletionPurge{}, errors.New("store root and safe session ID are required")
	}
	root = filepath.Clean(root)
	storeRoot, err := openSessionDeletionStoreRoot(root)
	if err != nil {
		return SessionDeletionPurge{}, err
	}
	defer storeRoot.Close()
	data, _, err := storeRoot.readStableRegularFile(SessionDeletionPurgePath(root, sessionID), maximumSessionDeletionBytes)
	if err != nil {
		return SessionDeletionPurge{}, err
	}
	var receipt SessionDeletionPurge
	if err := decodeStrictJSON(data, &receipt); err != nil {
		return SessionDeletionPurge{}, fmt.Errorf("decode session deletion purge receipt: %w", err)
	}
	if err := validateSessionDeletionPurge(root, receipt); err != nil {
		return SessionDeletionPurge{}, err
	}
	return receipt, nil
}

func purgeSessionDeletionLocked(root string, tombstone SessionDeletion) (bool, error) {
	if purged, err := resumeSessionDeletionPurgeLocked(root, tombstone); purged || err != nil {
		return purged, err
	}
	quarantineRoot := filepath.Dir(tombstone.RetiredSessionPath)
	identityBefore, err := captureDeletionPurgeTreeIdentity(root, quarantineRoot)
	if err != nil {
		return false, fmt.Errorf("capture deleted session quarantine proof: %w", err)
	}
	if err := validateDeletionPurgeContents(tombstone); err != nil {
		return false, err
	}
	if err := runSessionDeletionPurgeHook("semantic-validated"); err != nil {
		return false, err
	}
	identity, err := captureDeletionPurgeTreeIdentity(root, quarantineRoot)
	if err != nil {
		return false, fmt.Errorf("recapture deleted session quarantine proof: %w", err)
	}
	if !sameDeletionPurgeTreeIdentity(identityBefore, identity) {
		return false, errors.New("deleted session quarantine changed across semantic ownership validation")
	}
	tombstoneIdentity, err := captureStableDeletionFileWithinStore(root, SessionDeletionPath(root, tombstone.SessionID))
	if err != nil {
		return false, fmt.Errorf("hash managed session deletion tombstone before purge: %w", err)
	}
	tombstoneSHA256 := tombstoneIdentity.SHA256
	receipt := SessionDeletionPurge{
		Version: sessionDeletionPurgeVersion, Kind: sessionDeletionPurgeKind,
		SessionID: tombstone.SessionID, OperationToken: tombstone.OperationToken,
		TombstoneSHA256: tombstoneSHA256, QuarantineTreeSHA256: identity.SHA256,
		QuarantineBytes: identity.Bytes, QuarantineEntries: identity.Entries,
		QuarantineRootMode: identity.RootMode, QuarantineRootObject: identity.RootObject,
		QuarantineRootXattrs: identity.RootXattrs,
		Entries:              identity.Tree,
		StagingPath:          deletionPurgeStagingPath(root, tombstone),
		Phase:                sessionDeletionPurgePrepared, PreparedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeSessionDeletionPurge(root, receipt); err != nil {
		return false, err
	}
	if err := runSessionDeletionPurgeHook(sessionDeletionPurgePrepared); err != nil {
		return false, err
	}
	return advanceSessionDeletionPurgeLocked(root, tombstone, receipt)
}

func resumeSessionDeletionPurgeLocked(root string, tombstone SessionDeletion) (bool, error) {
	if tombstone.Version < sessionDeletionVersion {
		return false, errors.New("legacy session deletion cannot resume physical purge")
	}
	receipt, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if receipt.OperationToken != tombstone.OperationToken {
		return false, errors.New("session deletion purge receipt belongs to another deletion operation")
	}
	tombstoneIdentity, err := captureStableDeletionFileWithinStore(root, SessionDeletionPath(root, tombstone.SessionID))
	if err != nil {
		return false, fmt.Errorf("hash managed session deletion tombstone during purge replay: %w", err)
	}
	tombstoneSHA256 := tombstoneIdentity.SHA256
	if tombstoneSHA256 != receipt.TombstoneSHA256 {
		return false, errors.New("session deletion tombstone changed after purge publication")
	}
	return advanceSessionDeletionPurgeLocked(root, tombstone, receipt)
}

func advanceSessionDeletionPurgeLocked(root string, tombstone SessionDeletion, receipt SessionDeletionPurge) (bool, error) {
	if tombstone.Version < sessionDeletionVersion {
		return false, errors.New("legacy session deletion cannot advance physical purge")
	}
	tombstoneIdentity, err := captureStableDeletionFileWithinStore(root, SessionDeletionPath(root, tombstone.SessionID))
	if err != nil {
		return false, fmt.Errorf("hash managed session deletion tombstone before physical purge: %w", err)
	}
	tombstoneSHA256 := tombstoneIdentity.SHA256
	if tombstoneSHA256 != receipt.TombstoneSHA256 {
		return false, errors.New("session deletion tombstone changed before physical purge")
	}
	quarantineRoot := filepath.Dir(tombstone.RetiredSessionPath)
	stagingRoot := receipt.StagingPath
	quarantineExists, err := plainDeletionPurgeDirectoryExists(root, quarantineRoot)
	if err != nil {
		return false, err
	}
	stagingExists, err := plainDeletionPurgeDirectoryExists(root, stagingRoot)
	if err != nil {
		return false, err
	}

	switch receipt.Phase {
	case sessionDeletionPurgePrepared:
		if quarantineExists == stagingExists {
			if quarantineExists {
				return false, errors.New("session deletion quarantine and purge staging both exist")
			}
			return false, errors.New("prepared session deletion purge lost both quarantine and staging evidence")
		}
		proofRoot := quarantineRoot
		if stagingExists {
			proofRoot = stagingRoot
		}
		if err := verifyDeletionPurgeTreeIdentity(root, proofRoot, receipt); err != nil {
			return false, err
		}
		if quarantineExists {
			if err := prepareDeletionPurgeStaging(root, stagingRoot); err != nil {
				return false, err
			}
			storeRoot, err := openSessionDeletionStoreRoot(root)
			if err != nil {
				return false, err
			}
			renameErr := storeRoot.rename(quarantineRoot, stagingRoot, true)
			closeErr := storeRoot.Close()
			if err := errors.Join(renameErr, closeErr); err != nil {
				return false, fmt.Errorf("move deleted session quarantine into purge staging: %w", err)
			}
			if err := verifyDeletionPurgeTreeIdentity(root, stagingRoot, receipt); err != nil {
				return false, err
			}
		}
		receipt.Phase = sessionDeletionPurgeRenamed
		if err := replaceSessionDeletionPurge(root, receipt); err != nil {
			return false, err
		}
		if err := runSessionDeletionPurgeHook(sessionDeletionPurgeRenamed); err != nil {
			return false, err
		}
		stagingExists = true
		quarantineExists = false
	case sessionDeletionPurgeRenamed:
		if quarantineExists {
			return false, errors.New("deleted session quarantine reappeared after purge staging")
		}
	case sessionDeletionPurgeComplete:
		if quarantineExists || stagingExists {
			return false, errors.New("completed session deletion purge still has data")
		}
		return true, nil
	default:
		return false, errors.New("session deletion purge has an unknown phase")
	}

	if stagingExists {
		updated, err := removeDeletionPurgeStaging(root, stagingRoot, receipt)
		if err != nil {
			return false, fmt.Errorf("remove exactly proven deleted session staging: %w", err)
		}
		receipt = updated
	}
	if err := runSessionDeletionPurgeHook("removed"); err != nil {
		return false, err
	}
	receipt.Phase = sessionDeletionPurgeComplete
	receipt.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := replaceSessionDeletionPurge(root, receipt); err != nil {
		return false, err
	}
	return true, nil
}

func validateDeletionPurgeContents(tombstone SessionDeletion) error {
	if tombstone.Version < sessionDeletionVersion {
		return errors.New("legacy session deletion lacks an exact initial checkpoint and cannot be physically purged")
	}
	if err := verifyDeletionState(tombstone.RetiredSessionPath, tombstone); err != nil {
		return err
	}
	storeRoot := filepath.Dir(filepath.Dir(filepath.Dir(tombstone.SessionPath)))
	manifestIdentity, err := captureStableDeletionFileWithinStore(storeRoot, tombstone.RetiredManifestPath)
	if err != nil || manifestIdentity.SHA256 != tombstone.ManifestSHA256 {
		if err == nil {
			err = errors.New("quarantined deletion manifest identity differs")
		}
		return err
	}
	state, _, err := readSessionState(filepath.Join(tombstone.RetiredSessionPath, "state.json"))
	if err != nil {
		return err
	}
	chain, err := verifyDeletionCheckpointDescendant(tombstone.RetiredSessionPath, tombstone, state, tombstone.StateSHA256, false)
	if err != nil {
		return err
	}
	if err := validateRelocatedDeletionCheckpointArtifacts(tombstone, state, chain); err != nil {
		return err
	}
	if err := validateDeletionRetirementControlPair(tombstone.RetiredSessionPath, tombstone, chain); err != nil {
		return err
	}
	knownFiles := map[string]func(string) error{
		"state.json":         func(string) error { return nil },
		stateCatalogFilename: func(string) error { return nil },
		"writer.lease":       validateDeletionEmptyRegularFile,
		"retire.request.json": func(path string) error {
			return errors.Join(validateDeletionBoundMetadata(path, tombstone.RetirementRequest), validateDeletionRetirementControl(path, tombstone, chain))
		},
		"retire.ack.json": func(path string) error {
			return errors.Join(validateDeletionBoundMetadata(path, tombstone.RetirementAcknowledgement), validateDeletionRetirementControl(path, tombstone, chain))
		},
		"mounted.json": func(path string) error {
			return errors.Join(validateDeletionBoundMetadata(path, tombstone.MountAcknowledgement), validateDeletionMountAcknowledgement(path, tombstone, chain))
		},
		"journal.jsonl": func(path string) error {
			return errors.Join(validateDeletionBoundMetadata(path, tombstone.Journal), validateDeletionTerminalJournal(tombstone.RetiredSessionPath, tombstone.SessionID))
		},
		NativeRetirementFilename: func(path string) error {
			return errors.Join(validateDeletionBoundMetadata(path, tombstone.NativeRetirementProof), validateDeletionNativeRetirementProof(path, tombstone, chain))
		},
	}
	for _, dataPath := range []string{state.DeltaPath, state.BackingPath} {
		if dataPath == "" {
			continue
		}
		if filepath.Dir(dataPath) != tombstone.SessionPath {
			return errors.New("nested managed session data prevents deletion purge")
		}
		knownFiles[filepath.Base(dataPath)] = func(string) error { return nil }
	}
	knownDirectories := map[string]struct{}{
		stateGenerationsDirectoryName: {}, "state-orphans": {}, "leases": {}, "retained-native": {},
	}
	entries, err := os.ReadDir(tombstone.RetiredSessionPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(tombstone.RetiredSessionPath, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if validate, known := knownFiles[entry.Name()]; known {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("known deleted session file has an unsafe type: %s", entry.Name())
			}
			if err := validate(path); err != nil {
				return fmt.Errorf("validate deleted session metadata %s: %w", entry.Name(), err)
			}
			continue
		}
		if _, known := knownDirectories[entry.Name()]; known {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("known deleted session directory has an unsafe type: %s", entry.Name())
			}
			if err := validateDeletionPurgeOwnedDirectory(tombstone, chain, entry.Name(), path); err != nil {
				return err
			}
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			if info.IsDir() {
				nonempty, err := deletionPurgeTreeContainsContent(path)
				if err != nil {
					return err
				}
				if !nonempty {
					continue
				}
			}
			return fmt.Errorf("unknown deleted session content has an unsafe type: %s", entry.Name())
		}
		if info.Size() != 0 {
			return fmt.Errorf("unknown nonempty content prevents deleted session purge: %s", entry.Name())
		}
	}
	return nil
}

func validateRelocatedDeletionCheckpointArtifacts(tombstone SessionDeletion, state SessionState, chain stateCheckpointChain) error {
	storeRoot := filepath.Dir(filepath.Dir(filepath.Dir(tombstone.SessionPath)))
	manifest, err := captureStableDeletionFileWithinStore(storeRoot, tombstone.RetiredManifestPath)
	if err != nil {
		return err
	}
	if chain.checkpoint.Manifest.Path != tombstone.ManifestPath || manifest.Bytes != chain.checkpoint.Manifest.Bytes || manifest.SHA256 != chain.checkpoint.Manifest.SHA256 {
		return errors.New("quarantined manifest differs from the active checkpoint")
	}
	expected := make(map[string][]sessionStateFileIdentity)
	for _, checkpoint := range chain.lineage {
		for _, identity := range []sessionStateFileIdentity{checkpoint.Delta} {
			relocated, err := relocateDeletionSessionArtifact(tombstone, identity.Path)
			if err != nil {
				return err
			}
			expected[relocated] = append(expected[relocated], identity)
		}
		if checkpoint.Backing != nil {
			relocated, err := relocateDeletionSessionArtifact(tombstone, checkpoint.Backing.Path)
			if err != nil {
				return err
			}
			expected[relocated] = append(expected[relocated], *checkpoint.Backing)
		}
	}
	for _, required := range []sessionStateFileIdentity{chain.checkpoint.Delta} {
		relocated, err := relocateDeletionSessionArtifact(tombstone, required.Path)
		if err != nil {
			return err
		}
		if err := verifyRelocatedDeletionFile(storeRoot, relocated, required); err != nil {
			return fmt.Errorf("verify active deletion delta: %w", err)
		}
	}
	if chain.checkpoint.Backing != nil {
		relocated, err := relocateDeletionSessionArtifact(tombstone, chain.checkpoint.Backing.Path)
		if err != nil {
			return err
		}
		if err := verifyRelocatedDeletionFile(storeRoot, relocated, *chain.checkpoint.Backing); err != nil {
			return fmt.Errorf("verify active deletion backing: %w", err)
		}
	}
	entries, err := os.ReadDir(tombstone.RetiredSessionPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !managedSessionDataName(entry.Name()) {
			continue
		}
		path := filepath.Join(tombstone.RetiredSessionPath, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("managed deletion data has an unsafe type: %s", entry.Name())
		}
		if info.Size() == 0 {
			continue
		}
		identities := expected[path]
		if len(identities) == 0 {
			return fmt.Errorf("managed deletion data is absent from checkpoint lineage: %s", entry.Name())
		}
		actual, err := captureStableDeletionFileWithinStore(storeRoot, path)
		if err != nil {
			return err
		}
		matched := false
		for _, identity := range identities {
			if actual.Bytes == identity.Bytes && actual.SHA256 == identity.SHA256 {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("managed deletion data differs from checkpoint lineage: %s", entry.Name())
		}
	}
	if state != chain.checkpoint.State {
		return errors.New("deletion state differs from its active checkpoint")
	}
	return nil
}

func relocateDeletionSessionArtifact(tombstone SessionDeletion, original string) (string, error) {
	original = filepath.Clean(original)
	relative, err := filepath.Rel(tombstone.SessionPath, original)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("checkpoint artifact escapes the deleted session directory")
	}
	return filepath.Join(tombstone.RetiredSessionPath, relative), nil
}

func verifyRelocatedDeletionFile(storeRoot string, relocated string, expected sessionStateFileIdentity) error {
	actual, err := captureStableDeletionFileWithinStore(storeRoot, relocated)
	if err != nil {
		return err
	}
	if actual.Bytes != expected.Bytes || actual.SHA256 != expected.SHA256 {
		return errors.New("relocated deletion artifact differs from its checkpoint")
	}
	return nil
}

func validateDeletionEmptyRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != 0 {
		return errors.New("deletion writer lease is not an empty regular file")
	}
	return nil
}

func validateDeletionBoundMetadata(path string, expected NativeFile) error {
	storeRoot := deletionStoreRootFromRetiredMetadataPath(path)
	store, err := openSessionDeletionStoreRoot(storeRoot)
	if err != nil {
		return err
	}
	defer store.Close()
	info, err := store.lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("deletion protocol metadata is not a regular file")
	}
	if expected == (NativeFile{}) {
		if info.Size() == 0 {
			return nil
		}
		return errors.New("nonempty deletion protocol metadata is absent from the durable tombstone")
	}
	identity, err := store.captureStableRegularFile(path)
	if err != nil {
		return err
	}
	if identity.Bytes != expected.Bytes || identity.SHA256 != expected.SHA256 {
		return errors.New("deletion protocol metadata differs from its durable tombstone identity")
	}
	return nil
}

func deletionStoreRootFromRetiredMetadataPath(path string) string {
	// <store>/fs/deleted/<session>/<token>/session/<metadata>
	current := filepath.Clean(path)
	for range 6 {
		current = filepath.Dir(current)
	}
	return current
}

type deletionMountAcknowledgement struct {
	Generation uint64 `json:"generation"`
	Route      string `json:"route"`
}

func validateDeletionMountAcknowledgement(path string, tombstone SessionDeletion, chain stateCheckpointChain) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var acknowledgement deletionMountAcknowledgement
	if err := decodeStrictJSON(data, &acknowledgement); err != nil {
		return err
	}
	if acknowledgement.Route != tombstone.Route {
		return errors.New("mount acknowledgement does not match the deleted session route")
	}
	for _, checkpoint := range chain.lineage {
		if checkpoint.State.Generation == acknowledgement.Generation {
			return nil
		}
	}
	return errors.New("mount acknowledgement generation is absent from the deleted session checkpoint lineage")
}

type deletionRetirementControl struct {
	Token              string `json:"token"`
	Generation         uint64 `json:"generation"`
	StateSHA256        string `json:"state_sha256"`
	CheckpointSequence uint64 `json:"checkpoint_sequence"`
	CheckpointSHA256   string `json:"checkpoint_sha256"`
	Route              string `json:"route"`
	Bytes              int64  `json:"bytes"`
	SHA256             string `json:"sha256"`
	Error              string `json:"error,omitempty"`
}

func validateDeletionRetirementControl(path string, tombstone SessionDeletion, chain stateCheckpointChain) error {
	control, err := readDeletionRetirementControl(path)
	if err != nil {
		return err
	}
	token, tokenErr := hex.DecodeString(control.Token)
	checkpoint, exists := chain.lineage[control.CheckpointSHA256]
	if tokenErr != nil || len(token) != 16 || control.Generation == 0 || !validStateSHA256(control.StateSHA256) || control.CheckpointSequence == 0 || !validStateSHA256(control.CheckpointSHA256) || control.Route != tombstone.Route || control.Bytes < 0 || !validStateSHA256(control.SHA256) || !exists || checkpoint.State.Generation != control.Generation || checkpoint.Sequence != control.CheckpointSequence || checkpoint.StateSHA256 != control.StateSHA256 {
		return errors.New("retirement control is not bound to the deleted session checkpoint lineage")
	}
	if filepath.Base(path) == "retire.request.json" && control.Error != "" {
		return errors.New("retirement request contains an acknowledgement error")
	}
	return nil
}

func readDeletionRetirementControl(path string) (deletionRetirementControl, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return deletionRetirementControl{}, err
	}
	var control deletionRetirementControl
	if err := decodeStrictJSON(data, &control); err != nil {
		return deletionRetirementControl{}, err
	}
	return control, nil
}

func validateDeletionRetirementControlPair(directory string, tombstone SessionDeletion, chain stateCheckpointChain) error {
	requestPath := filepath.Join(directory, "retire.request.json")
	acknowledgementPath := filepath.Join(directory, "retire.ack.json")
	request, requestErr := readDeletionRetirementControl(requestPath)
	acknowledgement, acknowledgementErr := readDeletionRetirementControl(acknowledgementPath)
	requestExists := requestErr == nil
	acknowledgementExists := acknowledgementErr == nil
	if requestErr != nil && !errors.Is(requestErr, os.ErrNotExist) {
		return requestErr
	}
	if acknowledgementErr != nil && !errors.Is(acknowledgementErr, os.ErrNotExist) {
		return acknowledgementErr
	}
	if requestExists {
		if err := validateDeletionRetirementControl(requestPath, tombstone, chain); err != nil {
			return err
		}
	}
	if acknowledgementExists {
		if err := validateDeletionRetirementControl(acknowledgementPath, tombstone, chain); err != nil {
			return err
		}
	}
	if requestExists && acknowledgementExists {
		acknowledgement.Error = ""
		if acknowledgement != request {
			return errors.New("retirement request and acknowledgement do not describe the same deletion transaction")
		}
	}
	return nil
}

func validateDeletionNativeRetirementProof(path string, tombstone SessionDeletion, chain stateCheckpointChain) error {
	proof, err := LoadNativeRetirementProof(path)
	if err != nil {
		return err
	}
	storeRoot := filepath.Dir(filepath.Dir(filepath.Dir(tombstone.SessionPath)))
	expectedSnapshot := filepath.Join(storeRoot, "fs", "snapshots", tombstone.SessionID, "native.jsonl")
	if proof.SessionID != tombstone.SessionID || proof.StateGeneration > tombstone.StateGeneration || filepath.Clean(proof.Snapshot.Path) != expectedSnapshot {
		return errors.New("native retirement proof is not bound to the deleted session")
	}
	matched := false
	for _, checkpoint := range chain.lineage {
		if checkpoint.State.Generation == proof.StateGeneration && checkpoint.NativeSnapshot != nil && checkpoint.NativeSnapshot.Path == proof.Snapshot.Path && checkpoint.NativeSnapshot.Bytes == proof.Snapshot.Bytes && checkpoint.NativeSnapshot.SHA256 == proof.Snapshot.SHA256 {
			matched = true
			break
		}
	}
	if !matched {
		return errors.New("native retirement proof snapshot is absent from checkpoint lineage")
	}
	if tombstone.NativeSnapshotSidecar == (NativeFile{}) {
		if proof.Sidecar != nil {
			return errors.New("native retirement proof binds a sidecar absent from the deletion tombstone")
		}
	} else if proof.Sidecar == nil || *proof.Sidecar != tombstone.NativeSnapshotSidecar {
		return errors.New("native retirement sidecar proof differs from the deletion tombstone")
	}
	return nil
}

func validateDeletionTerminalJournal(directory string, sessionID string) error {
	path := filepath.Join(directory, "journal.jsonl")
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	state, _, err := readSessionState(filepath.Join(directory, "state.json"))
	if err != nil {
		return err
	}
	originalDirectory := filepath.Dir(state.DeltaPath)
	chain, err := loadStateCheckpointChain(directory, sessionID, true)
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	latest := make(map[string]JournalRecord)
	for scanner.Scan() {
		var record JournalRecord
		if err := decodeStrictJSON(scanner.Bytes(), &record); err != nil {
			return fmt.Errorf("decode deletion journal: %w", err)
		}
		if record.OperationID == "" || record.SessionID != sessionID {
			return errors.New("deletion journal record belongs to another operation or session")
		}
		if _, err := time.Parse(time.RFC3339Nano, record.At); err != nil {
			return errors.New("deletion journal record has an invalid timestamp")
		}
		if record.Candidate.Version == legacySessionStateVersion && record.Candidate.ManifestSHA256 == "" {
			upgraded, ok := deletionJournalLegacyCandidateFromLineage(chain, record.Candidate)
			if !ok {
				return errors.New("legacy deletion journal candidate is absent from checkpoint lineage")
			}
			record.Candidate = upgraded
		}
		switch record.Kind {
		case "copy-on-write":
			operationGeneration, valid := cowOperationGeneration(record.OperationID)
			if !valid || operationGeneration == ^uint64(0) {
				return errors.New("deletion journal has an invalid copy-on-write operation identity")
			}
			switch record.Phase {
			case "after-file-publish", "state-published", "complete":
				expectedBacking := filepath.Join(originalDirectory, fmt.Sprintf("backing-%020d.jsonl", operationGeneration+1))
				if record.Candidate == (SessionState{}) || record.Candidate.Generation != operationGeneration+1 || filepath.Clean(record.Candidate.BackingPath) != expectedBacking || filepath.Clean(record.FinalPath) != expectedBacking || !validCOWTemporaryPath(originalDirectory, record.Native.Path) || record.Native.Bytes < 0 || !validStateSHA256(record.Native.SHA256) {
					return errors.New("published deletion copy-on-write journal is not a valid protocol record")
				}
				if err := validateSessionStateForDirectory(record.Candidate, originalDirectory); err != nil {
					return errors.New("published deletion copy-on-write candidate is unsafe")
				}
			case "data-synced", "rolled-back":
				if record.Candidate != (SessionState{}) || !validCOWTemporaryPath(originalDirectory, record.TempPath) || filepath.Clean(record.Native.Path) != filepath.Clean(record.TempPath) || record.FinalPath != "" || record.Native.Bytes < 0 || !validStateSHA256(record.Native.SHA256) {
					return errors.New("prepared deletion copy-on-write journal is not a valid protocol record")
				}
			default:
				return errors.New("deletion copy-on-write journal has an unknown phase")
			}
		case "compact":
			session := &Session{directory: originalDirectory}
			if err := session.validateCompactJournalRecord(record); err != nil {
				return err
			}
			switch record.Phase {
			case "prepared", "state-publishing", "state-published", "complete", "rolled-back":
			default:
				return errors.New("deletion compact journal has an unknown phase")
			}
		default:
			return errors.New("deletion journal record has an unknown kind")
		}
		latest[record.OperationID] = record
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	for operationID, record := range latest {
		if record.Phase != "complete" && record.Phase != "rolled-back" {
			return fmt.Errorf("deletion journal operation %s is not terminal", operationID)
		}
		if record.Phase == "complete" && !deletionJournalCandidateInLineage(chain, record.Candidate) {
			return fmt.Errorf("completed deletion journal operation %s is absent from checkpoint lineage", operationID)
		}
	}
	return nil
}

func deletionJournalCandidateInLineage(chain stateCheckpointChain, candidate SessionState) bool {
	for _, checkpoint := range chain.lineage {
		if checkpoint.State == candidate {
			return true
		}
	}
	return false
}

func deletionJournalLegacyCandidateFromLineage(chain stateCheckpointChain, legacy SessionState) (SessionState, bool) {
	for _, checkpoint := range chain.lineage {
		candidate := checkpoint.State
		if candidate.Generation == legacy.Generation && candidate.SessionID == legacy.SessionID && candidate.ManifestPath == legacy.ManifestPath && candidate.BaseBytes == legacy.BaseBytes && candidate.BaseSHA256 == legacy.BaseSHA256 && candidate.DeltaPath == legacy.DeltaPath && candidate.BackingPath == legacy.BackingPath && candidate.NativeSnapshot == legacy.NativeSnapshot {
			return candidate, true
		}
	}
	return SessionState{}, false
}

// A recognized top-level directory name is not ownership proof for every
// byte below it. Only checkpoint files with a strict self-authenticating
// identity are removable as nonempty metadata. Lease trees must be empty of
// bytes, and retained-native accepts only the canonical snapshot whose exact
// byte identity is still named by state; all other content remains ambiguous.
func validateDeletionPurgeOwnedDirectory(tombstone SessionDeletion, chain stateCheckpointChain, name string, root string) error {
	switch name {
	case stateGenerationsDirectoryName, "state-orphans":
		return validateDeletionPurgeCheckpointDirectory(tombstone, chain, root)
	case "leases":
		return validateDeletionPurgeEmptyDirectoryTree(root)
	case "retained-native":
		return validateDeletionPurgeRetainedNativeTree(tombstone, chain, root)
	default:
		return fmt.Errorf("unknown deleted session directory cannot be proved: %s", name)
	}
}

func validateDeletionPurgeCheckpointDirectory(tombstone SessionDeletion, chain stateCheckpointChain, root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink prevents deleted session purge: %s", path)
		}
		if info.IsDir() {
			if err := validateDeletionPurgeEmptyDirectoryTree(path); err != nil {
				return fmt.Errorf("unknown nonempty checkpoint directory prevents deleted session purge: %s: %w", entry.Name(), err)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("special file prevents deleted session purge: %s", path)
		}
		if info.Size() == 0 {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		checkpoint, err := decodeStateCheckpoint(data)
		digest := digestStateBytes(data)
		if err == nil {
			err = validateSessionStateForDirectory(checkpoint.State, tombstone.SessionPath)
		}
		_, belongsToLineage := chain.lineage[digest]
		if err != nil || checkpoint.SessionID != tombstone.SessionID || entry.Name() != stateCheckpointFilename(checkpoint.State.Generation, checkpoint.Sequence, digest) || !belongsToLineage {
			return fmt.Errorf("unknown nonempty checkpoint content prevents deleted session purge: %s", entry.Name())
		}
	}
	return nil
}

func validateDeletionPurgeEmptyDirectoryTree(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink prevents deleted session purge: %s", path)
		}
		if info.IsDir() {
			if err := validateDeletionPurgeEmptyDirectoryTree(path); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("special file prevents deleted session purge: %s", path)
		}
		if info.Size() != 0 {
			return fmt.Errorf("unknown nonempty content prevents deleted session purge: %s", path)
		}
	}
	return nil
}

func validateDeletionPurgeRetainedNativeTree(tombstone SessionDeletion, chain stateCheckpointChain, root string) error {
	storeRoot := filepath.Dir(filepath.Dir(filepath.Dir(tombstone.SessionPath)))
	canonicalSnapshot := filepath.Join(storeRoot, "fs", "snapshots", tombstone.SessionID, "native.jsonl")
	expectedSnapshots := make([]NativeFile, 0, len(chain.lineage))
	for _, checkpoint := range chain.lineage {
		if checkpoint.NativeSnapshot != nil && filepath.Clean(checkpoint.NativeSnapshot.Path) == canonicalSnapshot {
			expectedSnapshots = append(expectedSnapshots, NativeFile{Path: checkpoint.NativeSnapshot.Path, Bytes: checkpoint.NativeSnapshot.Bytes, SHA256: checkpoint.NativeSnapshot.SHA256})
		}
	}
	expected := filepath.Join(root, "store-snapshot", "native.jsonl")
	expectedSidecar := filepath.Join(root, "store-snapshot", "._native.jsonl")
	return filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink prevents deleted session purge: %s", path)
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("special file prevents deleted session purge: %s", path)
		}
		if info.Size() == 0 {
			return nil
		}
		if filepath.Clean(path) == expectedSidecar {
			if tombstone.NativeSnapshotSidecar == (NativeFile{}) {
				return fmt.Errorf("nonempty retained native sidecar has no exact deletion proof: %s", path)
			}
			identity, err := captureStableDeletionFileWithinStore(storeRoot, path)
			if err != nil {
				return err
			}
			if identity.Bytes != tombstone.NativeSnapshotSidecar.Bytes || identity.SHA256 != tombstone.NativeSnapshotSidecar.SHA256 {
				return errors.New("retained native sidecar differs from its deletion tombstone")
			}
			return nil
		}
		if filepath.Clean(path) != expected {
			return fmt.Errorf("unknown nonempty retained native content prevents deleted session purge: %s", path)
		}
		identity, err := captureStableDeletionFileWithinStore(storeRoot, path)
		if err != nil {
			return err
		}
		matched := false
		for _, expectedIdentity := range expectedSnapshots {
			if identity.Bytes == expectedIdentity.Bytes && identity.SHA256 == expectedIdentity.SHA256 {
				matched = true
				break
			}
		}
		if !matched {
			return errors.New("retained native snapshot differs from the exact session state identity")
		}
		return nil
	})
}

func deletionPurgeTreeContainsContent(root string) (bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return true, nil
		}
		nonempty, err := deletionPurgeTreeContainsContent(path)
		if err != nil || nonempty {
			return nonempty, err
		}
	}
	return false, nil
}

func captureDeletionPurgeTreeIdentity(storeRoot string, root string) (deletionPurgeTreeIdentity, error) {
	first, err := captureDeletionPurgeTreeIdentityOnce(storeRoot, root)
	if err != nil {
		return deletionPurgeTreeIdentity{}, err
	}
	second, err := captureDeletionPurgeTreeIdentityOnce(storeRoot, root)
	if err != nil {
		return deletionPurgeTreeIdentity{}, err
	}
	if !sameDeletionPurgeTreeIdentity(first, second) {
		return deletionPurgeTreeIdentity{}, errors.New("deleted session quarantine changed while its complete tree proof was captured")
	}
	return first, nil
}

func sameDeletionPurgeTreeIdentity(left deletionPurgeTreeIdentity, right deletionPurgeTreeIdentity) bool {
	if left.SHA256 != right.SHA256 || left.Bytes != right.Bytes || left.Entries != right.Entries || left.RootMode != right.RootMode || left.RootObject != right.RootObject || left.RootXattrs != right.RootXattrs || len(left.Tree) != len(right.Tree) {
		return false
	}
	for index := range left.Tree {
		if left.Tree[index] != right.Tree[index] {
			return false
		}
	}
	return true
}

func captureDeletionPurgeTreeIdentityOnce(storeRoot string, root string) (deletionPurgeTreeIdentity, error) {
	store, err := openSessionDeletionStoreRoot(storeRoot)
	if err != nil {
		return deletionPurgeTreeIdentity{}, err
	}
	defer store.Close()
	info, rootObject, rootXattrs, err := captureDeletionPurgeObject(store, root)
	if err != nil {
		return deletionPurgeTreeIdentity{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return deletionPurgeTreeIdentity{}, errors.New("deletion purge root is not a plain directory")
	}
	rootRelative, err := store.relative(root)
	if err != nil {
		return deletionPurgeTreeIdentity{}, err
	}
	rootFSPath := filepath.ToSlash(rootRelative)
	var paths []string
	err = fs.WalkDir(store.root.FS(), rootFSPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == rootFSPath {
			return nil
		}
		relative := strings.TrimPrefix(path, rootFSPath+"/")
		if relative == "" || relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
			return errors.New("deletion purge entry escapes its root")
		}
		absolute := filepath.Join(root, filepath.FromSlash(relative))
		info, err := store.lstat(absolute)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink prevents deleted session purge: %s", relative)
		}
		paths = append(paths, filepath.FromSlash(relative))
		return nil
	})
	if err != nil {
		return deletionPurgeTreeIdentity{}, err
	}
	sort.Strings(paths)
	hasher := sha256.New()
	rootMode := uint32(info.Mode().Perm())
	_, _ = io.WriteString(hasher, "R\x00"+strconv.FormatUint(uint64(rootMode), 8)+"\x00"+deletionPurgeObjectHashFields(rootObject)+"\x00"+deletionPurgeXattrHashFields(rootXattrs)+"\n")
	identity := deletionPurgeTreeIdentity{
		Entries: len(paths), RootMode: rootMode, RootObject: rootObject, RootXattrs: rootXattrs,
		Tree: make([]SessionDeletionPurgeEntry, 0, len(paths)),
	}
	for _, relative := range paths {
		path := filepath.Join(root, relative)
		info, object, xattrs, err := captureDeletionPurgeObject(store, path)
		if err != nil {
			return deletionPurgeTreeIdentity{}, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return deletionPurgeTreeIdentity{}, fmt.Errorf("symlink prevents deleted session purge: %s", relative)
		}
		mode := strconv.FormatUint(uint64(info.Mode().Perm()), 8)
		if info.IsDir() {
			_, _ = io.WriteString(hasher, "D\x00"+filepath.ToSlash(relative)+"\x00"+mode+"\x00"+deletionPurgeObjectHashFields(object)+"\x00"+deletionPurgeXattrHashFields(xattrs)+"\n")
			identity.Tree = append(identity.Tree, SessionDeletionPurgeEntry{Path: filepath.ToSlash(relative), Kind: "directory", Mode: uint32(info.Mode().Perm()), Object: object, Xattrs: xattrs})
			continue
		}
		if !info.Mode().IsRegular() {
			return deletionPurgeTreeIdentity{}, fmt.Errorf("special file prevents deleted session purge: %s", relative)
		}
		fileIdentity, err := store.captureStableRegularFile(path)
		if err != nil {
			return deletionPurgeTreeIdentity{}, fmt.Errorf("hash deleted session purge entry %s: %w", relative, err)
		}
		if fileIdentity.Bytes > 0 && identity.Bytes > int64(^uint64(0)>>1)-fileIdentity.Bytes {
			return deletionPurgeTreeIdentity{}, errors.New("deleted session purge byte count overflow")
		}
		identity.Bytes += fileIdentity.Bytes
		infoAfter, objectAfter, xattrsAfter, err := captureDeletionPurgeObject(store, path)
		if err != nil || !os.SameFile(info, infoAfter) || object != objectAfter || xattrs != xattrsAfter {
			if err == nil {
				err = errors.New("deleted session purge entry changed while its exact metadata was captured")
			}
			return deletionPurgeTreeIdentity{}, err
		}
		_, _ = io.WriteString(hasher, "F\x00"+filepath.ToSlash(relative)+"\x00"+mode+"\x00"+deletionPurgeObjectHashFields(object)+"\x00"+deletionPurgeXattrHashFields(xattrs)+"\x00"+strconv.FormatInt(fileIdentity.Bytes, 10)+"\x00"+fileIdentity.SHA256+"\n")
		identity.Tree = append(identity.Tree, SessionDeletionPurgeEntry{Path: filepath.ToSlash(relative), Kind: "file", Mode: uint32(info.Mode().Perm()), Object: object, Xattrs: xattrs, Bytes: fileIdentity.Bytes, SHA256: fileIdentity.SHA256})
	}
	identity.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	return identity, nil
}

func captureDeletionPurgeObject(store *sessionDeletionStoreRoot, path string) (os.FileInfo, SessionDeletionPurgeObject, SessionDeletionPurgeXattrs, error) {
	before, err := store.lstat(path)
	if err != nil {
		return nil, SessionDeletionPurgeObject{}, SessionDeletionPurgeXattrs{}, err
	}
	if before.Mode()&os.ModeSymlink != 0 || before.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return nil, SessionDeletionPurgeObject{}, SessionDeletionPurgeXattrs{}, fmt.Errorf("unsafe mode prevents exact session deletion purge: %s", path)
	}
	if !before.IsDir() && !before.Mode().IsRegular() {
		return nil, SessionDeletionPurgeObject{}, SessionDeletionPurgeXattrs{}, fmt.Errorf("special file prevents exact session deletion purge: %s", path)
	}
	relative, err := store.relative(path)
	if err != nil {
		return nil, SessionDeletionPurgeObject{}, SessionDeletionPurgeXattrs{}, err
	}
	file, err := store.root.Open(relative)
	if err != nil {
		return nil, SessionDeletionPurgeObject{}, SessionDeletionPurgeXattrs{}, err
	}
	opened, statErr := file.Stat()
	if statErr != nil || !os.SameFile(before, opened) {
		_ = file.Close()
		if statErr == nil {
			statErr = errors.New("session deletion purge object changed while opening")
		}
		return nil, SessionDeletionPurgeObject{}, SessionDeletionPurgeXattrs{}, statErr
	}
	if err := store.requireSameDevice(opened, path); err != nil {
		_ = file.Close()
		return nil, SessionDeletionPurgeObject{}, SessionDeletionPurgeXattrs{}, err
	}
	firstObject, err := sessionDeletionObjectFromFile(file, opened)
	if err != nil || !validSessionDeletionPurgeObject(firstObject) {
		_ = file.Close()
		if err == nil {
			err = errors.New("session deletion purge object has an invalid durable identity")
		}
		return nil, SessionDeletionPurgeObject{}, SessionDeletionPurgeXattrs{}, err
	}
	xattrs, err := captureSessionDeletionXattrs(file, opened, path)
	if err != nil {
		_ = file.Close()
		return nil, SessionDeletionPurgeObject{}, SessionDeletionPurgeXattrs{}, err
	}
	openedAfterMetadata, err := file.Stat()
	if err != nil || !os.SameFile(opened, openedAfterMetadata) || opened.Mode() != openedAfterMetadata.Mode() {
		_ = file.Close()
		if err == nil {
			err = errors.New("session deletion purge object changed while its open descriptor was inspected")
		}
		return nil, SessionDeletionPurgeObject{}, SessionDeletionPurgeXattrs{}, err
	}
	secondObject, err := sessionDeletionObjectFromFile(file, openedAfterMetadata)
	if err != nil || secondObject != firstObject || !validSessionDeletionPurgeObject(secondObject) {
		_ = file.Close()
		if err == nil {
			err = errors.New("session deletion purge object identity changed during exact metadata capture")
		}
		return nil, SessionDeletionPurgeObject{}, SessionDeletionPurgeXattrs{}, err
	}
	after, err := store.lstat(path)
	if err != nil || !os.SameFile(openedAfterMetadata, after) || openedAfterMetadata.Mode() != after.Mode() {
		_ = file.Close()
		if err == nil {
			err = errors.New("session deletion purge object changed while its metadata was inspected")
		}
		return nil, SessionDeletionPurgeObject{}, SessionDeletionPurgeXattrs{}, err
	}
	openedFinal, err := file.Stat()
	if err != nil || !os.SameFile(after, openedFinal) || after.Mode() != openedFinal.Mode() {
		_ = file.Close()
		if err == nil {
			err = errors.New("session deletion purge path changed before final descriptor revalidation")
		}
		return nil, SessionDeletionPurgeObject{}, SessionDeletionPurgeXattrs{}, err
	}
	finalObject, err := sessionDeletionObjectFromFile(file, openedFinal)
	if err != nil || finalObject != firstObject || !validSessionDeletionPurgeObject(finalObject) {
		_ = file.Close()
		if err == nil {
			err = errors.New("session deletion purge object identity changed before final revalidation")
		}
		return nil, SessionDeletionPurgeObject{}, SessionDeletionPurgeXattrs{}, err
	}
	if err := file.Close(); err != nil {
		return nil, SessionDeletionPurgeObject{}, SessionDeletionPurgeXattrs{}, err
	}
	return after, finalObject, xattrs, nil
}

func validSessionDeletionPurgeObject(object SessionDeletionPurgeObject) bool {
	if object.Device == 0 || object.Inode == 0 || object.BirthtimeUnixNano <= 0 {
		return false
	}
	switch object.IdentityProof {
	case sessionDeletionObjectProofDarwin:
		return object.GenerationSource == sessionDeletionGenerationSourceDarwin &&
			object.BirthtimeSource == sessionDeletionBirthtimeSourceDarwin
	case sessionDeletionObjectProofLinux:
		return object.Generation != 0 &&
			object.GenerationSource == sessionDeletionGenerationSourceLinux &&
			object.BirthtimeSource == sessionDeletionBirthtimeSourceLinux
	default:
		return false
	}
}

func deletionPurgeObjectHashFields(object SessionDeletionPurgeObject) string {
	return object.IdentityProof + "\x00" +
		object.GenerationSource + "\x00" +
		object.BirthtimeSource + "\x00" +
		strconv.FormatUint(object.Device, 10) + "\x00" +
		strconv.FormatUint(object.Inode, 10) + "\x00" +
		strconv.FormatUint(object.Generation, 10) + "\x00" +
		strconv.FormatInt(object.BirthtimeUnixNano, 10) + "\x00" +
		strconv.FormatUint(uint64(object.UID), 10) + "\x00" +
		strconv.FormatUint(uint64(object.GID), 10)
}

func validSessionDeletionPurgeXattrs(xattrs SessionDeletionPurgeXattrs) bool {
	return xattrs.Count >= 0 && xattrs.Bytes >= 0 && validStateSHA256(xattrs.SHA256)
}

func deletionPurgeXattrHashFields(xattrs SessionDeletionPurgeXattrs) string {
	return strconv.Itoa(xattrs.Count) + "\x00" + strconv.FormatInt(xattrs.Bytes, 10) + "\x00" + xattrs.SHA256
}

func captureStableDeletionPurgeFile(path string) (NativeFile, error) {
	first, err := captureDeletionPurgeFile(path)
	if err != nil {
		return NativeFile{}, err
	}
	second, err := captureDeletionPurgeFile(path)
	if err != nil {
		return NativeFile{}, err
	}
	if first != second {
		return NativeFile{}, errors.New("file changed while deletion purge proof was captured")
	}
	return first, nil
}

func captureDeletionPurgeFile(path string) (NativeFile, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return NativeFile{}, err
	}
	if !before.Mode().IsRegular() {
		return NativeFile{}, errors.New("deletion purge entry is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return NativeFile{}, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		if err == nil {
			err = errors.New("deletion purge entry changed while opening")
		}
		return NativeFile{}, err
	}
	hasher := sha256.New()
	bytes, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	after, statErr := os.Lstat(path)
	if err := errors.Join(copyErr, closeErr, statErr); err != nil {
		return NativeFile{}, err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() || bytes != after.Size() {
		return NativeFile{}, errors.New("deletion purge entry changed while hashing")
	}
	return NativeFile{Path: filepath.Clean(path), Bytes: bytes, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

func verifyDeletionPurgeTreeIdentity(storeRoot string, root string, receipt SessionDeletionPurge) error {
	identity, err := captureDeletionPurgeTreeIdentity(storeRoot, root)
	if err != nil {
		return err
	}
	if identity.SHA256 != receipt.QuarantineTreeSHA256 || identity.Bytes != receipt.QuarantineBytes || identity.Entries != receipt.QuarantineEntries || identity.RootMode != receipt.QuarantineRootMode || identity.RootObject != receipt.QuarantineRootObject || identity.RootXattrs != receipt.QuarantineRootXattrs || !sameDeletionPurgeEntries(identity.Tree, receipt.Entries) {
		return errors.New("deleted session quarantine differs from its durable purge proof")
	}
	return nil
}

func verifyDeletionPurgeEntry(storeRoot string, path string, entry SessionDeletionPurgeEntry) error {
	store, err := openSessionDeletionStoreRoot(storeRoot)
	if err != nil {
		return err
	}
	defer store.Close()
	info, object, xattrs, err := captureDeletionPurgeObject(store, path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || uint32(info.Mode().Perm()) != entry.Mode || object != entry.Object || xattrs != entry.Xattrs {
		return fmt.Errorf("deletion purge entry changed type or mode: %s", entry.Path)
	}
	switch entry.Kind {
	case "directory":
		if !info.IsDir() {
			return fmt.Errorf("deletion purge directory was replaced: %s", entry.Path)
		}
	case "file":
		if !info.Mode().IsRegular() {
			return fmt.Errorf("deletion purge file was replaced: %s", entry.Path)
		}
		identity, err := captureStableDeletionFileWithinStore(storeRoot, path)
		if err != nil {
			return err
		}
		if identity.Bytes != entry.Bytes || identity.SHA256 != entry.SHA256 {
			return fmt.Errorf("deletion purge file differs from its durable receipt: %s", entry.Path)
		}
		infoAfter, objectAfter, xattrsAfter, err := captureDeletionPurgeObject(store, path)
		if err != nil || !os.SameFile(info, infoAfter) || objectAfter != object || xattrsAfter != xattrs {
			if err == nil {
				err = fmt.Errorf("deletion purge file metadata changed during verification: %s", entry.Path)
			}
			return err
		}
	default:
		return errors.New("deletion purge receipt contains an unknown entry kind")
	}
	return nil
}

func removeDeletionPurgeStaging(storeRoot string, stagingRoot string, receipt SessionDeletionPurge) (SessionDeletionPurge, error) {
	replacementDetected := false
	entries := append([]SessionDeletionPurgeEntry(nil), receipt.Entries...)
	sort.Slice(entries, func(i, j int) bool {
		leftDepth := strings.Count(entries[i].Path, "/")
		rightDepth := strings.Count(entries[j].Path, "/")
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind == "file"
		}
		return entries[i].Path > entries[j].Path
	})
	store, err := openSessionDeletionStoreRoot(storeRoot)
	if err != nil {
		return receipt, err
	}
	defer store.Close()
	aliasRoot := deletionPurgeAliasRoot(stagingRoot, receipt)
	aliasRootRelative, err := store.relative(aliasRoot)
	if err != nil {
		return receipt, err
	}
	if err := store.root.Mkdir(aliasRootRelative, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return receipt, err
	}
	aliasInfo, _, _, err := captureDeletionPurgeObject(store, aliasRoot)
	if err != nil || !aliasInfo.IsDir() || aliasInfo.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("session deletion purge alias root is not a plain directory")
		}
		return receipt, err
	}
	if err := syncStateDirectory(stagingRoot); err != nil {
		return receipt, fmt.Errorf("sync session deletion purge alias root creation: %w", err)
	}
	staged := make(map[string]struct{}, len(receipt.StagedEntries))
	for _, path := range receipt.StagedEntries {
		staged[path] = struct{}{}
	}
	removed := make(map[string]struct{}, len(receipt.RemovedEntries))
	for _, path := range receipt.RemovedEntries {
		removed[path] = struct{}{}
	}
	for _, entry := range entries {
		original := filepath.Join(stagingRoot, filepath.FromSlash(entry.Path))
		alias := deletionPurgeEntryAliasPath(aliasRoot, entry)
		_, alreadyStaged := staged[entry.Path]
		_, alreadyRemoved := removed[entry.Path]
		originalState, err := inspectDeletionPurgeEntryPath(storeRoot, store, original, entry)
		if err != nil {
			return receipt, err
		}
		aliasState, err := inspectDeletionPurgeEntryPath(storeRoot, store, alias, entry)
		if err != nil {
			return receipt, err
		}
		if alreadyRemoved {
			if originalState.exists || aliasState.exists {
				replacementDetected = true
			}
			continue
		}
		if !alreadyStaged {
			switch {
			case aliasState.exact:
				if originalState.exists {
					replacementDetected = true
				}
				if err := syncDeletionRenameParents(original, alias); err != nil {
					return receipt, err
				}
				if err := verifyDeletionPurgeEntry(storeRoot, alias, entry); err != nil {
					return receipt, fmt.Errorf("reverify unrecorded staged deletion purge entry %s: %w", entry.Path, err)
				}
				if err := markDeletionPurgeEntryStaged(storeRoot, &receipt, entry.Path); err != nil {
					return receipt, err
				}
				staged[entry.Path] = struct{}{}
				alreadyStaged = true
			case originalState.exact && !aliasState.exists:
				if entry.Kind == "directory" {
					empty, err := deletionPurgeDirectoryEmpty(store, original)
					if err != nil {
						return receipt, err
					}
					if !empty {
						replacementDetected = true
						continue
					}
				}
				if err := runSessionDeletionPurgeHook("before-entry-stage:" + entry.Path); err != nil {
					return receipt, err
				}
				if err := verifyDeletionPurgeEntry(storeRoot, original, entry); err != nil {
					replacementDetected = true
					continue
				}
				if entry.Kind == "directory" {
					empty, err := deletionPurgeDirectoryEmpty(store, original)
					if err != nil {
						return receipt, err
					}
					if !empty {
						replacementDetected = true
						continue
					}
				}
				if err := sessionDeletionRenameNoReplace(store, original, alias); err != nil {
					return receipt, fmt.Errorf("stage exact deletion purge entry %s: %w", entry.Path, err)
				}
				if err := verifyDeletionPurgeEntry(storeRoot, alias, entry); err != nil {
					if state, stateErr := inspectDeletionPurgeEntryPath(storeRoot, store, alias, entry); stateErr == nil && state.sameObject {
						if exists, existsErr := deletionPurgePathExists(store, original); existsErr == nil && !exists {
							_ = sessionDeletionRenameNoReplace(store, alias, original)
							_ = syncDeletionRenameParents(alias, original)
						}
					}
					return receipt, fmt.Errorf("reverify atomically staged deletion purge entry %s: %w", entry.Path, err)
				}
				if err := syncDeletionRenameParents(original, alias); err != nil {
					return receipt, err
				}
				if err := verifyDeletionPurgeEntry(storeRoot, alias, entry); err != nil {
					return receipt, fmt.Errorf("reverify durably staged deletion purge entry %s: %w", entry.Path, err)
				}
				if err := runSessionDeletionPurgeHook("after-entry-stage:" + entry.Path); err != nil {
					return receipt, err
				}
				if err := markDeletionPurgeEntryStaged(storeRoot, &receipt, entry.Path); err != nil {
					return receipt, err
				}
				staged[entry.Path] = struct{}{}
				alreadyStaged = true
				if err := runSessionDeletionPurgeHook("after-entry-marked:" + entry.Path); err != nil {
					return receipt, err
				}
			default:
				if originalState.exists || aliasState.exists {
					replacementDetected = true
				}
				continue
			}
		}
		if alreadyStaged {
			originalState, err = inspectDeletionPurgeEntryPath(storeRoot, store, original, entry)
			if err != nil {
				return receipt, err
			}
			aliasState, err = inspectDeletionPurgeEntryPath(storeRoot, store, alias, entry)
			if err != nil {
				return receipt, err
			}
			if !aliasState.exact && originalState.exact && !aliasState.exists {
				if entry.Kind == "directory" {
					empty, err := deletionPurgeDirectoryEmpty(store, original)
					if err != nil {
						return receipt, err
					}
					if !empty {
						replacementDetected = true
						continue
					}
				}
				if err := sessionDeletionRenameNoReplace(store, original, alias); err != nil {
					return receipt, fmt.Errorf("restage durable deletion purge entry %s: %w", entry.Path, err)
				}
				if err := syncDeletionRenameParents(original, alias); err != nil {
					return receipt, err
				}
				aliasState, err = inspectDeletionPurgeEntryPath(storeRoot, store, alias, entry)
				if err != nil {
					return receipt, err
				}
				originalState, err = inspectDeletionPurgeEntryPath(storeRoot, store, original, entry)
				if err != nil {
					return receipt, err
				}
			}
			if aliasState.exact {
				if originalState.exists {
					replacementDetected = true
				}
				if entry.Kind == "directory" {
					empty, err := deletionPurgeDirectoryEmpty(store, alias)
					if err != nil {
						return receipt, err
					}
					if !empty {
						replacementDetected = true
						continue
					}
				}
				aliasRelative, err := store.relative(alias)
				if err != nil {
					return receipt, err
				}
				if err := runSessionDeletionPurgeHook("before-staged-entry-remove:" + entry.Path); err != nil {
					return receipt, err
				}
				if err := verifyDeletionPurgeEntry(storeRoot, alias, entry); err != nil {
					replacementDetected = true
					continue
				}
				if entry.Kind == "directory" {
					empty, err := deletionPurgeDirectoryEmpty(store, alias)
					if err != nil {
						return receipt, err
					}
					if !empty {
						replacementDetected = true
						continue
					}
				}
				if err := store.root.Remove(aliasRelative); err != nil {
					return receipt, fmt.Errorf("remove exactly staged deletion purge entry %s: %w", entry.Path, err)
				}
				if err := syncStateDirectory(aliasRoot); err != nil {
					return receipt, fmt.Errorf("sync exact deletion purge entry removal %s: %w", entry.Path, err)
				}
				if err := runSessionDeletionPurgeHook("removed-entry:" + entry.Path); err != nil {
					return receipt, err
				}
				if err := markDeletionPurgeEntryRemoved(storeRoot, &receipt, entry.Path); err != nil {
					return receipt, err
				}
				removed[entry.Path] = struct{}{}
				continue
			}

			if originalState.exists || aliasState.exists {
				replacementDetected = true
			}
			if originalState.sameObject || aliasState.sameObject {
				continue
			}
			if err := syncDeletionRenameParents(original, alias); err != nil {
				return receipt, err
			}
			originalState, err = inspectDeletionPurgeEntryPath(storeRoot, store, original, entry)
			if err != nil {
				return receipt, err
			}
			aliasState, err = inspectDeletionPurgeEntryPath(storeRoot, store, alias, entry)
			if err != nil {
				return receipt, err
			}
			if originalState.sameObject || aliasState.sameObject {
				continue
			}
			if err := markDeletionPurgeEntryRemoved(storeRoot, &receipt, entry.Path); err != nil {
				return receipt, err
			}
			removed[entry.Path] = struct{}{}
		}
	}
	if err := store.root.Remove(aliasRootRelative); err != nil && !errors.Is(err, os.ErrNotExist) {
		replacementDetected = true
	} else if err == nil {
		if err := syncStateDirectory(stagingRoot); err != nil {
			return receipt, err
		}
	}
	remainingReplacement, err := inspectDeletionPurgeStaging(storeRoot, stagingRoot, receipt)
	if err != nil {
		return receipt, err
	}
	if replacementDetected || remainingReplacement {
		return receipt, errors.New("replacement or unknown content was preserved in deletion purge staging")
	}
	if len(receipt.RemovedEntries) != len(receipt.Entries) {
		return receipt, errors.New("deletion purge staging still has unremoved proven entries")
	}
	rootInfo, rootObject, rootXattrs, err := captureDeletionPurgeObject(store, stagingRoot)
	if err != nil {
		return receipt, err
	}
	if !rootInfo.IsDir() || uint32(rootInfo.Mode().Perm()) != receipt.QuarantineRootMode || rootObject != receipt.QuarantineRootObject || rootXattrs != receipt.QuarantineRootXattrs {
		return receipt, errors.New("deletion purge staging root differs from its durable receipt")
	}
	empty, err := deletionPurgeDirectoryEmpty(store, stagingRoot)
	if err != nil || !empty {
		if err == nil {
			err = errors.New("deletion purge staging root is not empty")
		}
		return receipt, err
	}
	stagingRelative, err := store.relative(stagingRoot)
	if err != nil {
		return receipt, err
	}
	if err := store.root.Remove(stagingRelative); err != nil && !errors.Is(err, os.ErrNotExist) {
		return receipt, fmt.Errorf("remove empty deletion purge staging: %w", err)
	}
	return receipt, syncStateDirectory(filepath.Dir(stagingRoot))
}

type deletionPurgeEntryPathState struct {
	exists     bool
	sameObject bool
	exact      bool
}

func inspectDeletionPurgeEntryPath(storeRoot string, store *sessionDeletionStoreRoot, path string, entry SessionDeletionPurgeEntry) (deletionPurgeEntryPathState, error) {
	_, err := store.lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return deletionPurgeEntryPathState{}, nil
	}
	if err != nil {
		return deletionPurgeEntryPathState{}, err
	}
	_, object, _, err := captureDeletionPurgeObject(store, path)
	if err != nil {
		return deletionPurgeEntryPathState{}, err
	}
	state := deletionPurgeEntryPathState{exists: true, sameObject: object == entry.Object}
	if !state.sameObject {
		return state, nil
	}
	state.exact = verifyDeletionPurgeEntry(storeRoot, path, entry) == nil
	return state, nil
}

func inspectDeletionPurgeStaging(storeRoot string, stagingRoot string, receipt SessionDeletionPurge) (bool, error) {
	expected := make(map[string]SessionDeletionPurgeEntry, len(receipt.Entries))
	aliasExpected := make(map[string]SessionDeletionPurgeEntry, len(receipt.Entries))
	aliasRoot := deletionPurgeAliasRoot(stagingRoot, receipt)
	aliasRootName := filepath.Base(aliasRoot)
	for _, entry := range receipt.Entries {
		if entry.Path == aliasRootName || strings.HasPrefix(entry.Path, aliasRootName+"/") {
			return false, errors.New("deletion purge receipt collides with its private alias namespace")
		}
		expected[entry.Path] = entry
		aliasExpected[filepath.ToSlash(filepath.Join(aliasRootName, filepath.Base(deletionPurgeEntryAliasPath(aliasRoot, entry))))] = entry
	}
	staged := make(map[string]struct{}, len(receipt.StagedEntries))
	for _, path := range receipt.StagedEntries {
		staged[path] = struct{}{}
	}
	removed := make(map[string]struct{}, len(receipt.RemovedEntries))
	for _, path := range receipt.RemovedEntries {
		removed[path] = struct{}{}
	}
	store, err := openSessionDeletionStoreRoot(storeRoot)
	if err != nil {
		return false, err
	}
	defer store.Close()
	rootRelative, err := store.relative(stagingRoot)
	if err != nil {
		return false, err
	}
	rootFSPath := filepath.ToSlash(rootRelative)
	aliasStates := make(map[string]deletionPurgeEntryPathState, len(receipt.Entries))
	for _, entry := range receipt.Entries {
		state, err := inspectDeletionPurgeEntryPath(storeRoot, store, deletionPurgeEntryAliasPath(aliasRoot, entry), entry)
		if err != nil {
			return false, err
		}
		aliasStates[entry.Path] = state
	}
	replacement := false
	err = fs.WalkDir(store.root.FS(), rootFSPath, func(current string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		absolute := stagingRoot
		if current != rootFSPath {
			key := strings.TrimPrefix(current, rootFSPath+"/")
			absolute = filepath.Join(stagingRoot, filepath.FromSlash(key))
		}
		info, err := store.lstat(absolute)
		if err != nil {
			return err
		}
		if current == rootFSPath {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("session deletion purge staging is not a plain directory")
			}
			_, object, xattrs, exactErr := captureDeletionPurgeObject(store, absolute)
			if exactErr != nil {
				replacement = true
				return fs.SkipAll
			}
			if uint32(info.Mode().Perm()) != receipt.QuarantineRootMode || object != receipt.QuarantineRootObject || xattrs != receipt.QuarantineRootXattrs {
				replacement = true
				return fs.SkipAll
			}
			return nil
		}
		key := strings.TrimPrefix(current, rootFSPath+"/")
		if key == aliasRootName {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				replacement = true
				return fs.SkipDir
			}
			if _, _, _, exactErr := captureDeletionPurgeObject(store, absolute); exactErr != nil {
				replacement = true
				return fs.SkipDir
			}
			return nil
		}
		if entry, ok := aliasExpected[key]; ok {
			state, err := inspectDeletionPurgeEntryPath(storeRoot, store, absolute, entry)
			if err != nil {
				return err
			}
			if _, wasRemoved := removed[entry.Path]; wasRemoved || !state.exact {
				replacement = true
			}
			return nil
		}
		entry, ok := expected[key]
		if !ok {
			replacement = true
			if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				return fs.SkipDir
			}
			return nil
		}
		state, err := inspectDeletionPurgeEntryPath(storeRoot, store, absolute, entry)
		if err != nil {
			return err
		}
		_, alreadyStaged := staged[key]
		_, alreadyRemoved := removed[key]
		allowedRollback := alreadyStaged && !alreadyRemoved && !aliasStates[key].exists && state.exact
		if alreadyRemoved || alreadyStaged && !allowedRollback || !alreadyStaged && !state.exact {
			replacement = true
		}
		return nil
	})
	return replacement, err
}

func deletionPurgeAliasRoot(stagingRoot string, receipt SessionDeletionPurge) string {
	return filepath.Join(stagingRoot, ".codexfold-delete-"+receipt.QuarantineTreeSHA256)
}

func deletionPurgeEntryAliasPath(aliasRoot string, entry SessionDeletionPurgeEntry) string {
	digest := sha256.Sum256([]byte(entry.Path))
	return filepath.Join(aliasRoot, hex.EncodeToString(digest[:]))
}

func deletionPurgePathExists(store *sessionDeletionStoreRoot, path string) (bool, error) {
	_, err := store.lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func deletionPurgeDirectoryEmpty(store *sessionDeletionStoreRoot, path string) (bool, error) {
	relative, err := store.relative(path)
	if err != nil {
		return false, err
	}
	directory, err := store.root.Open(relative)
	if err != nil {
		return false, err
	}
	entries, readErr := directory.ReadDir(1)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return false, readErr
	}
	if closeErr != nil {
		return false, closeErr
	}
	return len(entries) == 0, nil
}

func markDeletionPurgeEntryStaged(root string, receipt *SessionDeletionPurge, path string) error {
	index, exists := slices.BinarySearch(receipt.StagedEntries, path)
	if exists {
		return nil
	}
	receipt.StagedEntries = append(receipt.StagedEntries, "")
	copy(receipt.StagedEntries[index+1:], receipt.StagedEntries[index:])
	receipt.StagedEntries[index] = path
	return replaceSessionDeletionPurge(root, *receipt)
}

func markDeletionPurgeEntryRemoved(root string, receipt *SessionDeletionPurge, path string) error {
	if _, staged := slices.BinarySearch(receipt.StagedEntries, path); !staged {
		return errors.New("cannot mark an unstaged deletion purge entry removed")
	}
	index, exists := slices.BinarySearch(receipt.RemovedEntries, path)
	if exists {
		return nil
	}
	receipt.RemovedEntries = append(receipt.RemovedEntries, "")
	copy(receipt.RemovedEntries[index+1:], receipt.RemovedEntries[index:])
	receipt.RemovedEntries[index] = path
	return replaceSessionDeletionPurge(root, *receipt)
}

func sameDeletionPurgeEntries(left []SessionDeletionPurgeEntry, right []SessionDeletionPurgeEntry) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameDeletionPurgeStrings(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func deletionPurgeStagingPath(root string, tombstone SessionDeletion) string {
	return filepath.Join(filepath.Clean(root), "fs", "deletion-purge-staging", tombstone.SessionID, tombstone.OperationToken)
}

func prepareDeletionPurgeStaging(root string, stagingPath string) error {
	parent := filepath.Dir(stagingPath)
	if err := ensurePlainDeletionDirectoryChain(root, parent); err != nil {
		return fmt.Errorf("create session deletion purge staging: %w", err)
	}
	if err := requirePlainDeletionDirectory(parent); err != nil {
		return err
	}
	if _, err := os.Lstat(stagingPath); err == nil {
		return errors.New("session deletion purge staging appeared before rename")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func plainDeletionPurgeDirectoryExists(root string, path string) (bool, error) {
	storeRoot, err := openSessionDeletionStoreRoot(root)
	if err != nil {
		return false, err
	}
	defer storeRoot.Close()
	if err := storeRoot.validateExistingPlainAncestors(path); err != nil {
		return false, err
	}
	relative, err := storeRoot.relative(path)
	if err != nil {
		return false, err
	}
	info, err := storeRoot.root.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("session deletion purge path is not a plain directory")
	}
	if err := storeRoot.requireSameDevice(info, path); err != nil {
		return false, err
	}
	return true, nil
}

func validateSessionDeletionPurge(root string, receipt SessionDeletionPurge) error {
	if !filepath.IsAbs(root) || receipt.Version != sessionDeletionPurgeVersion || receipt.Kind != sessionDeletionPurgeKind || !safeSessionID(receipt.SessionID) || len(receipt.OperationToken) != 32 || !validStateSHA256(receipt.TombstoneSHA256) || !validStateSHA256(receipt.QuarantineTreeSHA256) || receipt.QuarantineBytes < 0 || receipt.QuarantineEntries < 2 || len(receipt.Entries) != receipt.QuarantineEntries || receipt.QuarantineRootMode > 0o777 || !validSessionDeletionPurgeObject(receipt.QuarantineRootObject) || !validSessionDeletionPurgeXattrs(receipt.QuarantineRootXattrs) {
		return errors.New("invalid managed session deletion purge receipt")
	}
	seen := make(map[string]struct{}, len(receipt.Entries))
	var fileBytes int64
	previousEntryPath := ""
	treeHasher := sha256.New()
	_, _ = io.WriteString(treeHasher, "R\x00"+strconv.FormatUint(uint64(receipt.QuarantineRootMode), 8)+"\x00"+deletionPurgeObjectHashFields(receipt.QuarantineRootObject)+"\x00"+deletionPurgeXattrHashFields(receipt.QuarantineRootXattrs)+"\n")
	for _, entry := range receipt.Entries {
		cleaned := pathCleanDeletionPurgeEntry(entry.Path)
		if cleaned == "" || cleaned != entry.Path {
			return errors.New("session deletion purge receipt contains an unsafe entry path")
		}
		if _, duplicate := seen[entry.Path]; duplicate {
			return errors.New("session deletion purge receipt contains a duplicate entry")
		}
		seen[entry.Path] = struct{}{}
		if previousEntryPath != "" && entry.Path <= previousEntryPath {
			return errors.New("session deletion purge receipt entries are not in canonical order")
		}
		previousEntryPath = entry.Path
		if entry.Mode > 0o777 || !validSessionDeletionPurgeObject(entry.Object) || !validSessionDeletionPurgeXattrs(entry.Xattrs) {
			return errors.New("session deletion purge receipt contains invalid entry metadata")
		}
		mode := strconv.FormatUint(uint64(entry.Mode), 8)
		switch entry.Kind {
		case "directory":
			if entry.Bytes != 0 || entry.SHA256 != "" {
				return errors.New("session deletion purge directory entry contains file identity")
			}
			_, _ = io.WriteString(treeHasher, "D\x00"+entry.Path+"\x00"+mode+"\x00"+deletionPurgeObjectHashFields(entry.Object)+"\x00"+deletionPurgeXattrHashFields(entry.Xattrs)+"\n")
		case "file":
			if entry.Bytes < 0 || !validStateSHA256(entry.SHA256) || fileBytes > int64(^uint64(0)>>1)-entry.Bytes {
				return errors.New("session deletion purge receipt contains an invalid file entry")
			}
			fileBytes += entry.Bytes
			_, _ = io.WriteString(treeHasher, "F\x00"+entry.Path+"\x00"+mode+"\x00"+deletionPurgeObjectHashFields(entry.Object)+"\x00"+deletionPurgeXattrHashFields(entry.Xattrs)+"\x00"+strconv.FormatInt(entry.Bytes, 10)+"\x00"+entry.SHA256+"\n")
		default:
			return errors.New("session deletion purge receipt contains an unknown entry kind")
		}
	}
	if fileBytes != receipt.QuarantineBytes {
		return errors.New("session deletion purge receipt byte count differs from its entries")
	}
	if hex.EncodeToString(treeHasher.Sum(nil)) != receipt.QuarantineTreeSHA256 {
		return errors.New("session deletion purge receipt tree hash differs from its entries")
	}
	previousStaged := ""
	stagedSet := make(map[string]struct{}, len(receipt.StagedEntries))
	for _, staged := range receipt.StagedEntries {
		if _, exists := seen[staged]; !exists || staged <= previousStaged {
			return errors.New("session deletion purge receipt contains invalid staged-entry progress")
		}
		stagedSet[staged] = struct{}{}
		previousStaged = staged
	}
	previousRemoved := ""
	for _, removed := range receipt.RemovedEntries {
		if _, exists := stagedSet[removed]; !exists || removed <= previousRemoved {
			return errors.New("session deletion purge receipt contains invalid removed-entry progress")
		}
		previousRemoved = removed
	}
	if token, err := hex.DecodeString(receipt.OperationToken); err != nil || len(token) != 16 {
		return errors.New("session deletion purge has an invalid operation token")
	}
	wantStaging := filepath.Join(filepath.Clean(root), "fs", "deletion-purge-staging", receipt.SessionID, receipt.OperationToken)
	if filepath.Clean(receipt.StagingPath) != wantStaging {
		return errors.New("session deletion purge contains an unsafe staging path")
	}
	if preparedAt, err := time.Parse(time.RFC3339Nano, receipt.PreparedAt); err != nil || preparedAt.IsZero() {
		return errors.New("session deletion purge has an invalid preparation time")
	}
	switch receipt.Phase {
	case sessionDeletionPurgePrepared, sessionDeletionPurgeRenamed:
		if receipt.CompletedAt != "" {
			return errors.New("incomplete session deletion purge has a completion time")
		}
		if receipt.Phase == sessionDeletionPurgePrepared && (len(receipt.StagedEntries) != 0 || len(receipt.RemovedEntries) != 0) {
			return errors.New("prepared session deletion purge contains entry progress")
		}
	case sessionDeletionPurgeComplete:
		if completedAt, err := time.Parse(time.RFC3339Nano, receipt.CompletedAt); err != nil || completedAt.IsZero() {
			return errors.New("completed session deletion purge has an invalid completion time")
		}
		if len(receipt.StagedEntries) != len(receipt.Entries) || len(receipt.RemovedEntries) != len(receipt.Entries) {
			return errors.New("completed session deletion purge lacks complete entry progress")
		}
	default:
		return errors.New("session deletion purge has an unknown phase")
	}
	return nil
}

func pathCleanDeletionPurgeEntry(value string) string {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.Contains(value, "\\") {
		return ""
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || filepath.IsAbs(filepath.FromSlash(value)) {
		return ""
	}
	return cleaned
}

func writeSessionDeletionPurge(root string, receipt SessionDeletionPurge) error {
	return commitSessionDeletionPurge(root, receipt, false)
}

func replaceSessionDeletionPurge(root string, receipt SessionDeletionPurge) error {
	return commitSessionDeletionPurge(root, receipt, true)
}

func commitSessionDeletionPurge(root string, receipt SessionDeletionPurge, replace bool) error {
	if err := validateSessionDeletionPurge(root, receipt); err != nil {
		return err
	}
	path := SessionDeletionPurgePath(root, receipt.SessionID)
	directory := filepath.Dir(path)
	if err := ensurePlainDeletionDirectoryChain(root, directory); err != nil {
		return fmt.Errorf("create session deletion purge directory: %w", err)
	}
	if err := requirePlainDeletionDirectory(directory); err != nil {
		return err
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	storeRoot, err := openSessionDeletionStoreRoot(root)
	if err != nil {
		return err
	}
	defer storeRoot.Close()
	if err := storeRoot.commitRegularFile(path, ".deletion-purge-", data, replace); err != nil {
		return fmt.Errorf("publish session deletion purge receipt: %w", err)
	}
	return nil
}

func runSessionDeletionPurgeHook(phase string) error {
	if sessionDeletionPurgeHook == nil {
		return nil
	}
	return sessionDeletionPurgeHook(phase)
}

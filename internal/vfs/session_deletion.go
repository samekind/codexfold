package vfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/storage"
)

const (
	legacySessionDeletionVersion  = 1
	sidecarSessionDeletionVersion = 2
	sessionDeletionVersion        = 3
	sessionDeletionKind           = "codexfold-managed-session-deletion"
	maximumSessionDeletionBytes   = 1 << 20
)

var ErrSessionDeletionBusy = errors.New("managed session deletion is waiting for active leases")

var sessionDeletionPublicationHook func(string) error

// SessionDeletion is durable proof that a canonical managed path was
// explicitly unlinked. Missing discovery metadata never creates this record.
type SessionDeletion struct {
	Version                   int        `json:"version"`
	Kind                      string     `json:"kind"`
	SessionID                 string     `json:"session_id"`
	InitialStateGeneration    uint64     `json:"initial_state_generation"`
	InitialStateSHA256        string     `json:"initial_state_sha256"`
	InitialCheckpointSequence uint64     `json:"initial_checkpoint_sequence"`
	InitialCheckpointSHA256   string     `json:"initial_checkpoint_sha256"`
	StateGeneration           uint64     `json:"state_generation"`
	StateSHA256               string     `json:"state_sha256"`
	ManifestPath              string     `json:"manifest_path"`
	ManifestSHA256            string     `json:"manifest_sha256"`
	Route                     string     `json:"route"`
	OperationToken            string     `json:"operation_token"`
	DeletedAt                 string     `json:"deleted_at"`
	SessionPath               string     `json:"session_path"`
	RetiredSessionPath        string     `json:"retired_session_path"`
	RetiredManifestPath       string     `json:"retired_manifest_path"`
	NativeSnapshotSidecar     NativeFile `json:"native_snapshot_sidecar"`
	RetirementRequest         NativeFile `json:"retirement_request"`
	RetirementAcknowledgement NativeFile `json:"retirement_acknowledgement"`
	MountAcknowledgement      NativeFile `json:"mount_acknowledgement"`
	Journal                   NativeFile `json:"journal"`
	NativeRetirementProof     NativeFile `json:"native_retirement_proof"`
}

func SessionDeletionPath(root string, sessionID string) string {
	return filepath.Join(filepath.Clean(root), "fs", "deletions", sessionID+".json")
}

// PublishSessionDeletion verifies the live state and exact manifest bytes,
// then atomically publishes the only authority for managed-session deletion.
func PublishSessionDeletion(root string, state SessionState, route string) (SessionDeletion, error) {
	root = filepath.Clean(root)
	if root == "." || !filepath.IsAbs(root) || !safeSessionID(state.SessionID) {
		return SessionDeletion{}, errors.New("absolute store root and safe session ID are required")
	}
	cleanRoute, err := cleanSessionDeletionRoute(route)
	if err != nil {
		return SessionDeletion{}, err
	}
	sessionPath := filepath.Join(root, "fs", "sessions", state.SessionID)
	if err := validateSessionStateForDirectory(state, sessionPath); err != nil {
		return SessionDeletion{}, fmt.Errorf("validate deletion state: %w", err)
	}

	lock, err := storage.AcquireOperationLock(root, "session-deletions")
	if err != nil {
		return SessionDeletion{}, err
	}
	defer lock.Close()
	checkpointLock, err := acquireSessionCheckpointLock(sessionPath, state.SessionID)
	if err != nil {
		if errors.Is(err, storage.ErrOperationLockHeld) {
			return SessionDeletion{}, fmt.Errorf("%w: checkpoint publication lock", ErrSessionDeletionBusy)
		}
		return SessionDeletion{}, err
	}
	defer checkpointLock.Close()
	storeRoot, err := openSessionDeletionStoreRoot(root)
	if err != nil {
		return SessionDeletion{}, err
	}
	defer storeRoot.Close()
	if info, err := storeRoot.lstat(sessionPath); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("managed session deletion source is not a plain directory")
		}
		return SessionDeletion{}, err
	}
	if err := storeRoot.validatePlainAncestors(state.ManifestPath); err != nil {
		return SessionDeletion{}, fmt.Errorf("validate deletion manifest ancestors: %w", err)
	}

	if existing, loadErr := LoadSessionDeletion(root, state.SessionID); loadErr == nil {
		if existing.StateGeneration == state.Generation && existing.ManifestSHA256 == state.ManifestSHA256 && existing.ManifestPath == filepath.Clean(state.ManifestPath) && existing.Route == cleanRoute {
			return existing, nil
		}
		return SessionDeletion{}, errors.New("a different deletion tombstone already exists for the managed session")
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return SessionDeletion{}, loadErr
	}

	statePath := filepath.Join(sessionPath, "state.json")
	persisted, stateData, err := readSessionState(statePath)
	if err != nil {
		return SessionDeletion{}, fmt.Errorf("read deletion state proof: %w", err)
	}
	if err := validateSessionStateForDirectory(persisted, sessionPath); err != nil {
		return SessionDeletion{}, fmt.Errorf("validate persisted deletion state: %w", err)
	}
	if persisted != state {
		return SessionDeletion{}, errors.New("managed session state changed before deletion publication")
	}
	stateSHA256 := digestDeletionBytes(stateData)
	checkpointChain, err := verifyDeletionCheckpointHead(sessionPath, persisted, stateSHA256)
	if err != nil {
		return SessionDeletion{}, fmt.Errorf("verify deletion checkpoint head: %w", err)
	}
	stateIdentity, err := storeRoot.captureStableRegularFile(statePath)
	if err != nil || stateIdentity.Bytes != int64(len(stateData)) || stateIdentity.SHA256 != stateSHA256 {
		if err == nil {
			err = errors.New("managed session state changed during deletion publication")
		}
		return SessionDeletion{}, err
	}
	manifestIdentity, err := storeRoot.captureStableRegularFile(state.ManifestPath)
	if err != nil {
		return SessionDeletion{}, fmt.Errorf("verify deletion manifest: %w", err)
	}
	if manifestIdentity.SHA256 != state.ManifestSHA256 {
		return SessionDeletion{}, errors.New("managed session manifest differs from the exact state identity")
	}
	sidecar := NativeFile{}
	expectedSnapshot := filepath.Join(root, "fs", "snapshots", state.SessionID, "native.jsonl")
	if filepath.Clean(state.NativeSnapshot.Path) == expectedSnapshot {
		sidecarPath := filepath.Join(filepath.Dir(expectedSnapshot), "._native.jsonl")
		if _, err := storeRoot.lstat(sidecarPath); err == nil {
			sidecar, err = storeRoot.captureStableRegularFile(sidecarPath)
			if err != nil {
				return SessionDeletion{}, fmt.Errorf("capture native snapshot sidecar for deletion: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return SessionDeletion{}, err
		}
	}
	protocolMetadata, err := captureSessionDeletionProtocolMetadata(storeRoot, sessionPath)
	if err != nil {
		return SessionDeletion{}, err
	}
	if sessionDeletionPublicationHook != nil {
		if err := sessionDeletionPublicationHook("checkpoint-captured"); err != nil {
			return SessionDeletion{}, err
		}
	}

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return SessionDeletion{}, fmt.Errorf("create deletion operation token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	retiredRoot := filepath.Join(root, "fs", "deleted", state.SessionID, token)
	tombstone := SessionDeletion{
		Version: sessionDeletionVersion, Kind: sessionDeletionKind,
		SessionID: state.SessionID, InitialStateGeneration: state.Generation, InitialStateSHA256: stateSHA256,
		InitialCheckpointSequence: checkpointChain.catalog.Sequence, InitialCheckpointSHA256: checkpointChain.catalog.CheckpointSHA256,
		StateGeneration: state.Generation, StateSHA256: stateSHA256,
		ManifestPath: filepath.Clean(state.ManifestPath), ManifestSHA256: state.ManifestSHA256,
		Route: cleanRoute, OperationToken: token, DeletedAt: time.Now().UTC().Format(time.RFC3339Nano),
		SessionPath: sessionPath, RetiredSessionPath: filepath.Join(retiredRoot, "session"),
		RetiredManifestPath: filepath.Join(retiredRoot, "manifest.json"), NativeSnapshotSidecar: sidecar,
		RetirementRequest: protocolMetadata.retirementRequest, RetirementAcknowledgement: protocolMetadata.retirementAcknowledgement,
		MountAcknowledgement: protocolMetadata.mountAcknowledgement, Journal: protocolMetadata.journal,
		NativeRetirementProof: protocolMetadata.nativeRetirementProof,
	}
	if err := validateSessionDeletion(root, tombstone); err != nil {
		return SessionDeletion{}, err
	}
	if err := writeSessionDeletion(root, tombstone); err != nil {
		return SessionDeletion{}, err
	}
	return tombstone, nil
}

type sessionDeletionProtocolMetadata struct {
	retirementRequest         NativeFile
	retirementAcknowledgement NativeFile
	mountAcknowledgement      NativeFile
	journal                   NativeFile
	nativeRetirementProof     NativeFile
}

func captureSessionDeletionProtocolMetadata(storeRoot *sessionDeletionStoreRoot, directory string) (sessionDeletionProtocolMetadata, error) {
	metadata := sessionDeletionProtocolMetadata{}
	for _, item := range []struct {
		name        string
		destination *NativeFile
	}{
		{"retire.request.json", &metadata.retirementRequest},
		{"retire.ack.json", &metadata.retirementAcknowledgement},
		{"mounted.json", &metadata.mountAcknowledgement},
		{"journal.jsonl", &metadata.journal},
		{NativeRetirementFilename, &metadata.nativeRetirementProof},
	} {
		path := filepath.Join(directory, item.name)
		if _, err := storeRoot.lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return sessionDeletionProtocolMetadata{}, err
		}
		identity, err := storeRoot.captureStableRegularFile(path)
		if err != nil {
			return sessionDeletionProtocolMetadata{}, fmt.Errorf("capture deletion protocol metadata %s: %w", item.name, err)
		}
		*item.destination = identity
	}
	return metadata, nil
}

func applySessionDeletionProtocolMetadata(tombstone SessionDeletion, metadata sessionDeletionProtocolMetadata) SessionDeletion {
	tombstone.RetirementRequest = metadata.retirementRequest
	tombstone.RetirementAcknowledgement = metadata.retirementAcknowledgement
	tombstone.MountAcknowledgement = metadata.mountAcknowledgement
	tombstone.Journal = metadata.journal
	tombstone.NativeRetirementProof = metadata.nativeRetirementProof
	return tombstone
}

func LoadSessionDeletion(root string, sessionID string) (SessionDeletion, error) {
	if root == "" || !safeSessionID(sessionID) {
		return SessionDeletion{}, errors.New("store root and safe session ID are required")
	}
	root = filepath.Clean(root)
	storeRoot, err := openSessionDeletionStoreRoot(root)
	if err != nil {
		return SessionDeletion{}, err
	}
	defer storeRoot.Close()
	data, _, err := storeRoot.readStableRegularFile(SessionDeletionPath(root, sessionID), maximumSessionDeletionBytes)
	if err != nil {
		return SessionDeletion{}, err
	}
	var tombstone SessionDeletion
	if err := decodeSessionDeletion(data, &tombstone); err != nil {
		return SessionDeletion{}, fmt.Errorf("decode session deletion tombstone: %w", err)
	}
	if tombstone.SessionID != sessionID {
		return SessionDeletion{}, errors.New("session deletion tombstone belongs to another session")
	}
	if err := validateSessionDeletion(root, tombstone); err != nil {
		return SessionDeletion{}, err
	}
	return tombstone, nil
}

func DiscoverSessionDeletions(root string) ([]SessionDeletion, error) {
	root = filepath.Clean(root)
	directory := filepath.Join(root, "fs", "deletions")
	storeRoot, err := openSessionDeletionStoreRoot(root)
	if err != nil {
		return nil, err
	}
	defer storeRoot.Close()
	info, err := storeRoot.lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session deletion tombstones: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("session deletion tombstone directory is not a plain directory")
	}
	relative, err := storeRoot.relative(directory)
	if err != nil {
		return nil, err
	}
	handle, err := storeRoot.root.Open(relative)
	if err != nil {
		return nil, err
	}
	entries, readErr := handle.ReadDir(-1)
	closeErr := handle.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("read session deletion tombstones: %w", err)
	}
	deletions := make([]SessionDeletion, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".deletion-") && strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil, fmt.Errorf("unrecognized session deletion entry: %s", entry.Name())
		}
		sessionID := strings.TrimSuffix(entry.Name(), ".json")
		deletion, err := LoadSessionDeletion(root, sessionID)
		if err != nil {
			return nil, err
		}
		deletions = append(deletions, deletion)
	}
	sort.Slice(deletions, func(i, j int) bool { return deletions[i].SessionID < deletions[j].SessionID })
	return deletions, nil
}

// AdvanceSessionDeletion moves the state directory and manifest into their
// deterministic quarantine. Busy leases are a retryable defer, not deletion.
func AdvanceSessionDeletion(root string, tombstone SessionDeletion) (bool, error) {
	root = filepath.Clean(root)
	if err := validateSessionDeletion(root, tombstone); err != nil {
		return false, err
	}
	lock, err := storage.AcquireOperationLock(root, "session-deletions")
	if err != nil {
		if errors.Is(err, storage.ErrOperationLockHeld) {
			return false, fmt.Errorf("%w: deletion replay lock", ErrSessionDeletionBusy)
		}
		return false, err
	}
	defer lock.Close()

	current, err := LoadSessionDeletion(root, tombstone.SessionID)
	if err != nil {
		return false, err
	}
	if current != tombstone {
		return false, errors.New("session deletion tombstone changed before replay")
	}
	return advanceSessionDeletionLocked(root, tombstone)
}

func advanceSessionDeletionLocked(root string, tombstone SessionDeletion) (bool, error) {
	if purged, err := resumeSessionDeletionPurgeLocked(root, tombstone); purged || err != nil {
		return purged, err
	}
	storeRoot, err := openSessionDeletionStoreRoot(root)
	if err != nil {
		return false, err
	}
	defer storeRoot.Close()
	sessionSource, sessionTarget, err := storeRoot.pathPair(tombstone.SessionPath, tombstone.RetiredSessionPath, true)
	if err != nil {
		return false, err
	}
	manifestSource, manifestTarget, err := storeRoot.pathPair(tombstone.ManifestPath, tombstone.RetiredManifestPath, false)
	if err != nil {
		return false, err
	}
	if !sessionSource && !sessionTarget {
		return false, errors.New("managed session deletion has neither source nor quarantined state")
	}
	if !manifestSource && !manifestTarget {
		return false, errors.New("managed session deletion has neither source nor quarantined manifest")
	}
	if sessionSource && manifestTarget {
		identity, err := storeRoot.captureStableRegularFile(tombstone.RetiredManifestPath)
		if err != nil || identity.SHA256 != tombstone.ManifestSHA256 {
			if err == nil {
				err = errors.New("manifest-first deletion quarantine differs from the tombstone")
			}
			return false, err
		}
		if err := storeRoot.rename(tombstone.RetiredManifestPath, tombstone.ManifestPath, false); err != nil {
			return false, fmt.Errorf("restore manifest-first deletion before journal recovery: %w", err)
		}
		manifestSource = true
		manifestTarget = false
	}

	activeSessionPath := tombstone.SessionPath
	if !sessionSource {
		activeSessionPath = tombstone.RetiredSessionPath
	}
	lease, err := storeRoot.acquireWriterLease(filepath.Join(activeSessionPath, "writer.lease"))
	if errors.Is(err, ErrWriterBusy) {
		return false, fmt.Errorf("%w: writer for %s", ErrSessionDeletionBusy, tombstone.SessionID)
	}
	if err != nil {
		return false, fmt.Errorf("acquire deletion writer lease: %w", err)
	}
	leaseOpen := true
	defer func() {
		if leaseOpen {
			_ = unlockWriterFile(lease)
			_ = lease.Close()
		}
	}()

	if sessionSource {
		refreshed, err := refreshSessionDeletionState(root, tombstone, activeSessionPath)
		if err != nil {
			return false, err
		}
		tombstone = refreshed
	} else if err := verifyDeletionState(activeSessionPath, tombstone); err != nil {
		return false, err
	}
	deletionState, _, err := readSessionState(filepath.Join(activeSessionPath, "state.json"))
	if err != nil {
		return false, fmt.Errorf("read deletion snapshot proof: %w", err)
	}
	if sessionSource {
		if err := completePendingNativeRetirementForDeletion(root, activeSessionPath, deletionState); err != nil {
			return false, err
		}
	}
	if err := validateDeletionNativeSnapshotPreflight(root, tombstone, deletionState); err != nil {
		return false, err
	}
	activeReaders, err := sessionDeletionReadersActive(filepath.Join(activeSessionPath, "leases"))
	if err != nil {
		return false, err
	}
	if activeReaders {
		return false, fmt.Errorf("%w: readers for %s", ErrSessionDeletionBusy, tombstone.SessionID)
	}
	manifestPath := tombstone.ManifestPath
	if !manifestSource {
		manifestPath = tombstone.RetiredManifestPath
	}
	manifestIdentity, err := storeRoot.captureStableRegularFile(manifestPath)
	if err != nil {
		return false, fmt.Errorf("verify deletion manifest before quarantine: %w", err)
	}
	if manifestIdentity.SHA256 != tombstone.ManifestSHA256 {
		return false, errors.New("managed session deletion manifest identity changed")
	}

	if err := prepareDeletionQuarantine(tombstone); err != nil {
		return false, err
	}
	if sessionSource {
		if err := storeRoot.rename(tombstone.SessionPath, tombstone.RetiredSessionPath, true); err != nil {
			return false, fmt.Errorf("quarantine deleted session state: %w", err)
		}
	}
	if err := quarantineDeletionNativeSnapshot(root, tombstone, deletionState); err != nil {
		return false, err
	}
	if manifestSource {
		if err := storeRoot.rename(tombstone.ManifestPath, tombstone.RetiredManifestPath, false); err != nil {
			return false, fmt.Errorf("quarantine deleted session manifest: %w", err)
		}
	}
	if err := verifyDeletionState(tombstone.RetiredSessionPath, tombstone); err != nil {
		return false, err
	}
	if identity, err := storeRoot.captureStableRegularFile(tombstone.RetiredManifestPath); err != nil || identity.SHA256 != tombstone.ManifestSHA256 {
		if err == nil {
			err = errors.New("quarantined deletion manifest identity differs")
		}
		return false, err
	}
	if err := lease.Truncate(0); err != nil {
		return false, fmt.Errorf("clear deletion writer lease before purge: %w", err)
	}
	if err := lease.Sync(); err != nil {
		return false, fmt.Errorf("sync cleared deletion writer lease before purge: %w", err)
	}
	if err := unlockWriterFile(lease); err != nil {
		return false, fmt.Errorf("release deletion writer lease before purge: %w", err)
	}
	if err := lease.Close(); err != nil {
		return false, fmt.Errorf("close deletion writer lease before purge: %w", err)
	}
	leaseOpen = false
	return purgeSessionDeletionLocked(root, tombstone)
}

func completePendingNativeRetirementForDeletion(root string, sessionPath string, state SessionState) error {
	if state.NativeSnapshot != (NativeFile{}) {
		return nil
	}
	proofPath := filepath.Join(sessionPath, NativeRetirementFilename)
	proof, err := LoadNativeRetirementProof(proofPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load pending native retirement before session deletion: %w", err)
	}
	if err := ValidateNativeRetirementProofForState(root, state, proof); err != nil {
		return fmt.Errorf("bind pending native retirement before session deletion: %w", err)
	}
	if err := CompleteNativeSnapshotRetirement(root, proof); err != nil {
		return fmt.Errorf("complete pending native retirement before session deletion: %w", err)
	}
	return nil
}

func validateDeletionNativeSnapshotPreflight(root string, tombstone SessionDeletion, state SessionState) error {
	expected := filepath.Join(filepath.Clean(root), "fs", "snapshots", tombstone.SessionID, "native.jsonl")
	if state.NativeSnapshot.Path != "" && filepath.Clean(state.NativeSnapshot.Path) != expected {
		return errors.New("noncanonical native snapshot prevents managed session physical deletion")
	}
	storeRoot, err := openSessionDeletionStoreRoot(root)
	if err != nil {
		return err
	}
	defer storeRoot.Close()
	sourceDirectory := filepath.Dir(expected)
	targetDirectory := filepath.Join(tombstone.RetiredSessionPath, "retained-native", "store-snapshot")
	sourceExists, targetExists, err := storeRoot.pathPair(sourceDirectory, targetDirectory, true)
	if err != nil {
		return err
	}
	if state.NativeSnapshot.Path == "" {
		return nil
	}
	if sourceExists == targetExists {
		if sourceExists {
			return errors.New("native snapshot source and deletion quarantine both exist")
		}
		return errors.New("exact native snapshot is missing from deletion recovery locations")
	}
	directory := sourceDirectory
	if targetExists {
		directory = targetDirectory
	}
	return verifyDeletionNativeSnapshotDirectory(root, directory, state.NativeSnapshot, tombstone.NativeSnapshotSidecar)
}

func quarantineDeletionNativeSnapshot(root string, tombstone SessionDeletion, state SessionState) error {
	storeRoot, err := openSessionDeletionStoreRoot(root)
	if err != nil {
		return err
	}
	defer storeRoot.Close()
	sourceDirectory := filepath.Join(filepath.Clean(root), "fs", "snapshots", tombstone.SessionID)
	targetDirectory := filepath.Join(tombstone.RetiredSessionPath, "retained-native", "store-snapshot")
	sourceExists, targetExists, err := storeRoot.pathPair(sourceDirectory, targetDirectory, true)
	if err != nil {
		return err
	}
	if !sourceExists && !targetExists {
		if state.NativeSnapshot.Path != "" {
			return errors.New("exact native snapshot disappeared before deletion quarantine")
		}
		return nil
	}
	if sourceExists {
		if err := ensurePlainDeletionDirectoryChain(tombstone.RetiredSessionPath, filepath.Dir(targetDirectory)); err != nil {
			return fmt.Errorf("create retained native deletion quarantine: %w", err)
		}
		if err := storeRoot.rename(sourceDirectory, targetDirectory, true); err != nil {
			return fmt.Errorf("quarantine deleted native snapshot tree: %w", err)
		}
	}
	if state.NativeSnapshot.Path != "" {
		if err := verifyDeletionNativeSnapshotDirectory(root, targetDirectory, state.NativeSnapshot, tombstone.NativeSnapshotSidecar); err != nil {
			return err
		}
	}
	return nil
}

func validateDeletionNativeSnapshotAncestors(root string, sessionID string) error {
	storeRoot, _, err := openNativeRetirementStoreRoot(filepath.Clean(root), sessionID)
	if storeRoot != nil {
		err = errors.Join(err, storeRoot.Close())
	}
	return err
}

func verifyDeletionNativeSnapshotDirectory(root string, directory string, expected NativeFile, expectedSidecar NativeFile) error {
	identity, err := captureStableDeletionFileWithinStore(root, filepath.Join(directory, "native.jsonl"))
	if err != nil {
		return fmt.Errorf("verify native snapshot in deletion quarantine: %w", err)
	}
	if identity.Bytes != expected.Bytes || identity.SHA256 != expected.SHA256 {
		return errors.New("native snapshot deletion quarantine differs from session state")
	}
	sidecarPath := filepath.Join(directory, "._native.jsonl")
	storeRoot, err := openSessionDeletionStoreRoot(root)
	if err != nil {
		return err
	}
	defer storeRoot.Close()
	info, err := storeRoot.lstat(sidecarPath)
	if errors.Is(err, os.ErrNotExist) {
		if expectedSidecar.Path != "" {
			return errors.New("proved native snapshot sidecar is missing from deletion quarantine")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("native snapshot sidecar has an unsafe type")
	}
	if expectedSidecar.Path == "" {
		if info.Size() != 0 {
			return errors.New("nonempty native snapshot sidecar has no exact deletion proof")
		}
		return nil
	}
	expectedCanonicalSidecar := filepath.Join(filepath.Dir(filepath.Clean(expected.Path)), "._native.jsonl")
	if filepath.Clean(expectedSidecar.Path) != expectedCanonicalSidecar {
		return errors.New("native snapshot sidecar deletion proof is not canonical")
	}
	sidecar, err := captureStableDeletionFileWithinStore(root, sidecarPath)
	if err != nil {
		return err
	}
	if sidecar.Bytes != expectedSidecar.Bytes || sidecar.SHA256 != expectedSidecar.SHA256 {
		return errors.New("native snapshot sidecar differs from its exact deletion proof")
	}
	return nil
}

func verifyDeletionCheckpointHead(directory string, state SessionState, stateSHA256 string) (stateCheckpointChain, error) {
	chain, err := loadStateCheckpointChain(filepath.Clean(directory), state.SessionID, true)
	if err != nil {
		return stateCheckpointChain{}, err
	}
	if chain.checkpoint.State != state || chain.catalog.Generation != state.Generation || chain.catalog.StateSHA256 != stateSHA256 || chain.checkpoint.StateSHA256 != stateSHA256 {
		return stateCheckpointChain{}, errors.New("managed session primary state is not the exact checkpoint head")
	}
	return chain, nil
}

func verifyDeletionCheckpointDescendant(directory string, tombstone SessionDeletion, state SessionState, stateSHA256 string, verifyArtifacts bool) (stateCheckpointChain, error) {
	chain, err := loadStateCheckpointChain(filepath.Clean(directory), state.SessionID, true)
	if err != nil {
		return stateCheckpointChain{}, err
	}
	if chain.checkpoint.State != state || chain.catalog.Generation != state.Generation || chain.catalog.StateSHA256 != stateSHA256 || chain.checkpoint.StateSHA256 != stateSHA256 {
		return stateCheckpointChain{}, errors.New("managed session primary state is not the exact checkpoint head")
	}
	initialCheckpoint, initialFound := chain.lineage[tombstone.InitialCheckpointSHA256]
	if tombstone.Version < sessionDeletionVersion {
		initialFound = false
		for _, checkpoint := range chain.lineage {
			if checkpoint.State.Generation == tombstone.InitialStateGeneration && checkpoint.StateSHA256 == tombstone.InitialStateSHA256 {
				initialFound = true
				initialCheckpoint = checkpoint
				break
			}
		}
	}
	if !initialFound || (tombstone.Version >= sessionDeletionVersion && initialCheckpoint.Sequence != tombstone.InitialCheckpointSequence) || initialCheckpoint.State.Generation != tombstone.InitialStateGeneration || initialCheckpoint.StateSHA256 != tombstone.InitialStateSHA256 {
		return stateCheckpointChain{}, errors.New("managed session checkpoint lineage does not contain the deletion publication state")
	}
	if verifyArtifacts {
		if err := verifyStateCheckpointArtifactsForChain(directory, chain); err != nil {
			return stateCheckpointChain{}, fmt.Errorf("verify deletion checkpoint artifacts: %w", err)
		}
	}
	return chain, nil
}

// refreshSessionDeletionState advances only the mutable current-state proof.
// The deletion identity (session, route, manifest, operation token, and
// initial state) remains immutable. This permits a writer that was already
// open when unlink was accepted to flush safely before quarantine.
func refreshSessionDeletionState(root string, tombstone SessionDeletion, directory string) (SessionDeletion, error) {
	statePath := filepath.Join(directory, "state.json")
	storeRoot, err := openSessionDeletionStoreRoot(root)
	if err != nil {
		return SessionDeletion{}, err
	}
	defer storeRoot.Close()
	_, manifestErr := storeRoot.lstat(tombstone.ManifestPath)
	manifestAtSource := manifestErr == nil
	var persisted SessionState
	if manifestAtSource {
		persisted, err = RecoverSessionJournalWithWriterLease(context.Background(), statePath)
		if err != nil {
			return SessionDeletion{}, fmt.Errorf("recover deletion state journal: %w", err)
		}
	} else {
		if !errors.Is(manifestErr, os.ErrNotExist) {
			return SessionDeletion{}, manifestErr
		}
		if err := validateDeletionTerminalJournal(directory, tombstone.SessionID); err != nil {
			return SessionDeletion{}, fmt.Errorf("validate deletion journal after manifest quarantine: %w", err)
		}
		persisted, _, err = readSessionState(statePath)
		if err != nil {
			return SessionDeletion{}, err
		}
	}
	state, data, err := readSessionState(statePath)
	if err != nil {
		return SessionDeletion{}, err
	}
	if state != persisted || filepath.Clean(directory) != tombstone.SessionPath {
		return SessionDeletion{}, errors.New("managed session state changed while validating deletion")
	}
	stateSHA256 := digestDeletionBytes(data)
	if _, err := verifyDeletionCheckpointDescendant(directory, tombstone, state, stateSHA256, manifestAtSource); err != nil {
		return SessionDeletion{}, err
	}
	if state.SessionID != tombstone.SessionID || state.Generation < tombstone.InitialStateGeneration || filepath.Clean(state.ManifestPath) != tombstone.ManifestPath || state.ManifestSHA256 != tombstone.ManifestSHA256 {
		return SessionDeletion{}, errors.New("managed session state is not a descendant of the deletion proof")
	}
	if state.Generation < tombstone.StateGeneration {
		return SessionDeletion{}, errors.New("managed session state generation moved backwards after deletion")
	}
	metadata, err := captureSessionDeletionProtocolMetadata(storeRoot, directory)
	if err != nil {
		return SessionDeletion{}, err
	}
	updated := tombstone
	if tombstone.Version >= sessionDeletionVersion {
		updated = applySessionDeletionProtocolMetadata(updated, metadata)
	}
	updated.StateGeneration = state.Generation
	updated.StateSHA256 = stateSHA256
	if updated == tombstone {
		return tombstone, nil
	}
	if err := replaceSessionDeletion(root, updated); err != nil {
		return SessionDeletion{}, err
	}
	return updated, nil
}

func writeSessionDeletion(root string, tombstone SessionDeletion) error {
	return commitSessionDeletion(root, tombstone, false)
}

func replaceSessionDeletion(root string, tombstone SessionDeletion) error {
	return commitSessionDeletion(root, tombstone, true)
}

func commitSessionDeletion(root string, tombstone SessionDeletion, replace bool) error {
	directory := filepath.Dir(SessionDeletionPath(root, tombstone.SessionID))
	if err := ensurePlainDeletionDirectoryChain(root, directory); err != nil {
		return fmt.Errorf("create session deletion directory: %w", err)
	}
	if err := requirePlainDeletionDirectory(directory); err != nil {
		return err
	}
	data, err := json.MarshalIndent(tombstone, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session deletion tombstone: %w", err)
	}
	data = append(data, '\n')
	target := SessionDeletionPath(root, tombstone.SessionID)
	storeRoot, err := openSessionDeletionStoreRoot(root)
	if err != nil {
		return err
	}
	defer storeRoot.Close()
	if err := storeRoot.commitRegularFile(target, ".deletion-", data, replace); err != nil {
		return fmt.Errorf("publish session deletion tombstone: %w", err)
	}
	return nil
}

func validateSessionDeletion(root string, tombstone SessionDeletion) error {
	if root == "" || !filepath.IsAbs(root) || (tombstone.Version != legacySessionDeletionVersion && tombstone.Version != sidecarSessionDeletionVersion && tombstone.Version != sessionDeletionVersion) || tombstone.Kind != sessionDeletionKind || !safeSessionID(tombstone.SessionID) || tombstone.InitialStateGeneration == 0 || tombstone.StateGeneration < tombstone.InitialStateGeneration || !validStateSHA256(tombstone.InitialStateSHA256) || !validStateSHA256(tombstone.StateSHA256) || !validStateSHA256(tombstone.ManifestSHA256) {
		return errors.New("invalid managed session deletion tombstone")
	}
	if _, err := cleanSessionDeletionRoute(tombstone.Route); err != nil {
		return err
	}
	if filepath.Clean(tombstone.ManifestPath) != tombstone.ManifestPath || !pathWithin(filepath.Join(root, "manifests"), tombstone.ManifestPath) || tombstone.ManifestPath == filepath.Join(root, "manifests") {
		return errors.New("session deletion contains an unsafe manifest path")
	}
	if filepath.Clean(tombstone.SessionPath) != filepath.Join(root, "fs", "sessions", tombstone.SessionID) {
		return errors.New("session deletion contains an unsafe state path")
	}
	if len(tombstone.OperationToken) != 32 {
		return errors.New("session deletion contains an invalid operation token")
	}
	if token, err := hex.DecodeString(tombstone.OperationToken); err != nil || len(token) != 16 {
		return errors.New("session deletion contains an invalid operation token")
	}
	if deletedAt, err := time.Parse(time.RFC3339Nano, tombstone.DeletedAt); err != nil || deletedAt.IsZero() {
		return errors.New("session deletion contains an invalid deletion time")
	}
	retiredRoot := filepath.Join(root, "fs", "deleted", tombstone.SessionID, tombstone.OperationToken)
	if filepath.Clean(tombstone.RetiredSessionPath) != filepath.Join(retiredRoot, "session") || filepath.Clean(tombstone.RetiredManifestPath) != filepath.Join(retiredRoot, "manifest.json") {
		return errors.New("session deletion contains unsafe quarantine paths")
	}
	if tombstone.Version == legacySessionDeletionVersion && tombstone.NativeSnapshotSidecar != (NativeFile{}) {
		return errors.New("legacy session deletion cannot bind a native snapshot sidecar")
	}
	if tombstone.NativeSnapshotSidecar != (NativeFile{}) {
		expected := filepath.Join(root, "fs", "snapshots", tombstone.SessionID, "._native.jsonl")
		if filepath.Clean(tombstone.NativeSnapshotSidecar.Path) != expected || tombstone.NativeSnapshotSidecar.Bytes < 0 || !validStateSHA256(tombstone.NativeSnapshotSidecar.SHA256) {
			return errors.New("session deletion contains an invalid native snapshot sidecar proof")
		}
	}
	metadata := []struct {
		name     string
		identity NativeFile
	}{
		{"retire.request.json", tombstone.RetirementRequest},
		{"retire.ack.json", tombstone.RetirementAcknowledgement},
		{"mounted.json", tombstone.MountAcknowledgement},
		{"journal.jsonl", tombstone.Journal},
		{NativeRetirementFilename, tombstone.NativeRetirementProof},
	}
	if tombstone.Version < sessionDeletionVersion {
		if tombstone.InitialCheckpointSequence != 0 || tombstone.InitialCheckpointSHA256 != "" {
			return errors.New("legacy session deletion cannot bind an initial checkpoint")
		}
		for _, item := range metadata {
			if item.identity != (NativeFile{}) {
				return errors.New("legacy session deletion cannot bind protocol metadata")
			}
		}
	} else if tombstone.InitialCheckpointSequence == 0 || !validStateSHA256(tombstone.InitialCheckpointSHA256) {
		return errors.New("session deletion contains an invalid initial checkpoint identity")
	}
	for _, item := range metadata {
		if item.identity == (NativeFile{}) {
			continue
		}
		expected := filepath.Join(tombstone.SessionPath, item.name)
		if filepath.Clean(item.identity.Path) != expected || item.identity.Bytes < 0 || !validStateSHA256(item.identity.SHA256) {
			return fmt.Errorf("session deletion contains an invalid %s identity", item.name)
		}
	}
	return nil
}

func cleanSessionDeletionRoute(route string) (string, error) {
	if route == "" || strings.ContainsRune(route, '\x00') {
		return "", errors.New("canonical session deletion route is required")
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(route, "/"))
	if cleaned != route || !strings.HasSuffix(cleaned, ".jsonl") || (!strings.HasPrefix(cleaned, "/sessions/") && !strings.HasPrefix(cleaned, "/archived_sessions/")) {
		return "", errors.New("session deletion route is not a clean canonical session path")
	}
	return cleaned, nil
}

func decodeSessionDeletion(data []byte, destination *SessionDeletion) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("session deletion tombstone contains multiple JSON values")
		}
		return err
	}
	return nil
}

func digestDeletionBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func hashRegularDeletionFile(path string) (string, error) {
	identity, err := captureStableDeletionPurgeFile(path)
	if err != nil {
		return "", err
	}
	return identity.SHA256, nil
}

func deletionPathPair(source string, target string, directory bool) (bool, bool, error) {
	sourceInfo, sourceErr := os.Lstat(source)
	targetInfo, targetErr := os.Lstat(target)
	sourceExists := sourceErr == nil
	targetExists := targetErr == nil
	if sourceErr != nil && !errors.Is(sourceErr, os.ErrNotExist) {
		return false, false, sourceErr
	}
	if targetErr != nil && !errors.Is(targetErr, os.ErrNotExist) {
		return false, false, targetErr
	}
	if sourceExists && targetExists {
		return false, false, errors.New("session deletion source and quarantine target both exist")
	}
	for _, item := range []struct {
		exists bool
		info   os.FileInfo
	}{
		{sourceExists, sourceInfo}, {targetExists, targetInfo},
	} {
		if !item.exists {
			continue
		}
		if item.info.Mode()&os.ModeSymlink != 0 || item.info.IsDir() != directory || (!directory && !item.info.Mode().IsRegular()) {
			return false, false, errors.New("session deletion source or quarantine target has an unsafe type")
		}
	}
	return sourceExists, targetExists, nil
}

func verifyDeletionState(directory string, tombstone SessionDeletion) error {
	statePath := filepath.Join(directory, "state.json")
	state, data, err := readSessionState(statePath)
	if err != nil {
		return fmt.Errorf("read deletion state: %w", err)
	}
	if err := validateSessionStateForDirectory(state, tombstone.SessionPath); err != nil {
		return fmt.Errorf("validate deletion state: %w", err)
	}
	if digestDeletionBytes(data) != tombstone.StateSHA256 || state.SessionID != tombstone.SessionID || state.Generation != tombstone.StateGeneration || filepath.Clean(state.ManifestPath) != tombstone.ManifestPath || state.ManifestSHA256 != tombstone.ManifestSHA256 {
		return errors.New("managed session state differs from the deletion tombstone")
	}
	return nil
}

func sessionDeletionReadersActive(root string) (bool, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return false, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("unrecognized reader lease entry: %s", entry.Name())
		}
		active, err := storage.DirectoryHasActiveLease(filepath.Join(root, entry.Name()), true)
		if err != nil {
			return false, err
		}
		if active {
			return true, nil
		}
	}
	return false, nil
}

func prepareDeletionQuarantine(tombstone SessionDeletion) error {
	directory := filepath.Dir(tombstone.RetiredSessionPath)
	root := filepath.Dir(filepath.Dir(filepath.Dir(tombstone.SessionPath)))
	if err := ensurePlainDeletionDirectoryChain(root, directory); err != nil {
		return fmt.Errorf("create deleted session quarantine: %w", err)
	}
	if err := requirePlainDeletionDirectory(directory); err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(tombstone.RetiredSessionPath) && entry.Name() != filepath.Base(tombstone.RetiredManifestPath) {
			return fmt.Errorf("unrecognized nonempty deletion quarantine entry: %s", entry.Name())
		}
	}
	return syncStateDirectory(directory)
}

func requirePlainDeletionDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("session deletion directory is not a plain directory")
	}
	return nil
}

func syncDeletionRenameParents(source string, target string) error {
	sourceParent := filepath.Dir(source)
	targetParent := filepath.Dir(target)
	if err := syncStateDirectory(sourceParent); err != nil {
		return fmt.Errorf("sync deletion source parent: %w", err)
	}
	if sourceParent != targetParent {
		if err := syncStateDirectory(targetParent); err != nil {
			return fmt.Errorf("sync deletion quarantine parent: %w", err)
		}
	}
	return nil
}

func syncDeletionDirectoryChain(root string, directory string) error {
	root = filepath.Clean(root)
	directory = filepath.Clean(directory)
	relative, err := filepath.Rel(root, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("session deletion directory escapes the store root")
	}
	if err := syncStateDirectory(root); err != nil {
		return fmt.Errorf("sync deletion store root: %w", err)
	}
	if relative == "." {
		return nil
	}
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("session deletion directory chain contains an unsafe component: %s", current)
		}
		if err := syncStateDirectory(current); err != nil {
			return fmt.Errorf("sync deletion directory chain: %w", err)
		}
	}
	return nil
}

func ensurePlainDeletionDirectoryChain(root string, directory string) error {
	root = filepath.Clean(root)
	directory = filepath.Clean(directory)
	relative, err := filepath.Rel(root, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("session deletion directory escapes the store root")
	}
	storeRoot, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer storeRoot.Close()
	current := ""
	if relative != "." {
		for _, part := range strings.Split(relative, string(filepath.Separator)) {
			if part == "" || part == "." {
				continue
			}
			current = filepath.Join(current, part)
			if err := storeRoot.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err := storeRoot.Lstat(current)
			if err != nil {
				return err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("session deletion directory chain contains an unsafe component: %s", filepath.Join(root, current))
			}
		}
	}
	return syncDeletionDirectoryChain(root, directory)
}

package vfs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"
)

const (
	legacyNativeRetirementVersion = 1
	nativeRetirementVersion       = 2

	nativeRetirementDeletionVersion = 1
	nativeRetirementDeletionKind    = "codexfold-native-snapshot-deletion"
	nativeRetirementDeletable       = "deletable"

	nativeRetirementPhaseVerified        = "verified"
	nativeRetirementPhaseStaged          = "staged"
	nativeRetirementPhaseDeletable       = "deletable"
	nativeRetirementPhaseSnapshotRemoved = "snapshot-removed"

	maxNativeRetirementMarkerBytes = 64 << 10
)
const NativeRetirementFilename = "native-retirement.json"

type NativeRetirementProof struct {
	Version         int         `json:"version"`
	SessionID       string      `json:"session_id"`
	StateGeneration uint64      `json:"state_generation"`
	RetiredAt       string      `json:"retired_at"`
	Snapshot        NativeFile  `json:"snapshot"`
	Sidecar         *NativeFile `json:"sidecar,omitempty"`
	Visible         NativeFile  `json:"visible"`
}

// nativeRetirementDeletion is the durable authority for deleting only the
// exact files already moved out of the canonical snapshot namespace. The
// marker is deliberately published after the directory rename: a crash before
// publication can replay from the deterministic staging path and the durable
// NativeRetirementProof, while a replacement at the canonical path is never
// considered owned by this deletion.
type nativeRetirementDeletion struct {
	Version         int         `json:"version"`
	Kind            string      `json:"kind"`
	SessionID       string      `json:"session_id"`
	ProofSHA256     string      `json:"proof_sha256"`
	Snapshot        NativeFile  `json:"snapshot"`
	Sidecar         *NativeFile `json:"sidecar,omitempty"`
	StagingRelative string      `json:"staging_relative"`
	Phase           string      `json:"phase"`
	StagedAt        string      `json:"staged_at"`
}

var nativeRetirementHook func(string) error

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
	sidecar, err := captureNativeRetirementSidecar(snapshot)
	if err != nil {
		return NativeRetirementProof{}, err
	}
	proof := NativeRetirementProof{
		Version: nativeRetirementVersion, SessionID: s.state.SessionID,
		StateGeneration: s.state.Generation, RetiredAt: time.Now().UTC().Format(time.RFC3339Nano),
		Snapshot: snapshot, Sidecar: sidecar, Visible: visible,
	}
	proofPath := filepath.Join(s.directory, NativeRetirementFilename)
	if err := writeNativeRetirementProof(proofPath, proof); err != nil {
		return NativeRetirementProof{}, err
	}
	next := s.state
	next.Generation++
	next.NativeSnapshot = NativeFile{}
	if err := publishSessionState(s.statePath, next); err != nil {
		// Publication can fail after the new state becomes durable. Keep the
		// prepared proof so replay can distinguish that case; an equal-generation
		// state still cannot use it as authority for a missing snapshot.
		return proof, err
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
	persisted, err := LoadSessionState(s.statePath)
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
		before.ManifestSHA256 == after.ManifestSHA256 &&
		before.BaseBytes == after.BaseBytes &&
		before.BaseSHA256 == after.BaseSHA256 &&
		before.DeltaPath == after.DeltaPath &&
		before.BackingPath == after.BackingPath
}

func CompleteNativeSnapshotRetirement(storeRoot string, proof NativeRetirementProof) error {
	storeRoot = filepath.Clean(storeRoot)
	if !filepath.IsAbs(storeRoot) {
		return errors.New("absolute store root is required")
	}
	if err := validateNativeRetirementProof(proof); err != nil {
		return err
	}
	expectedSnapshot := filepath.Join(storeRoot, "fs", "snapshots", proof.SessionID, "native.jsonl")
	if filepath.Clean(proof.Snapshot.Path) != filepath.Clean(expectedSnapshot) {
		return errors.New("native retirement proof does not reference the canonical snapshot")
	}
	expectedSidecar := filepath.Join(filepath.Dir(expectedSnapshot), "._native.jsonl")
	if proof.Sidecar != nil && filepath.Clean(proof.Sidecar.Path) != filepath.Clean(expectedSidecar) {
		return errors.New("native retirement proof does not reference the canonical snapshot sidecar")
	}
	proofSHA256, err := nativeRetirementProofDigest(proof)
	if err != nil {
		return err
	}
	snapshotRelative := filepath.Join("fs", "snapshots", proof.SessionID, "native.jsonl")
	sourceDirectoryRelative := filepath.Dir(snapshotRelative)
	stagingDirectoryRelative := filepath.Join("fs", "native-retirement-staging", proof.SessionID, proofSHA256)
	markerRelative := filepath.Join("fs", "native-retirements", proof.SessionID, proofSHA256+".json")

	root, _, err := openNativeRetirementStoreRoot(storeRoot, proof.SessionID)
	if err != nil {
		return err
	}
	defer root.Close()

	if _, err := validateExistingNativeRetirementDirectoryChain(root, filepath.Dir(markerRelative)); err != nil {
		return fmt.Errorf("validate native retirement marker path: %w", err)
	}
	if _, err := validateExistingNativeRetirementDirectoryChain(root, filepath.Dir(stagingDirectoryRelative)); err != nil {
		return fmt.Errorf("validate native retirement staging path: %w", err)
	}

	marker, markerExists, err := loadNativeRetirementDeletion(root, markerRelative, proof, proofSHA256, stagingDirectoryRelative)
	if err != nil {
		return err
	}
	stagingExists, err := nativeRetirementDirectoryExists(root, stagingDirectoryRelative)
	if err != nil {
		return fmt.Errorf("inspect native retirement staging: %w", err)
	}

	if markerExists {
		if marker.Phase != nativeRetirementDeletable {
			return errors.New("native retirement deletion marker is not deletable")
		}
		if !stagingExists {
			return nil
		}
		return deleteStagedNativeRetirement(root, stagingDirectoryRelative, proof)
	}

	if stagingExists {
		if _, _, err := inspectNativeRetirementDirectory(root, stagingDirectoryRelative, proof, true); err != nil {
			return fmt.Errorf("verify interrupted native retirement staging: %w", err)
		}
	} else {
		sourceExists, err := nativeRetirementDirectoryExists(root, sourceDirectoryRelative)
		if err != nil {
			return fmt.Errorf("inspect retired native snapshot directory: %w", err)
		}
		if !sourceExists {
			// Compatibility with retirements completed by the pre-staging
			// implementation: no pathname is used as deletion authority when
			// both the canonical source and deterministic staging are absent.
			return nil
		}
		if _, _, err := inspectNativeRetirementDirectory(root, sourceDirectoryRelative, proof, true); err != nil {
			return fmt.Errorf("verify retired native snapshot before staging: %w", err)
		}
		if err := ensureNativeRetirementDirectoryChain(root, filepath.Dir(stagingDirectoryRelative)); err != nil {
			return fmt.Errorf("create native retirement staging parent: %w", err)
		}
		if err := runNativeRetirementHook(nativeRetirementPhaseVerified); err != nil {
			return err
		}
		// Rehash after the test seam and immediately before the rename. Inode,
		// size, and mtime equality alone never authorizes deletion.
		if _, _, err := inspectNativeRetirementDirectory(root, sourceDirectoryRelative, proof, true); err != nil {
			return fmt.Errorf("reverify retired native snapshot before staging: %w", err)
		}
		if exists, err := validateExistingNativeRetirementDirectoryChain(root, filepath.Dir(stagingDirectoryRelative)); err != nil {
			return fmt.Errorf("revalidate native retirement staging parent: %w", err)
		} else if !exists {
			return errors.New("native retirement staging parent disappeared before rename")
		}
		if exists, err := nativeRetirementPathExists(root, stagingDirectoryRelative); err != nil {
			return err
		} else if exists {
			return errors.New("native retirement staging appeared before rename")
		}
		if err := root.Rename(sourceDirectoryRelative, stagingDirectoryRelative); err != nil {
			return fmt.Errorf("move exact native snapshot into retirement staging: %w", err)
		}
		if err := syncNativeRetirementDirectory(root, filepath.Dir(sourceDirectoryRelative)); err != nil {
			return fmt.Errorf("sync native snapshot parent after staging: %w", err)
		}
		if err := syncNativeRetirementDirectory(root, filepath.Dir(stagingDirectoryRelative)); err != nil {
			return fmt.Errorf("sync native retirement staging parent: %w", err)
		}
		if _, _, err := inspectNativeRetirementDirectory(root, stagingDirectoryRelative, proof, true); err != nil {
			return fmt.Errorf("verify staged native retirement: %w", err)
		}
		stagingExists = true
	}

	if err := runNativeRetirementHook(nativeRetirementPhaseStaged); err != nil {
		return err
	}
	if _, _, err := inspectNativeRetirementDirectory(root, stagingDirectoryRelative, proof, true); err != nil {
		return fmt.Errorf("reverify native retirement staging before durable marker: %w", err)
	}
	if err := ensureNativeRetirementDirectoryChain(root, filepath.Dir(markerRelative)); err != nil {
		return fmt.Errorf("create native retirement marker parent: %w", err)
	}
	marker = nativeRetirementDeletion{
		Version: nativeRetirementDeletionVersion, Kind: nativeRetirementDeletionKind,
		SessionID: proof.SessionID, ProofSHA256: proofSHA256,
		Snapshot: proof.Snapshot, Sidecar: cloneNativeRetirementFile(proof.Sidecar),
		StagingRelative: stagingDirectoryRelative, Phase: nativeRetirementDeletable,
		StagedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeNativeRetirementDeletion(root, markerRelative, marker, proof); err != nil {
		return err
	}
	if err := runNativeRetirementHook(nativeRetirementPhaseDeletable); err != nil {
		return err
	}
	if !stagingExists {
		return errors.New("native retirement staging disappeared before durable deletion")
	}
	return deleteStagedNativeRetirement(root, stagingDirectoryRelative, proof)
}

func nativeRetirementProofDigest(proof NativeRetirementProof) (string, error) {
	data, err := json.Marshal(proof)
	if err != nil {
		return "", fmt.Errorf("encode native retirement proof identity: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func cloneNativeRetirementFile(file *NativeFile) *NativeFile {
	if file == nil {
		return nil
	}
	cloned := *file
	return &cloned
}

func sameNativeRetirementFile(left *NativeFile, right *NativeFile) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func runNativeRetirementHook(phase string) error {
	if nativeRetirementHook == nil {
		return nil
	}
	return nativeRetirementHook(phase)
}

func nativeRetirementPathExists(root *os.Root, relative string) (bool, error) {
	_, err := root.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func nativeRetirementDirectoryExists(root *os.Root, relative string) (bool, error) {
	info, err := root.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("native retirement path is not a plain directory")
	}
	return true, nil
}

// validateExistingNativeRetirementDirectoryChain rejects every symlink and
// non-directory component that already exists. Missing suffixes are reported
// as exists=false without being created.
func validateExistingNativeRetirementDirectoryChain(root *os.Root, relative string) (bool, error) {
	components, err := nativeRetirementPathComponents(relative)
	if err != nil {
		return false, err
	}
	current := ""
	for _, component := range components {
		current = filepath.Join(current, component)
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("native retirement path contains an unsafe directory: %s", current)
		}
	}
	return true, nil
}

func ensureNativeRetirementDirectoryChain(root *os.Root, relative string) error {
	components, err := nativeRetirementPathComponents(relative)
	if err != nil {
		return err
	}
	current := ""
	for _, component := range components {
		current = filepath.Join(current, component)
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := root.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = root.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("native retirement path contains an unsafe directory: %s", current)
		}
	}
	return nil
}

func nativeRetirementPathComponents(relative string) ([]string, error) {
	clean := filepath.Clean(relative)
	if clean == "." || filepath.IsAbs(clean) {
		return nil, errors.New("native retirement path must be a nonempty relative path")
	}
	components := make([]string, 0)
	for current := clean; current != "."; {
		directory, base := filepath.Split(current)
		if base == "" || base == "." || base == ".." {
			return nil, errors.New("native retirement path contains an unsafe component")
		}
		components = append(components, base)
		current = filepath.Clean(directory)
	}
	slices.Reverse(components)
	return components, nil
}

func syncNativeRetirementDirectory(root *os.Root, relative string) error {
	if exists, err := validateExistingNativeRetirementDirectoryChain(root, relative); err != nil {
		return err
	} else if !exists {
		return errors.New("native retirement sync directory does not exist")
	}
	directory, err := root.Open(relative)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func inspectNativeRetirementDirectory(root *os.Root, relative string, proof NativeRetirementProof, requireAll bool) (bool, bool, error) {
	firstNames, err := readStableNativeRetirementDirectory(root, relative)
	if err != nil {
		return false, false, err
	}
	var snapshotExists bool
	var sidecarExists bool
	for _, name := range firstNames {
		switch name {
		case "native.jsonl":
			snapshotExists = true
		case "._native.jsonl":
			if proof.Sidecar == nil {
				return false, false, errors.New("native snapshot sidecar has no exact retirement proof")
			}
			sidecarExists = true
		default:
			return false, false, fmt.Errorf("unrecognized native retirement content: %s", name)
		}
	}
	if requireAll && !snapshotExists {
		return false, false, errors.New("proved native snapshot is missing")
	}
	if requireAll && proof.Sidecar != nil && !sidecarExists {
		return false, false, errors.New("proved native snapshot sidecar is missing")
	}
	if snapshotExists {
		identity, err := captureNativeRetirementFile(root, filepath.Join(relative, "native.jsonl"), proof.Snapshot.Path)
		if err != nil {
			return false, false, err
		}
		if identity.Bytes != proof.Snapshot.Bytes || identity.SHA256 != proof.Snapshot.SHA256 {
			return false, false, errors.New("retired native snapshot differs from its exact retirement proof")
		}
	}
	if sidecarExists {
		identity, err := captureNativeRetirementFile(root, filepath.Join(relative, "._native.jsonl"), proof.Sidecar.Path)
		if err != nil {
			return false, false, err
		}
		if identity.Bytes != proof.Sidecar.Bytes || identity.SHA256 != proof.Sidecar.SHA256 {
			return false, false, errors.New("native snapshot sidecar differs from its exact retirement proof")
		}
	}
	secondNames, err := readStableNativeRetirementDirectory(root, relative)
	if err != nil {
		return false, false, err
	}
	if !slices.Equal(firstNames, secondNames) {
		return false, false, errors.New("native retirement directory changed while it was verified")
	}
	return snapshotExists, sidecarExists, nil
}

func readStableNativeRetirementDirectory(root *os.Root, relative string) ([]string, error) {
	before, err := root.Lstat(relative)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("native retirement directory is not a plain directory")
	}
	directory, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	openedBefore, err := directory.Stat()
	if err != nil || !openedBefore.IsDir() || !os.SameFile(before, openedBefore) {
		_ = directory.Close()
		if err == nil {
			err = errors.New("native retirement directory changed while opening")
		}
		return nil, err
	}
	entries, readErr := directory.ReadDir(-1)
	openedAfter, statErr := directory.Stat()
	closeErr := directory.Close()
	after, lstatErr := root.Lstat(relative)
	if err := errors.Join(readErr, statErr, closeErr, lstatErr); err != nil {
		return nil, err
	}
	if !sameNativeRetirementFileInfo(before, openedBefore) || !sameNativeRetirementFileInfo(openedBefore, openedAfter) || !sameNativeRetirementFileInfo(openedAfter, after) {
		return nil, errors.New("native retirement directory changed while reading")
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

func captureNativeRetirementFile(root *os.Root, relative string, proofPath string) (NativeFile, error) {
	before, err := root.Lstat(relative)
	if err != nil {
		return NativeFile{}, err
	}
	if !before.Mode().IsRegular() {
		return NativeFile{}, errors.New("native retirement entry is not a regular file")
	}
	file, err := root.Open(relative)
	if err != nil {
		return NativeFile{}, err
	}
	openedBefore, err := file.Stat()
	if err != nil || !openedBefore.Mode().IsRegular() || !os.SameFile(before, openedBefore) {
		_ = file.Close()
		if err == nil {
			err = errors.New("native retirement entry changed while opening")
		}
		return NativeFile{}, err
	}
	hasher := sha256.New()
	bytesRead, copyErr := io.Copy(hasher, file)
	openedAfter, statErr := file.Stat()
	closeErr := file.Close()
	after, lstatErr := root.Lstat(relative)
	if err := errors.Join(copyErr, statErr, closeErr, lstatErr); err != nil {
		return NativeFile{}, err
	}
	if !sameNativeRetirementFileInfo(before, openedBefore) || !sameNativeRetirementFileInfo(openedBefore, openedAfter) || !sameNativeRetirementFileInfo(openedAfter, after) || bytesRead != after.Size() {
		return NativeFile{}, errors.New("native retirement entry changed while hashing")
	}
	return NativeFile{Path: filepath.Clean(proofPath), Bytes: bytesRead, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

func sameNativeRetirementFileInfo(left os.FileInfo, right os.FileInfo) bool {
	return left != nil && right != nil && os.SameFile(left, right) && left.Mode() == right.Mode() && left.Size() == right.Size() && left.ModTime().Equal(right.ModTime())
}

func loadNativeRetirementDeletion(root *os.Root, markerRelative string, proof NativeRetirementProof, proofSHA256 string, stagingRelative string) (nativeRetirementDeletion, bool, error) {
	data, exists, err := readNativeRetirementMarker(root, markerRelative)
	if err != nil || !exists {
		return nativeRetirementDeletion{}, exists, err
	}
	var marker nativeRetirementDeletion
	if err := decodeStrictJSON(data, &marker); err != nil {
		return nativeRetirementDeletion{}, false, fmt.Errorf("decode native retirement deletion marker: %w", err)
	}
	if err := validateNativeRetirementDeletion(marker, proof, proofSHA256, stagingRelative); err != nil {
		return nativeRetirementDeletion{}, false, err
	}
	return marker, true, nil
}

func readNativeRetirementMarker(root *os.Root, relative string) ([]byte, bool, error) {
	before, err := root.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !before.Mode().IsRegular() {
		return nil, false, errors.New("native retirement deletion marker is not a regular file")
	}
	file, err := root.Open(relative)
	if err != nil {
		return nil, false, err
	}
	openedBefore, err := file.Stat()
	if err != nil || !openedBefore.Mode().IsRegular() || !os.SameFile(before, openedBefore) {
		_ = file.Close()
		if err == nil {
			err = errors.New("native retirement deletion marker changed while opening")
		}
		return nil, false, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxNativeRetirementMarkerBytes+1))
	openedAfter, statErr := file.Stat()
	closeErr := file.Close()
	after, lstatErr := root.Lstat(relative)
	if err := errors.Join(readErr, statErr, closeErr, lstatErr); err != nil {
		return nil, false, err
	}
	if len(data) > maxNativeRetirementMarkerBytes {
		return nil, false, errors.New("native retirement deletion marker is too large")
	}
	if !sameNativeRetirementFileInfo(before, openedBefore) || !sameNativeRetirementFileInfo(openedBefore, openedAfter) || !sameNativeRetirementFileInfo(openedAfter, after) || int64(len(data)) != after.Size() {
		return nil, false, errors.New("native retirement deletion marker changed while reading")
	}
	return data, true, nil
}

func validateNativeRetirementDeletion(marker nativeRetirementDeletion, proof NativeRetirementProof, proofSHA256 string, stagingRelative string) error {
	if marker.Version != nativeRetirementDeletionVersion || marker.Kind != nativeRetirementDeletionKind || marker.SessionID != proof.SessionID || marker.ProofSHA256 != proofSHA256 || marker.Snapshot != proof.Snapshot || !sameNativeRetirementFile(marker.Sidecar, proof.Sidecar) || filepath.Clean(marker.StagingRelative) != filepath.Clean(stagingRelative) || marker.Phase != nativeRetirementDeletable {
		return errors.New("native retirement deletion marker does not match the exact retirement proof")
	}
	if !validStateSHA256(marker.ProofSHA256) {
		return errors.New("native retirement deletion marker has an invalid proof identity")
	}
	if stagedAt, err := time.Parse(time.RFC3339Nano, marker.StagedAt); err != nil || stagedAt.IsZero() {
		return errors.New("native retirement deletion marker has an invalid staging time")
	}
	return nil
}

func writeNativeRetirementDeletion(root *os.Root, markerRelative string, marker nativeRetirementDeletion, proof NativeRetirementProof) error {
	if err := validateNativeRetirementDeletion(marker, proof, marker.ProofSHA256, marker.StagingRelative); err != nil {
		return err
	}
	if _, exists, err := loadNativeRetirementDeletion(root, markerRelative, proof, marker.ProofSHA256, marker.StagingRelative); err != nil {
		return err
	} else if exists {
		return nil
	}
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return fmt.Errorf("create native retirement marker temporary name: %w", err)
	}
	temporaryRelative := markerRelative + ".tmp-" + hex.EncodeToString(random)
	temporary, err := root.OpenFile(temporaryRelative, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = root.Remove(temporaryRelative)
		}
	}()
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
	if err := root.Link(temporaryRelative, markerRelative); err != nil {
		if _, exists, loadErr := loadNativeRetirementDeletion(root, markerRelative, proof, marker.ProofSHA256, marker.StagingRelative); loadErr == nil && exists {
			return nil
		}
		return fmt.Errorf("publish native retirement deletion marker: %w", err)
	}
	if err := root.Remove(temporaryRelative); err != nil {
		return fmt.Errorf("remove native retirement marker temporary: %w", err)
	}
	removeTemporary = false
	if err := syncNativeRetirementDirectory(root, filepath.Dir(markerRelative)); err != nil {
		return fmt.Errorf("sync native retirement marker directory: %w", err)
	}
	if _, exists, err := loadNativeRetirementDeletion(root, markerRelative, proof, marker.ProofSHA256, marker.StagingRelative); err != nil || !exists {
		if err == nil {
			err = errors.New("native retirement deletion marker disappeared after publication")
		}
		return err
	}
	return nil
}

func deleteStagedNativeRetirement(root *os.Root, stagingRelative string, proof NativeRetirementProof) error {
	snapshotExists, sidecarExists, err := inspectNativeRetirementDirectory(root, stagingRelative, proof, false)
	if err != nil {
		return fmt.Errorf("verify durable native retirement staging: %w", err)
	}
	if snapshotExists {
		if _, err := captureNativeRetirementFile(root, filepath.Join(stagingRelative, "native.jsonl"), proof.Snapshot.Path); err != nil {
			return fmt.Errorf("reverify staged native snapshot: %w", err)
		}
		if err := root.Remove(filepath.Join(stagingRelative, "native.jsonl")); err != nil {
			return fmt.Errorf("remove staged native snapshot: %w", err)
		}
		if err := syncNativeRetirementDirectory(root, stagingRelative); err != nil {
			return fmt.Errorf("sync staged native snapshot deletion: %w", err)
		}
		if err := runNativeRetirementHook(nativeRetirementPhaseSnapshotRemoved); err != nil {
			return err
		}
	}
	if sidecarExists {
		if _, err := captureNativeRetirementFile(root, filepath.Join(stagingRelative, "._native.jsonl"), proof.Sidecar.Path); err != nil {
			return fmt.Errorf("reverify staged native snapshot sidecar: %w", err)
		}
		if err := root.Remove(filepath.Join(stagingRelative, "._native.jsonl")); err != nil {
			return fmt.Errorf("remove staged native snapshot sidecar: %w", err)
		}
		if err := syncNativeRetirementDirectory(root, stagingRelative); err != nil {
			return fmt.Errorf("sync staged native snapshot sidecar deletion: %w", err)
		}
	}
	remaining, err := readStableNativeRetirementDirectory(root, stagingRelative)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return fmt.Errorf("native retirement staging gained replacement content: %s", remaining[0])
	}
	info, err := root.Lstat(stagingRelative)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("native retirement staging changed before directory removal")
		}
		return err
	}
	if err := root.Remove(stagingRelative); err != nil {
		return fmt.Errorf("remove empty native retirement staging directory: %w", err)
	}
	if err := syncNativeRetirementDirectory(root, filepath.Dir(stagingRelative)); err != nil {
		return fmt.Errorf("sync native retirement staging completion: %w", err)
	}
	return nil
}

func captureNativeRetirementSidecar(snapshot NativeFile) (*NativeFile, error) {
	path := filepath.Join(filepath.Dir(snapshot.Path), "._"+filepath.Base(snapshot.Path))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("native snapshot sidecar is not a regular file")
	}
	identity, err := captureStableDeletionPurgeFile(path)
	if err != nil {
		return nil, err
	}
	return &identity, nil
}

func openNativeRetirementStoreRoot(storeRoot string, sessionID string) (*os.Root, bool, error) {
	storeInfo, err := os.Lstat(storeRoot)
	if err != nil {
		return nil, false, err
	}
	if !storeInfo.IsDir() || storeInfo.Mode()&os.ModeSymlink != 0 {
		return nil, false, errors.New("native retirement store root is not a plain directory")
	}
	root, err := os.OpenRoot(storeRoot)
	if err != nil {
		return nil, false, err
	}
	openedInfo, err := root.Stat(".")
	if err != nil || !openedInfo.IsDir() || !os.SameFile(storeInfo, openedInfo) {
		_ = root.Close()
		if err == nil {
			err = errors.New("native retirement store root changed while opening")
		}
		return nil, false, err
	}
	currentStoreInfo, err := os.Lstat(storeRoot)
	if err != nil || !sameNativeRetirementFileInfo(storeInfo, currentStoreInfo) {
		_ = root.Close()
		if err == nil {
			err = errors.New("native retirement store root changed while opening")
		}
		return nil, false, err
	}
	exists, err := validateNativeRetirementAncestors(root, storeRoot, sessionID)
	if err != nil {
		_ = root.Close()
		return nil, false, err
	}
	return root, exists, nil
}

func validateNativeRetirementAncestors(root *os.Root, storeRoot string, sessionID string) (bool, error) {
	for _, relative := range []string{
		"fs",
		filepath.Join("fs", "snapshots"),
		filepath.Join("fs", "snapshots", sessionID),
	} {
		info, err := root.Lstat(relative)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("native retirement path contains an unsafe directory: %s", filepath.Join(storeRoot, relative))
		}
	}
	return true, nil
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
	if filepath.Clean(state.NativeSnapshot.Path) != expectedSnapshot || proof.SessionID != state.SessionID || proof.Snapshot != state.NativeSnapshot || proof.StateGeneration >= state.Generation {
		return false, errors.New("missing native snapshot does not match its retirement proof")
	}
	return true, nil
}

// ValidateNativeRetirementProofForState binds an already-retired proof to the
// current durable checkpoint lineage. Generation numbers and a canonical path
// alone are not authority to delete a restored snapshot.
func ValidateNativeRetirementProofForState(storeRoot string, state SessionState, proof NativeRetirementProof) error {
	storeRoot = filepath.Clean(storeRoot)
	if !filepath.IsAbs(storeRoot) || state.NativeSnapshot != (NativeFile{}) {
		return errors.New("already-retired managed state and an absolute store root are required")
	}
	if err := validateNativeRetirementProof(proof); err != nil {
		return err
	}
	expectedDirectory := filepath.Join(storeRoot, "fs", "sessions", state.SessionID)
	if err := validateSessionStateForDirectory(state, expectedDirectory); err != nil {
		return err
	}
	expectedSnapshot := filepath.Join(storeRoot, "fs", "snapshots", state.SessionID, "native.jsonl")
	if proof.SessionID != state.SessionID || proof.StateGeneration >= state.Generation || filepath.Clean(proof.Snapshot.Path) != expectedSnapshot {
		return errors.New("native retirement proof does not match the current managed state")
	}
	statePath := filepath.Join(expectedDirectory, "state.json")
	persisted, data, err := readSessionState(statePath)
	if err != nil {
		return err
	}
	chain, err := loadStateCheckpointChain(expectedDirectory, state.SessionID, true)
	if err != nil {
		return err
	}
	stateSHA256 := digestStateBytes(data)
	if persisted != state || chain.checkpoint.State != state || chain.catalog.StateSHA256 != stateSHA256 || chain.checkpoint.StateSHA256 != stateSHA256 {
		return errors.New("current managed state is not the exact retirement checkpoint head")
	}
	for _, checkpoint := range chain.lineage {
		if checkpoint.State.Generation == proof.StateGeneration && checkpoint.NativeSnapshot != nil && checkpoint.NativeSnapshot.Path == proof.Snapshot.Path && checkpoint.NativeSnapshot.Bytes == proof.Snapshot.Bytes && checkpoint.NativeSnapshot.SHA256 == proof.Snapshot.SHA256 {
			return nil
		}
	}
	return errors.New("native retirement proof snapshot is absent from the managed checkpoint lineage")
}

func LoadNativeRetirementProof(path string) (NativeRetirementProof, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return NativeRetirementProof{}, err
	}
	if !info.Mode().IsRegular() {
		return NativeRetirementProof{}, errors.New("native retirement proof is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return NativeRetirementProof{}, err
	}
	var proof NativeRetirementProof
	if err := decodeStrictJSON(data, &proof); err != nil {
		return NativeRetirementProof{}, fmt.Errorf("decode native retirement proof: %w", err)
	}
	if err := validateNativeRetirementProof(proof); err != nil {
		return NativeRetirementProof{}, err
	}
	return proof, nil
}

func validateNativeRetirementProof(proof NativeRetirementProof) error {
	if (proof.Version != legacyNativeRetirementVersion && proof.Version != nativeRetirementVersion) || !safeSessionID(proof.SessionID) || proof.StateGeneration == 0 || proof.Snapshot.Path == "" || proof.Snapshot.Bytes < 0 || !validStateSHA256(proof.Snapshot.SHA256) || proof.Visible.Path == "" || proof.Visible.Bytes < 0 || !validStateSHA256(proof.Visible.SHA256) {
		return errors.New("invalid native retirement proof")
	}
	if retiredAt, err := time.Parse(time.RFC3339Nano, proof.RetiredAt); err != nil || retiredAt.IsZero() {
		return errors.New("invalid native retirement proof time")
	}
	if proof.Version == legacyNativeRetirementVersion && proof.Sidecar != nil {
		return errors.New("legacy native retirement proof cannot bind a sidecar")
	}
	if proof.Sidecar != nil {
		if proof.Sidecar.Path == "" || proof.Sidecar.Bytes < 0 || !validStateSHA256(proof.Sidecar.SHA256) {
			return errors.New("invalid native retirement sidecar proof")
		}
	}
	return nil
}

func writeNativeRetirementProof(path string, proof NativeRetirementProof) error {
	if err := validateNativeRetirementProof(proof); err != nil {
		return err
	}
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

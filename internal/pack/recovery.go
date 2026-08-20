package pack

import (
	"archive/tar"
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
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
)

const (
	recoveryArchiveFilename      = "recovery.tar.zst"
	recoveryCatalogPath          = "catalog.json"
	recoveryVersion              = 1
	recoveryKind                 = "pack-recovery-v1"
	publishedMarkerFilename      = "published.json"
	legacyPublishedVersion       = 2
	legacyPublishedKind          = "pack-published-v2"
	publishedVersion             = 3
	publishedKind                = "pack-published-v3"
	packStoreIdentityFile        = "STORE-ID.json"
	packStoreIdentityVersion     = 1
	packStoreIdentityKind        = "pack-store-identity-v1"
	publicationHeadFilename      = "PUBLISHED"
	legacyPublicationHeadVersion = 1
	legacyPublicationHeadKind    = "pack-publication-head-v1"
	publicationHeadVersion       = 2
	publicationHeadKind          = "pack-publication-head-v2"
)

type RecoveryFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type RecoveryCatalog struct {
	Version    int            `json:"version"`
	Kind       string         `json:"kind"`
	Generation string         `json:"generation"`
	CreatedAt  string         `json:"created_at"`
	Files      []RecoveryFile `json:"files"`
	PackFiles  []RecoveryFile `json:"pack_files"`
}

type PublishedGeneration struct {
	Version            int    `json:"version"`
	Kind               string `json:"kind"`
	StoreID            string `json:"store_id,omitempty"`
	Generation         string `json:"generation"`
	Sequence           uint64 `json:"sequence"`
	PreviousGeneration string `json:"previous_generation,omitempty"`
	PublishedAt        string `json:"published_at"`
	RecoveryBytes      int64  `json:"recovery_bytes"`
	RecoverySHA256     string `json:"recovery_sha256"`
}

type PublicationHead struct {
	Version      int    `json:"version"`
	Kind         string `json:"kind"`
	StoreID      string `json:"store_id,omitempty"`
	Generation   string `json:"generation"`
	Sequence     uint64 `json:"sequence"`
	MarkerBytes  int64  `json:"marker_bytes"`
	MarkerSHA256 string `json:"marker_sha256"`
}

type packStoreIdentity struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	StoreID string `json:"store_id"`
}

type recoverySource struct {
	archivePath string
	diskPath    string
	identity    RecoveryFile
}

type recoveryRestoreTarget struct {
	root     string
	relative string
	replace  bool
}

func writeRecoveryArchive(storeDir string, generationDir string, meta indexV3Meta) error {
	sources, packFiles, err := collectRecoverySources(storeDir, generationDir, meta)
	if err != nil {
		return err
	}
	catalog := RecoveryCatalog{
		Version: recoveryVersion, Kind: recoveryKind, Generation: meta.Generation,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), PackFiles: packFiles,
	}
	for _, source := range sources {
		catalog.Files = append(catalog.Files, source.identity)
	}
	catalogData, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return fmt.Errorf("encode pack recovery catalog: %w", err)
	}
	catalogData = append(catalogData, '\n')

	temporary, err := os.CreateTemp(generationDir, ".recovery-*.tmp")
	if err != nil {
		return fmt.Errorf("create pack recovery archive: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	encoder, err := zstd.NewWriter(temporary,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1),
		zstd.WithLowerEncoderMem(true),
	)
	if err != nil {
		_ = temporary.Close()
		return fmt.Errorf("create pack recovery encoder: %w", err)
	}
	archive := tar.NewWriter(encoder)
	writeFailure := func(err error) error {
		_ = archive.Close()
		_ = encoder.Close()
		_ = temporary.Close()
		return err
	}
	if err := writeRecoveryBytes(archive, recoveryCatalogPath, catalogData); err != nil {
		return writeFailure(err)
	}
	for _, source := range sources {
		if err := writeRecoveryFile(archive, source); err != nil {
			return writeFailure(err)
		}
	}
	if err := archive.Close(); err != nil {
		_ = encoder.Close()
		_ = temporary.Close()
		return fmt.Errorf("close pack recovery archive: %w", err)
	}
	if err := encoder.Close(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("close pack recovery encoder: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync pack recovery archive: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close pack recovery file: %w", err)
	}
	finalPath := filepath.Join(generationDir, recoveryArchiveFilename)
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return fmt.Errorf("publish pack recovery archive: %w", err)
	}
	return syncDirectory(generationDir)
}

func collectRecoverySources(storeDir string, generationDir string, meta indexV3Meta) ([]recoverySource, []RecoveryFile, error) {
	var sources []recoverySource
	for _, name := range []string{indexV3MetaFilename, indexV3ObjectsFile, indexV3BlocksFile} {
		path := filepath.Join(generationDir, name)
		identity, err := recoveryFileIdentity(name, path)
		if err != nil {
			return nil, nil, err
		}
		sources = append(sources, recoverySource{archivePath: name, diskPath: path, identity: identity})
	}
	manifestRoot := filepath.Join(filepath.Clean(storeDir), "manifests")
	err := filepath.WalkDir(manifestRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return filepath.SkipDir
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil
		}
		relative, err := filepath.Rel(filepath.Clean(storeDir), path)
		if err != nil || !safeRecoveryPath(relative) || !strings.HasPrefix(filepath.ToSlash(relative), "manifests/") {
			return fmt.Errorf("unsafe recovery manifest path %q", path)
		}
		archivePath := filepath.ToSlash(relative)
		identity, err := recoveryFileIdentity(archivePath, path)
		if err != nil {
			return err
		}
		sources = append(sources, recoverySource{archivePath: archivePath, diskPath: path, identity: identity})
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].archivePath < sources[j].archivePath })

	packFiles := make([]RecoveryFile, 0, len(meta.Packs))
	for _, name := range meta.Packs {
		identity, err := recoveryFileIdentity(name, filepath.Join(generationDir, name))
		if err != nil {
			return nil, nil, err
		}
		packFiles = append(packFiles, identity)
	}
	sort.Slice(packFiles, func(i, j int) bool { return packFiles[i].Path < packFiles[j].Path })
	return sources, packFiles, nil
}

func recoveryFileIdentity(archivePath string, diskPath string) (RecoveryFile, error) {
	if !safeRecoveryPath(archivePath) {
		return RecoveryFile{}, fmt.Errorf("unsafe pack recovery path %q", archivePath)
	}
	file, err := os.Open(diskPath)
	if err != nil {
		return RecoveryFile{}, err
	}
	hasher := sha256.New()
	bytesRead, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil {
		return RecoveryFile{}, copyErr
	}
	if closeErr != nil {
		return RecoveryFile{}, closeErr
	}
	return RecoveryFile{Path: filepath.ToSlash(archivePath), Bytes: bytesRead, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

func writeRecoveryBytes(archive *tar.Writer, path string, data []byte) error {
	header := &tar.Header{Name: path, Mode: 0o600, Size: int64(len(data)), ModTime: time.Unix(0, 0)}
	if err := archive.WriteHeader(header); err != nil {
		return fmt.Errorf("write pack recovery header %s: %w", path, err)
	}
	if _, err := archive.Write(data); err != nil {
		return fmt.Errorf("write pack recovery data %s: %w", path, err)
	}
	return nil
}

func writeRecoveryFile(archive *tar.Writer, source recoverySource) error {
	header := &tar.Header{Name: source.archivePath, Mode: 0o600, Size: source.identity.Bytes, ModTime: time.Unix(0, 0)}
	if err := archive.WriteHeader(header); err != nil {
		return fmt.Errorf("write pack recovery header %s: %w", source.archivePath, err)
	}
	file, err := os.Open(source.diskPath)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(archive, file)
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("write pack recovery data %s: %w", source.archivePath, copyErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if written != source.identity.Bytes {
		return fmt.Errorf("pack recovery source %s changed while archiving", source.archivePath)
	}
	return nil
}

func ReadRecoveryCatalog(generationDir string) (RecoveryCatalog, error) {
	archive, closeArchive, err := openRecoveryArchive(generationDir)
	if err != nil {
		return RecoveryCatalog{}, err
	}
	defer closeArchive()
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return RecoveryCatalog{}, errors.New("pack recovery catalog is missing")
		}
		if err != nil {
			return RecoveryCatalog{}, fmt.Errorf("read pack recovery archive: %w", err)
		}
		if header.Name != recoveryCatalogPath {
			continue
		}
		if header.Size < 0 || header.Size > 64<<20 {
			return RecoveryCatalog{}, errors.New("pack recovery catalog size is invalid")
		}
		data, err := io.ReadAll(io.LimitReader(archive, header.Size+1))
		if err != nil {
			return RecoveryCatalog{}, err
		}
		if int64(len(data)) != header.Size {
			return RecoveryCatalog{}, errors.New("pack recovery catalog is truncated")
		}
		var catalog RecoveryCatalog
		if err := json.Unmarshal(data, &catalog); err != nil {
			return RecoveryCatalog{}, fmt.Errorf("decode pack recovery catalog: %w", err)
		}
		if err := validateRecoveryCatalog(catalog, filepath.Base(filepath.Clean(generationDir))); err != nil {
			return RecoveryCatalog{}, err
		}
		return catalog, nil
	}
}

func VerifyRecovery(ctx context.Context, storeDir string, generation string) (RecoveryCatalog, error) {
	if !safeGeneration(generation) {
		return RecoveryCatalog{}, errors.New("safe pack generation is required")
	}
	directory := filepath.Join(filepath.Clean(storeDir), "packs", generation)
	return VerifyRecoveryDirectory(ctx, storeDir, directory)
}

func VerifyRecoveryDirectory(ctx context.Context, storeDir string, directory string) (RecoveryCatalog, error) {
	catalog, err := ReadRecoveryCatalog(directory)
	if err != nil {
		return RecoveryCatalog{}, err
	}
	if err := verifyRecoveryArchive(ctx, directory, catalog); err != nil {
		return RecoveryCatalog{}, err
	}
	return catalog, nil
}

func verifyRecoveryArchive(ctx context.Context, generationDir string, catalog RecoveryCatalog) error {
	root, err := openPackGenerationRoot(generationDir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, identity := range catalog.PackFiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := verifyRecoveryDiskFile(root, identity.Path, identity); err != nil {
			return fmt.Errorf("verify recovery pack %s: %w", identity.Path, err)
		}
	}
	expected := make(map[string]RecoveryFile, len(catalog.Files))
	for _, identity := range catalog.Files {
		expected[identity.Path] = identity
	}
	archive, closeArchive, err := openRecoveryArchive(generationDir)
	if err != nil {
		return err
	}
	defer closeArchive()
	seen := make(map[string]struct{}, len(expected))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if header.Name == recoveryCatalogPath {
			continue
		}
		identity, ok := expected[header.Name]
		if !ok || !safeRecoveryPath(header.Name) || header.Size != identity.Bytes {
			return fmt.Errorf("unexpected pack recovery entry %q", header.Name)
		}
		if _, duplicate := seen[header.Name]; duplicate {
			return fmt.Errorf("duplicate pack recovery entry %q", header.Name)
		}
		hasher := sha256.New()
		written, err := io.Copy(hasher, archive)
		if err != nil || written != identity.Bytes || hex.EncodeToString(hasher.Sum(nil)) != identity.SHA256 {
			return fmt.Errorf("pack recovery entry %s failed verification", header.Name)
		}
		seen[header.Name] = struct{}{}
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("pack recovery archive contains %d of %d required files", len(seen), len(expected))
	}
	return nil
}

type verifiedRecoveryManifest struct {
	manifest fold.Manifest
	identity RecoveryFile
}

func verifyRecoveryManifestArchive(ctx context.Context, generationDir string, catalog RecoveryCatalog, reader fold.ObjectReader) (map[string]verifiedRecoveryManifest, error) {
	expected := make(map[string]RecoveryFile)
	for _, identity := range catalog.Files {
		if strings.HasPrefix(identity.Path, "manifests/") {
			expected[identity.Path] = identity
		}
	}
	verified := make(map[string]verifiedRecoveryManifest, len(expected))
	if len(expected) == 0 {
		return verified, nil
	}
	archive, closeArchive, err := openRecoveryArchive(generationDir)
	if err != nil {
		return nil, err
	}
	defer closeArchive()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		identity, ok := expected[header.Name]
		if !ok {
			continue
		}
		if header.Size != identity.Bytes || identity.Bytes > 512<<20 {
			return nil, fmt.Errorf("recovery manifest %s has an invalid size", header.Name)
		}
		data, err := io.ReadAll(io.LimitReader(archive, identity.Bytes+1))
		if err != nil || int64(len(data)) != identity.Bytes {
			return nil, fmt.Errorf("read recovery manifest %s: %w", header.Name, err)
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != identity.SHA256 {
			return nil, fmt.Errorf("recovery manifest %s has a SHA-256 mismatch", header.Name)
		}
		var manifest fold.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("decode recovery manifest %s: %w", header.Name, err)
		}
		if err := fold.VerifyManifest(ctx, reader, manifest); err != nil {
			return nil, fmt.Errorf("recovery manifest %s cannot reconstruct its declared source: %w", header.Name, err)
		}
		verified[header.Name] = verifiedRecoveryManifest{manifest: manifest, identity: identity}
	}
	if len(verified) != len(expected) {
		return nil, fmt.Errorf("verified %d of %d recovery manifests", len(verified), len(expected))
	}
	return verified, nil
}

func verifyRecoveryAndLiveManifests(ctx context.Context, storeDir string, generationDir string, catalog RecoveryCatalog, reader fold.ObjectReader) error {
	archived, err := verifyRecoveryManifestArchive(ctx, generationDir, catalog, reader)
	if err != nil {
		return err
	}
	manifestRoot := filepath.Join(filepath.Clean(storeDir), "manifests")
	err = filepath.WalkDir(manifestRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return filepath.SkipDir
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("live manifest %s is not a regular file", path)
		}
		manifest, err := fold.LoadManifestPath(path)
		if err != nil {
			return err
		}
		if err := fold.VerifyManifest(ctx, reader, manifest); err != nil {
			return fmt.Errorf("live manifest %s cannot be reconstructed from generation %s: %w", path, filepath.Base(generationDir), err)
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	states, issues, err := vfs.DiscoverSessionStatesDetailed(storeDir)
	if err != nil {
		return err
	}
	if len(issues) != 0 {
		return fmt.Errorf("refusing pack recovery with %d unresolved managed session state issue(s)", len(issues))
	}
	for _, state := range states {
		target := filepath.Clean(state.ManifestPath)
		relative, err := filepath.Rel(filepath.Clean(storeDir), target)
		if err != nil {
			return err
		}
		archivePath := filepath.ToSlash(relative)
		if !strings.HasPrefix(archivePath, "manifests/") || !safeRecoveryPath(archivePath) {
			return fmt.Errorf("managed manifest %s is outside the recoverable manifest store", state.SessionID)
		}
		var manifest fold.Manifest
		info, statErr := os.Lstat(target)
		if statErr == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("managed manifest %s is not a regular file", state.SessionID)
			}
			identity, err := recoveryFileIdentity(archivePath, target)
			if err != nil {
				return err
			}
			if identity.SHA256 != state.ManifestSHA256 {
				return fmt.Errorf("managed manifest bytes differ from state for session %s", state.SessionID)
			}
			manifest, err = fold.LoadManifestPath(target)
			if err != nil {
				return fmt.Errorf("load current managed manifest %s: %w", state.SessionID, err)
			}
		} else if errors.Is(statErr, os.ErrNotExist) {
			recovered, ok := archived[archivePath]
			if !ok {
				return fmt.Errorf("managed manifest %s is absent from the authoritative recovery archive", state.SessionID)
			}
			if recovered.identity.SHA256 != state.ManifestSHA256 {
				return fmt.Errorf("authoritative recovery archive has a stale manifest version for session %s", state.SessionID)
			}
			manifest = recovered.manifest
		} else {
			return statErr
		}
		if manifest.Session.ID != state.SessionID || manifest.Source.Bytes != state.BaseBytes || manifest.Source.SHA256 != state.BaseSHA256 {
			return fmt.Errorf("managed manifest identity differs for session %s", state.SessionID)
		}
		if err := fold.VerifyManifest(ctx, reader, manifest); err != nil {
			return fmt.Errorf("managed manifest %s is not byte-complete in generation %s: %w", state.SessionID, filepath.Base(generationDir), err)
		}
	}
	return nil
}

func repairGenerationIndex(generationDir string) error {
	generationDir = filepath.Clean(generationDir)
	packsDir := filepath.Dir(generationDir)
	storeDir := filepath.Dir(packsDir)
	if filepath.Base(packsDir) != "packs" || !safeGeneration(filepath.Base(generationDir)) {
		return errors.New("pack generation recovery path is not canonical")
	}
	generationRoot, err := openPackGenerationRoot(generationDir)
	if err != nil {
		return fmt.Errorf("validate pack generation before index repair: %w", err)
	}
	_ = generationRoot.Close()
	catalog, err := ReadRecoveryCatalog(generationDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	wanted := make(map[string]RecoveryFile)
	for _, identity := range catalog.Files {
		switch identity.Path {
		case indexV3MetaFilename, indexV3ObjectsFile, indexV3BlocksFile:
			wanted[identity.Path] = identity
		}
	}
	if len(wanted) != 3 {
		return errors.New("pack recovery archive does not contain the complete runtime index")
	}
	root, err := os.OpenRoot(storeDir)
	if err != nil {
		return fmt.Errorf("open pack repair store root: %w", err)
	}
	defer root.Close()
	for _, name := range []string{indexV3MetaFilename, indexV3ObjectsFile, indexV3BlocksFile} {
		relative := filepath.Join("packs", filepath.Base(generationDir), name)
		info, statErr := root.Lstat(relative)
		if statErr == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("refusing to repair non-regular pack index %s", name)
			}
			continue
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect pack index %s before repair: %w", name, statErr)
		}
	}
	return restoreRecoveryEntries(generationDir, wanted, func(path string) (recoveryRestoreTarget, error) {
		return newRecoveryRestoreTarget(storeDir, filepath.Join("packs", filepath.Base(generationDir), filepath.FromSlash(path)), true)
	})
}

func RepairCurrentManifests(storeDir string) (int, error) {
	ctx := context.Background()
	deletionLock, err := storage.AcquireOperationLock(storeDir, "session-deletions")
	if err != nil {
		return 0, fmt.Errorf("freeze managed session deletions during manifest repair: %w", err)
	}
	defer deletionLock.Close()
	deletions, err := vfs.DiscoverSessionDeletions(storeDir)
	if err != nil {
		return 0, fmt.Errorf("discover managed session deletions before manifest repair: %w", err)
	}
	excluded := make(map[string]struct{}, len(deletions))
	for _, deletion := range deletions {
		excluded[deletion.SessionID] = struct{}{}
	}
	generation, err := currentOrRecoveredGeneration(ctx, storeDir)
	if err != nil {
		return 0, err
	}
	states, issues, err := vfs.DiscoverSessionStatesDetailed(storeDir)
	if err != nil {
		return 0, err
	}
	states, issues = excludeDeletedManifestRepairStates(states, issues, excluded)
	if len(issues) != 0 {
		return 0, fmt.Errorf("refusing manifest repair with %d unresolved session state issue(s)", len(issues))
	}
	writerGuards := make([]*vfs.WriterLeaseGuard, 0, len(states))
	defer func() {
		for index := len(writerGuards) - 1; index >= 0; index-- {
			_ = writerGuards[index].Close()
		}
	}()
	for _, state := range states {
		guard, locked, err := vfs.TryAcquireWriterLeaseGuard(storeDir, state.SessionID)
		if err != nil {
			return 0, err
		}
		if !locked {
			return 0, fmt.Errorf("managed session %s is changing during manifest repair", state.SessionID)
		}
		writerGuards = append(writerGuards, guard)
	}
	objectLock, err := storage.AcquireOperationLock(storeDir, "objects")
	if err != nil {
		return 0, err
	}
	defer objectLock.Close()
	current, err := CurrentGeneration(storeDir)
	if err != nil {
		return 0, fmt.Errorf("read pack CURRENT before manifest repair: %w", err)
	}
	if current != generation {
		return 0, fmt.Errorf("pack CURRENT changed before manifest repair from %s to %s", generation, current)
	}
	if err := requireSameManifestRepairDeletions(storeDir, deletions); err != nil {
		return 0, err
	}
	currentStates, currentIssues, err := vfs.DiscoverSessionStatesDetailed(storeDir)
	currentStates, currentIssues = excludeDeletedManifestRepairStates(currentStates, currentIssues, excluded)
	if err != nil || len(currentIssues) != 0 || len(currentStates) != len(states) {
		return 0, fmt.Errorf("managed session set changed before manifest repair: states=%d/%d issues=%d err=%v", len(states), len(currentStates), len(currentIssues), err)
	}
	for index := range states {
		if currentStates[index] != states[index] {
			return 0, fmt.Errorf("managed session %s changed before manifest repair", states[index].SessionID)
		}
	}
	directory := filepath.Join(filepath.Clean(storeDir), "packs", generation)
	catalog, err := ReadRecoveryCatalog(directory)
	if err != nil {
		return 0, err
	}
	available := make(map[string]RecoveryFile)
	for _, identity := range catalog.Files {
		if strings.HasPrefix(identity.Path, "manifests/") {
			available[identity.Path] = identity
		}
	}
	wanted := make(map[string]RecoveryFile)
	references := make(map[string]vfs.SessionState)
	for _, reference := range states {
		if _, deleting := excluded[reference.SessionID]; deleting {
			continue
		}
		target := filepath.Clean(reference.ManifestPath)
		relative, err := filepath.Rel(filepath.Clean(storeDir), target)
		if err != nil {
			return 0, err
		}
		archivePath := filepath.ToSlash(relative)
		if !strings.HasPrefix(archivePath, "manifests/") || !safeRecoveryPath(archivePath) {
			return 0, fmt.Errorf("managed manifest %s is outside the recoverable manifest store", reference.SessionID)
		}
		info, statErr := os.Lstat(target)
		if statErr == nil {
			if !info.Mode().IsRegular() {
				return 0, fmt.Errorf("managed manifest for session %s is not a regular file", reference.SessionID)
			}
			identity, err := recoveryFileIdentity(archivePath, target)
			if err != nil {
				return 0, err
			}
			if identity.SHA256 != reference.ManifestSHA256 {
				return 0, fmt.Errorf("refusing to overwrite a valid but different manifest version for session %s", reference.SessionID)
			}
			existing, loadErr := fold.LoadManifestPath(target)
			if loadErr != nil {
				return 0, fmt.Errorf("refusing to replace unreadable manifest for session %s: %w", reference.SessionID, loadErr)
			}
			if existing.Session.ID != reference.SessionID || existing.Source.Bytes != reference.BaseBytes || existing.Source.SHA256 != reference.BaseSHA256 {
				return 0, fmt.Errorf("refusing to overwrite a valid but different manifest for session %s", reference.SessionID)
			}
			continue
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return 0, fmt.Errorf("inspect managed manifest for session %s: %w", reference.SessionID, statErr)
		}
		identity, ok := available[archivePath]
		if !ok {
			return 0, fmt.Errorf("current manifest for session %s is absent from the published recovery archive", reference.SessionID)
		}
		if identity.SHA256 != reference.ManifestSHA256 {
			return 0, fmt.Errorf("published recovery archive contains a stale manifest version for session %s", reference.SessionID)
		}
		wanted[archivePath] = identity
		references[archivePath] = reference
	}
	resolver, err := openGeneration(directory, 0, false)
	if err != nil {
		return 0, fmt.Errorf("open published pack before manifest repair: %w", err)
	}
	defer resolver.Close()
	if err := requireSameManifestRepairDeletions(storeDir, deletions); err != nil {
		return 0, err
	}
	restored := 0
	err = restoreRecoveryEntriesValidated(directory, wanted, func(path string) (recoveryRestoreTarget, error) {
		if !strings.HasPrefix(path, "manifests/") || !safeRecoveryPath(path) {
			return recoveryRestoreTarget{}, fmt.Errorf("unsafe recovery manifest %q", path)
		}
		return newRecoveryRestoreTarget(storeDir, filepath.FromSlash(path), false)
	}, func(path string, _ string, temporary *os.File) error {
		reference, ok := references[path]
		if !ok {
			return fmt.Errorf("recovery manifest %s has no current-session proof", path)
		}
		if _, err := temporary.Seek(0, io.SeekStart); err != nil {
			return err
		}
		data, err := io.ReadAll(temporary)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != reference.ManifestSHA256 {
			return fmt.Errorf("recovery manifest %s bytes are not the current version for session %s", path, reference.SessionID)
		}
		manifest, err := fold.DecodeManifest(data)
		if err != nil {
			return err
		}
		if manifest.Session.ID != reference.SessionID || manifest.Source.Bytes != reference.BaseBytes || manifest.Source.SHA256 != reference.BaseSHA256 {
			return fmt.Errorf("recovery manifest %s is not the current version for session %s", path, reference.SessionID)
		}
		if err := fold.VerifyManifest(ctx, resolver, manifest); err != nil {
			return fmt.Errorf("recovery manifest %s cannot be reconstructed from the published pack: %w", path, err)
		}
		return nil
	}, func(_ string) { restored++ })
	return restored, err
}

func excludeDeletedManifestRepairStates(states []vfs.SessionState, issues []vfs.SessionStateIssue, excluded map[string]struct{}) ([]vfs.SessionState, []vfs.SessionStateIssue) {
	currentStates := make([]vfs.SessionState, 0, len(states))
	for _, state := range states {
		if _, deleting := excluded[state.SessionID]; !deleting {
			currentStates = append(currentStates, state)
		}
	}
	currentIssues := make([]vfs.SessionStateIssue, 0, len(issues))
	for _, issue := range issues {
		if _, deleting := excluded[issue.SessionID]; !deleting {
			currentIssues = append(currentIssues, issue)
		}
	}
	return currentStates, currentIssues
}

func requireSameManifestRepairDeletions(storeDir string, expected []vfs.SessionDeletion) error {
	current, err := vfs.DiscoverSessionDeletions(storeDir)
	if err != nil {
		return fmt.Errorf("rediscover managed session deletions during manifest repair: %w", err)
	}
	if len(current) != len(expected) {
		return errors.New("managed session deletion set changed during manifest repair")
	}
	for index := range expected {
		if current[index] != expected[index] {
			return fmt.Errorf("managed session deletion %s changed during manifest repair", expected[index].SessionID)
		}
	}
	return nil
}

func restoreRecoveryEntries(generationDir string, wanted map[string]RecoveryFile, target func(string) (recoveryRestoreTarget, error), callbacks ...func(string)) error {
	return restoreRecoveryEntriesValidated(generationDir, wanted, target, nil, callbacks...)
}

func restoreRecoveryEntriesValidated(generationDir string, wanted map[string]RecoveryFile, target func(string) (recoveryRestoreTarget, error), validate func(string, string, *os.File) error, callbacks ...func(string)) error {
	missing := make(map[string]RecoveryFile)
	for path, identity := range wanted {
		targetPath, err := target(path)
		if err != nil {
			return err
		}
		if verifyRecoveryRootFile(targetPath, identity) == nil {
			continue
		}
		missing[path] = identity
	}
	if len(missing) == 0 {
		return nil
	}
	archive, closeArchive, err := openRecoveryArchive(generationDir)
	if err != nil {
		return err
	}
	defer closeArchive()
	for len(missing) > 0 {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		identity, ok := missing[header.Name]
		if !ok {
			continue
		}
		if header.Size != identity.Bytes {
			return fmt.Errorf("pack recovery entry %s size changed", header.Name)
		}
		restoreTarget, err := target(header.Name)
		if err != nil {
			return err
		}
		if err := restoreRecoveryEntryValidated(archive, restoreTarget, identity, func(temporary *os.File) error {
			if validate == nil {
				return nil
			}
			return validate(header.Name, restoreTarget.absolutePath(), temporary)
		}); err != nil {
			return err
		}
		delete(missing, header.Name)
		for _, callback := range callbacks {
			callback(header.Name)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("pack recovery archive is missing %d requested entries", len(missing))
	}
	return nil
}

func restoreRecoveryEntry(source io.Reader, target string, identity RecoveryFile) error {
	restoreTarget, err := newRecoveryRestoreTarget(filepath.Dir(filepath.Clean(target)), filepath.Base(filepath.Clean(target)), true)
	if err != nil {
		return err
	}
	return restoreRecoveryEntryValidated(source, restoreTarget, identity, nil)
}

func restoreRecoveryEntryValidated(source io.Reader, target recoveryRestoreTarget, identity RecoveryFile, validate func(*os.File) error) error {
	root, err := os.OpenRoot(target.root)
	if err != nil {
		return fmt.Errorf("open recovery target root: %w", err)
	}
	defer root.Close()
	parent := filepath.Dir(target.relative)
	if err := root.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	temporary, temporaryPath, err := createRecoveryRootTemp(root, parent)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(temporaryPath) }()
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hasher), source)
	if copyErr != nil || written != identity.Bytes || hex.EncodeToString(hasher.Sum(nil)) != identity.SHA256 {
		_ = temporary.Close()
		return fmt.Errorf("recovered file %s failed verification", identity.Path)
	}
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if validate != nil {
		if err := validate(temporary); err != nil {
			_ = temporary.Close()
			return err
		}
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if target.replace {
		if err := root.Rename(temporaryPath, target.relative); err != nil {
			return err
		}
	} else {
		if err := root.Link(temporaryPath, target.relative); err != nil {
			return fmt.Errorf("publish recovered file without replacement: %w", err)
		}
		if err := root.Remove(temporaryPath); err != nil {
			return fmt.Errorf("remove linked recovery temporary: %w", err)
		}
	}
	return syncRecoveryRootDirectory(root, parent)
}

func newRecoveryRestoreTarget(root string, relative string, replace bool) (recoveryRestoreTarget, error) {
	if root == "" || relative == "" || filepath.IsAbs(relative) {
		return recoveryRestoreTarget{}, errors.New("recovery target requires a root-relative path")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return recoveryRestoreTarget{}, err
	}
	relative = filepath.Clean(relative)
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return recoveryRestoreTarget{}, errors.New("recovery target escapes its root")
	}
	return recoveryRestoreTarget{root: filepath.Clean(absoluteRoot), relative: relative, replace: replace}, nil
}

func (t recoveryRestoreTarget) absolutePath() string {
	return filepath.Join(t.root, t.relative)
}

func verifyRecoveryRootFile(target recoveryRestoreTarget, identity RecoveryFile) error {
	root, err := os.OpenRoot(target.root)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(target.relative)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("recovery target is not a regular file")
	}
	file, err := root.Open(target.relative)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	if written != identity.Bytes || hex.EncodeToString(hasher.Sum(nil)) != identity.SHA256 {
		return errors.New("size or SHA-256 mismatch")
	}
	return nil
}

func createRecoveryRootTemp(root *os.Root, directory string) (*os.File, string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return nil, "", err
		}
		name := filepath.Join(directory, ".recovery-restore-"+hex.EncodeToString(token[:])+".tmp")
		file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return file, name, nil
	}
	return nil, "", errors.New("could not allocate a recovery temporary file")
}

func syncRecoveryRootDirectory(root *os.Root, relative string) error {
	directory, err := root.Open(relative)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func markGenerationPublished(generationDir string, sequence uint64, previousGeneration string) (PublishedGeneration, error) {
	generationDir = filepath.Clean(generationDir)
	generation := filepath.Base(generationDir)
	if !safeGeneration(generation) {
		return PublishedGeneration{}, errors.New("safe pack generation is required")
	}
	if sequence == 0 || (sequence == 1 && previousGeneration != "") || (sequence > 1 && !safeGeneration(previousGeneration)) {
		return PublishedGeneration{}, errors.New("valid pack publication sequence and predecessor are required")
	}
	storeIdentity, err := loadOrCreatePackStoreIdentity(filepath.Dir(generationDir))
	if err != nil {
		return PublishedGeneration{}, fmt.Errorf("load pack store identity: %w", err)
	}
	recovery, err := recoveryFileIdentity(recoveryArchiveFilename, filepath.Join(generationDir, recoveryArchiveFilename))
	if err != nil {
		return PublishedGeneration{}, fmt.Errorf("hash published pack recovery archive: %w", err)
	}
	marker := PublishedGeneration{
		Version: publishedVersion, Kind: publishedKind, StoreID: storeIdentity.StoreID, Generation: generation,
		Sequence: sequence, PreviousGeneration: previousGeneration,
		PublishedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		RecoveryBytes: recovery.Bytes, RecoverySHA256: recovery.SHA256,
	}
	if err := writeAtomicJSON(filepath.Join(generationDir, publishedMarkerFilename), ".published-", marker); err != nil {
		return PublishedGeneration{}, err
	}
	return marker, nil
}

func publishPublicationHead(packsDir string, marker PublishedGeneration) error {
	version := publicationHeadVersion
	kind := publicationHeadKind
	storeID := marker.StoreID
	if marker.Version == legacyPublishedVersion && marker.Kind == legacyPublishedKind && marker.StoreID == "" {
		version = legacyPublicationHeadVersion
		kind = legacyPublicationHeadKind
		storeID = ""
	} else if marker.Version != publishedVersion || marker.Kind != publishedKind || !validPackStoreID(marker.StoreID) {
		return errors.New("pack publication marker has no valid store identity")
	}
	markerIdentity, err := recoveryFileIdentity(publishedMarkerFilename, filepath.Join(packsDir, marker.Generation, publishedMarkerFilename))
	if err != nil {
		return fmt.Errorf("hash published pack marker: %w", err)
	}
	head := PublicationHead{
		Version: version, Kind: kind, StoreID: storeID,
		Generation: marker.Generation, Sequence: marker.Sequence,
		MarkerBytes: markerIdentity.Bytes, MarkerSHA256: markerIdentity.SHA256,
	}
	return writeAtomicJSON(filepath.Join(filepath.Clean(packsDir), publicationHeadFilename), ".PUBLISHED-", head)
}

func readPackStoreIdentity(packsDir string) (packStoreIdentity, error) {
	path := filepath.Join(filepath.Clean(packsDir), packStoreIdentityFile)
	info, err := os.Lstat(path)
	if err != nil {
		return packStoreIdentity{}, err
	}
	if !info.Mode().IsRegular() {
		return packStoreIdentity{}, errors.New("pack store identity is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return packStoreIdentity{}, err
	}
	var identity packStoreIdentity
	if err := decodeStrictPackJSON(data, &identity); err != nil {
		return packStoreIdentity{}, fmt.Errorf("decode pack store identity: %w", err)
	}
	if identity.Version != packStoreIdentityVersion || identity.Kind != packStoreIdentityKind || !validPackStoreID(identity.StoreID) {
		return packStoreIdentity{}, errors.New("invalid pack store identity")
	}
	return identity, nil
}

func loadOrCreatePackStoreIdentity(packsDir string) (packStoreIdentity, error) {
	identity, err := readPackStoreIdentity(packsDir)
	if err == nil {
		return identity, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return packStoreIdentity{}, err
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return packStoreIdentity{}, fmt.Errorf("create pack store identity: %w", err)
	}
	identity = packStoreIdentity{
		Version: packStoreIdentityVersion,
		Kind:    packStoreIdentityKind,
		StoreID: hex.EncodeToString(random),
	}
	if err := writeAtomicJSON(filepath.Join(filepath.Clean(packsDir), packStoreIdentityFile), ".STORE-ID-", identity); err != nil {
		return packStoreIdentity{}, fmt.Errorf("publish pack store identity: %w", err)
	}
	loaded, err := readPackStoreIdentity(packsDir)
	if err != nil {
		return packStoreIdentity{}, err
	}
	if loaded != identity {
		return packStoreIdentity{}, errors.New("pack store identity changed during publication")
	}
	return loaded, nil
}

func validPackStoreID(value string) bool {
	if len(value) != 32 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16
}

func decodeStrictPackJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func writeAtomicJSON(path string, temporaryPrefix string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	directory := filepath.Dir(filepath.Clean(path))
	temporary, err := os.CreateTemp(directory, temporaryPrefix+"*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
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
	if err := replaceFile(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func readPublishedMarker(generationDir string) (PublishedGeneration, error) {
	markerPath := filepath.Join(filepath.Clean(generationDir), publishedMarkerFilename)
	info, err := os.Lstat(markerPath)
	if err != nil {
		return PublishedGeneration{}, err
	}
	if !info.Mode().IsRegular() {
		return PublishedGeneration{}, errors.New("published pack marker is not a regular file")
	}
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return PublishedGeneration{}, err
	}
	var marker PublishedGeneration
	if err := decodeStrictPackJSON(data, &marker); err != nil {
		return PublishedGeneration{}, fmt.Errorf("decode published pack marker: %w", err)
	}
	versionValid := marker.Version == legacyPublishedVersion && marker.Kind == legacyPublishedKind && marker.StoreID == ""
	versionValid = versionValid || (marker.Version == publishedVersion && marker.Kind == publishedKind && validPackStoreID(marker.StoreID))
	if !versionValid || marker.Generation != filepath.Base(filepath.Clean(generationDir)) || !safeGeneration(marker.Generation) || marker.Sequence == 0 || marker.RecoveryBytes < 0 || len(marker.RecoverySHA256) != sha256.Size*2 {
		return PublishedGeneration{}, errors.New("invalid published pack marker")
	}
	if (marker.Sequence == 1 && marker.PreviousGeneration != "") || (marker.Sequence > 1 && !safeGeneration(marker.PreviousGeneration)) || marker.PreviousGeneration == marker.Generation {
		return PublishedGeneration{}, errors.New("invalid published pack predecessor")
	}
	if _, err := time.Parse(time.RFC3339Nano, marker.PublishedAt); err != nil {
		return PublishedGeneration{}, errors.New("invalid pack publication time")
	}
	return marker, nil
}

func readPublishedGeneration(generationDir string) (PublishedGeneration, error) {
	marker, err := readPublishedMarker(generationDir)
	if err != nil {
		return PublishedGeneration{}, err
	}
	recovery, err := recoveryFileIdentity(recoveryArchiveFilename, filepath.Join(generationDir, recoveryArchiveFilename))
	if err != nil {
		return PublishedGeneration{}, err
	}
	if recovery.Bytes != marker.RecoveryBytes || recovery.SHA256 != marker.RecoverySHA256 {
		return PublishedGeneration{}, errors.New("published marker does not match the recovery archive")
	}
	return marker, nil
}

func readPublicationHead(packsDir string) (PublicationHead, PublishedGeneration, error) {
	packsDir = filepath.Clean(packsDir)
	headPath := filepath.Join(packsDir, publicationHeadFilename)
	info, err := os.Lstat(headPath)
	if err != nil {
		return PublicationHead{}, PublishedGeneration{}, err
	}
	if !info.Mode().IsRegular() {
		return PublicationHead{}, PublishedGeneration{}, errors.New("pack publication head is not a regular file")
	}
	data, err := os.ReadFile(headPath)
	if err != nil {
		return PublicationHead{}, PublishedGeneration{}, err
	}
	var head PublicationHead
	if err := json.Unmarshal(data, &head); err != nil {
		return PublicationHead{}, PublishedGeneration{}, fmt.Errorf("decode pack publication head: %w", err)
	}
	versionValid := head.Version == legacyPublicationHeadVersion && head.Kind == legacyPublicationHeadKind && head.StoreID == ""
	versionValid = versionValid || (head.Version == publicationHeadVersion && head.Kind == publicationHeadKind && validPackStoreID(head.StoreID))
	if !versionValid || !safeGeneration(head.Generation) || head.Sequence == 0 || head.MarkerBytes < 0 || len(head.MarkerSHA256) != sha256.Size*2 {
		return PublicationHead{}, PublishedGeneration{}, errors.New("invalid pack publication head")
	}
	generationDir := filepath.Join(packsDir, head.Generation)
	markerIdentity, err := recoveryFileIdentity(publishedMarkerFilename, filepath.Join(generationDir, publishedMarkerFilename))
	if err != nil {
		return PublicationHead{}, PublishedGeneration{}, fmt.Errorf("read pack publication head marker: %w", err)
	}
	if markerIdentity.Bytes != head.MarkerBytes || markerIdentity.SHA256 != head.MarkerSHA256 {
		return PublicationHead{}, PublishedGeneration{}, errors.New("pack publication head does not match its generation marker")
	}
	marker, err := readPublishedMarker(generationDir)
	if err != nil {
		return PublicationHead{}, PublishedGeneration{}, err
	}
	if marker.Generation != head.Generation || marker.Sequence != head.Sequence {
		return PublicationHead{}, PublishedGeneration{}, errors.New("pack publication head and marker identities differ")
	}
	if marker.StoreID != head.StoreID {
		return PublicationHead{}, PublishedGeneration{}, errors.New("pack publication head and marker store identities differ")
	}
	return head, marker, nil
}

func RecoverCurrentGeneration(ctx context.Context, storeDir string) (string, error) {
	lock, err := storage.AcquireOperationLock(storeDir, "objects")
	if err != nil {
		return "", err
	}
	defer lock.Close()
	return recoverCurrentGenerationLocked(ctx, storeDir)
}

func findPublishedSuccessor(packsDir string, head PublicationHead) (PublishedGeneration, bool, error) {
	if head.Sequence == ^uint64(0) {
		return PublishedGeneration{}, false, errors.New("pack publication sequence cannot advance")
	}
	entries, err := os.ReadDir(filepath.Clean(packsDir))
	if err != nil {
		return PublishedGeneration{}, false, err
	}
	var successor PublishedGeneration
	found := false
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !safeGeneration(entry.Name()) {
			continue
		}
		markerPath := filepath.Join(packsDir, entry.Name(), publishedMarkerFilename)
		if _, err := os.Lstat(markerPath); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return PublishedGeneration{}, false, err
		}
		marker, err := readPublishedMarker(filepath.Join(packsDir, entry.Name()))
		if err != nil {
			return PublishedGeneration{}, false, fmt.Errorf("read pack publication marker for %s: %w", entry.Name(), err)
		}
		if marker.Sequence <= head.Sequence {
			continue
		}
		if marker.Sequence != head.Sequence+1 || marker.PreviousGeneration != head.Generation {
			return PublishedGeneration{}, false, fmt.Errorf("pack publication marker %s is not a unique successor of authoritative generation %s", marker.Generation, head.Generation)
		}
		if head.StoreID != "" && marker.StoreID != head.StoreID {
			return PublishedGeneration{}, false, fmt.Errorf("pack publication successor %s belongs to another store", marker.Generation)
		}
		if found {
			return PublishedGeneration{}, false, fmt.Errorf("multiple pack publication successors of generation %s", head.Generation)
		}
		successor, found = marker, true
	}
	return successor, found, nil
}

// findPublishedChainTip resolves every surviving marker after head into one
// contiguous successor chain. Markers before head may have been reclaimed by
// storage GC, but any surviving marker at or after head must be explainable by
// the same chain before publication metadata is rebuilt.
func findPublishedChainTip(packsDir string, head PublishedGeneration) (PublishedGeneration, error) {
	entries, err := os.ReadDir(filepath.Clean(packsDir))
	if err != nil {
		return PublishedGeneration{}, err
	}
	markers := make([]PublishedGeneration, 0)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !safeGeneration(entry.Name()) {
			continue
		}
		markerPath := filepath.Join(packsDir, entry.Name(), publishedMarkerFilename)
		if _, err := os.Lstat(markerPath); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return PublishedGeneration{}, err
		}
		marker, err := readPublishedMarker(filepath.Join(packsDir, entry.Name()))
		if err != nil {
			return PublishedGeneration{}, fmt.Errorf("read pack publication marker for %s: %w", entry.Name(), err)
		}
		if marker.Generation == head.Generation {
			if marker.Sequence != head.Sequence {
				return PublishedGeneration{}, fmt.Errorf("pack publication marker %s does not match migration head", marker.Generation)
			}
			continue
		}
		if marker.Sequence < head.Sequence {
			continue
		}
		if marker.Sequence == head.Sequence {
			return PublishedGeneration{}, fmt.Errorf("multiple pack publication generations at sequence %d", head.Sequence)
		}
		markers = append(markers, marker)
	}

	tip := head
	for len(markers) > 0 {
		if tip.Sequence == ^uint64(0) {
			return PublishedGeneration{}, errors.New("pack publication sequence cannot advance")
		}
		expectedSequence := tip.Sequence + 1
		next := -1
		for index, marker := range markers {
			if marker.Sequence != expectedSequence || marker.PreviousGeneration != tip.Generation {
				continue
			}
			if tip.StoreID != "" && marker.StoreID != tip.StoreID {
				return PublishedGeneration{}, fmt.Errorf("pack publication successor %s belongs to another store", marker.Generation)
			}
			if next != -1 {
				return PublishedGeneration{}, fmt.Errorf("multiple pack publication successors of generation %s", tip.Generation)
			}
			next = index
		}
		if next == -1 {
			return PublishedGeneration{}, fmt.Errorf("pack publication markers do not form a complete chain after generation %s", tip.Generation)
		}
		tip = markers[next]
		markers = append(markers[:next], markers[next+1:]...)
	}
	return tip, nil
}

func promotePublishedGenerationLocked(ctx context.Context, storeDir string, marker PublishedGeneration, updateHead bool) (string, error) {
	root := filepath.Join(filepath.Clean(storeDir), "packs")
	directory := filepath.Join(root, marker.Generation)
	if _, err := readPublishedGeneration(directory); err != nil {
		return "", fmt.Errorf("latest published pack generation %s is not recoverable: %w", marker.Generation, err)
	}
	catalog, err := VerifyRecovery(ctx, storeDir, marker.Generation)
	if err != nil {
		return "", fmt.Errorf("latest published pack generation %s is not recoverable: %w", marker.Generation, err)
	}
	if err := repairGenerationIndex(directory); err != nil {
		return "", err
	}
	if err := verifyGeneration(ctx, directory); err != nil {
		return "", err
	}
	resolver, err := openGeneration(directory, 0, false)
	if err != nil {
		return "", fmt.Errorf("open authoritative pack generation: %w", err)
	}
	verifyErr := verifyRecoveryAndLiveManifests(ctx, storeDir, directory, catalog, resolver)
	closeErr := resolver.Close()
	if err := errors.Join(verifyErr, closeErr); err != nil {
		return "", err
	}
	if updateHead {
		if err := publishPublicationHead(root, marker); err != nil {
			return "", err
		}
	}
	if err := publishCurrent(root, marker.Generation); err != nil {
		return "", err
	}
	return marker.Generation, nil
}

func recoverCurrentGenerationLocked(ctx context.Context, storeDir string) (string, error) {
	root := filepath.Join(filepath.Clean(storeDir), "packs")
	head, selectedMarker, err := readPublicationHead(root)
	if err != nil {
		return "", fmt.Errorf("read authoritative pack publication head: %w", err)
	}
	selected := head.Generation
	if selectedMarker.Generation != selected || selectedMarker.Sequence != head.Sequence {
		return "", errors.New("authoritative pack publication identity changed during recovery")
	}
	successor, found, err := findPublishedSuccessor(root, head)
	if err != nil {
		return "", err
	}
	if found {
		return promotePublishedGenerationLocked(ctx, storeDir, successor, true)
	}
	return promotePublishedGenerationLocked(ctx, storeDir, selectedMarker, false)
}

func currentOrRecoveredGeneration(ctx context.Context, storeDir string) (string, error) {
	generation, err := CurrentGeneration(storeDir)
	if err == nil {
		packsDir := filepath.Join(filepath.Clean(storeDir), "packs")
		if _, statErr := os.Lstat(filepath.Join(packsDir, publicationHeadFilename)); errors.Is(statErr, os.ErrNotExist) {
			return generation, nil
		} else if statErr != nil {
			return generation, nil
		}
		head, marker, headErr := readPublicationHead(packsDir)
		if headErr != nil {
			return generation, nil
		}
		if head.Generation == generation {
			return generation, nil
		}
		if marker.PreviousGeneration != generation {
			return generation, nil
		}
		lock, lockErr := storage.AcquireOperationLock(storeDir, "objects")
		if errors.Is(lockErr, storage.ErrOperationLockHeld) {
			return generation, nil
		}
		if lockErr != nil {
			return generation, nil
		}
		defer lock.Close()
		current, currentErr := CurrentGeneration(storeDir)
		currentHead, currentMarker, currentHeadErr := readPublicationHead(packsDir)
		if currentErr == nil && currentHeadErr == nil && currentHead.Generation == current {
			return current, nil
		}
		if currentErr != nil || currentHeadErr != nil || currentMarker.PreviousGeneration != current {
			return generation, nil
		}
		return recoverCurrentGenerationLocked(ctx, storeDir)
	}
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrInvalidCurrent) {
		return "", err
	}
	return RecoverCurrentGeneration(ctx, storeDir)
}

// currentPublicationForBuild must be called while the objects operation lock is held.
func currentPublicationForBuild(ctx context.Context, storeDir string) (string, uint64, error) {
	packsDir := filepath.Join(filepath.Clean(storeDir), "packs")
	generation, currentErr := CurrentGeneration(storeDir)
	_, headStatErr := os.Lstat(filepath.Join(packsDir, publicationHeadFilename))
	if errors.Is(headStatErr, os.ErrNotExist) {
		if errors.Is(currentErr, os.ErrNotExist) {
			return "", 0, nil
		}
		if currentErr != nil {
			return "", 0, currentErr
		}
		migrated, sequence, err := migrateLegacyCurrentPublication(ctx, storeDir, generation)
		if err != nil {
			return "", 0, err
		}
		return migrated, sequence, nil
	}
	if headStatErr != nil {
		return "", 0, headStatErr
	}
	head, marker, err := readPublicationHead(packsDir)
	if err != nil {
		return "", 0, err
	}
	if currentErr == nil && generation == head.Generation {
		successor, found, successorErr := findPublishedSuccessor(packsDir, head)
		if successorErr != nil {
			return "", 0, successorErr
		}
		if found {
			recovered, err := promotePublishedGenerationLocked(ctx, storeDir, successor, true)
			if err != nil {
				return "", 0, err
			}
			recoveredHead, recoveredMarker, err := readPublicationHead(packsDir)
			if err != nil {
				return "", 0, fmt.Errorf("reread recovered pack publication head: %w", err)
			}
			if recoveredHead.Generation != recovered || recoveredMarker.Generation != recoveredHead.Generation || recoveredMarker.Sequence != recoveredHead.Sequence {
				return "", 0, errors.New("recovered pack publication identity changed after promotion")
			}
			return recoveredHead.Generation, recoveredHead.Sequence, nil
		}
		return generation, head.Sequence, nil
	}
	if currentErr == nil && marker.PreviousGeneration != generation {
		return "", 0, fmt.Errorf("pack CURRENT %s and publication head %s do not form a forward publication step", generation, head.Generation)
	}
	if currentErr != nil && !errors.Is(currentErr, os.ErrNotExist) && !errors.Is(currentErr, ErrInvalidCurrent) {
		return "", 0, currentErr
	}
	recovered, err := recoverCurrentGenerationLocked(ctx, storeDir)
	if err != nil {
		return "", 0, err
	}
	recoveredHead, recoveredMarker, err := readPublicationHead(packsDir)
	if err != nil {
		return "", 0, fmt.Errorf("reread recovered pack publication head: %w", err)
	}
	if recoveredHead.Generation != recovered || recoveredMarker.Generation != recoveredHead.Generation || recoveredMarker.Sequence != recoveredHead.Sequence {
		return "", 0, errors.New("recovered pack publication identity changed after recovery")
	}
	return recoveredHead.Generation, recoveredHead.Sequence, nil
}

// migrateLegacyCurrentPublication establishes v2 publication proof for a
// generation selected by the pre-head CURRENT protocol. The objects lock must
// already be held by the caller.
func migrateLegacyCurrentPublication(ctx context.Context, storeDir string, generation string) (string, uint64, error) {
	if !safeGeneration(generation) {
		return "", 0, errors.New("safe legacy pack generation is required")
	}
	packsDir := filepath.Join(filepath.Clean(storeDir), "packs")
	directory := filepath.Join(packsDir, generation)
	if err := verifyGeneration(ctx, directory); err != nil {
		return "", 0, fmt.Errorf("verify legacy current pack generation: %w", err)
	}
	catalog, err := VerifyRecovery(ctx, storeDir, generation)
	if errors.Is(err, os.ErrNotExist) {
		index, indexErr := openIndexV3(directory)
		if indexErr != nil {
			return "", 0, fmt.Errorf("open legacy current pack index: %w", indexErr)
		}
		meta := index.meta
		closeErr := index.close()
		if closeErr != nil {
			return "", 0, closeErr
		}
		if err := writeRecoveryArchive(storeDir, directory, meta); err != nil {
			return "", 0, fmt.Errorf("create legacy current recovery archive: %w", err)
		}
		catalog, err = VerifyRecovery(ctx, storeDir, generation)
	}
	if err != nil {
		return "", 0, fmt.Errorf("verify legacy current recovery archive: %w", err)
	}
	resolver, err := openGeneration(directory, 0, false)
	if err != nil {
		return "", 0, err
	}
	verifyErr := verifyRecoveryAndLiveManifests(ctx, storeDir, directory, catalog, resolver)
	closeErr := resolver.Close()
	if err := errors.Join(verifyErr, closeErr); err != nil {
		return "", 0, fmt.Errorf("verify legacy current live manifests: %w", err)
	}
	current, err := CurrentGeneration(storeDir)
	if err != nil || current != generation {
		return "", 0, fmt.Errorf("pack CURRENT changed during publication migration: current=%q err=%v", current, err)
	}
	markerPath := filepath.Join(directory, publishedMarkerFilename)
	marker, markerErr := readPublishedGeneration(directory)
	if errors.Is(markerErr, os.ErrNotExist) {
		entries, err := os.ReadDir(packsDir)
		if err != nil {
			return "", 0, err
		}
		for _, entry := range entries {
			if !entry.IsDir() || entry.Name() == generation || !safeGeneration(entry.Name()) {
				continue
			}
			if _, err := os.Lstat(filepath.Join(packsDir, entry.Name(), publishedMarkerFilename)); err == nil {
				return "", 0, fmt.Errorf("cannot migrate legacy CURRENT while generation %s has publication metadata", entry.Name())
			} else if !errors.Is(err, os.ErrNotExist) {
				return "", 0, err
			}
		}
		marker, err = markGenerationPublished(directory, 1, "")
		if err != nil {
			return "", 0, fmt.Errorf("publish migrated pack generation marker: %w", err)
		}
	} else if markerErr != nil {
		if _, statErr := os.Lstat(markerPath); statErr == nil {
			return "", 0, fmt.Errorf("refusing to overwrite existing legacy publication marker: %w", markerErr)
		}
		return "", 0, markerErr
	}
	tip, err := findPublishedChainTip(packsDir, marker)
	if err != nil {
		return "", 0, err
	}
	if tip.Generation != marker.Generation {
		promoted, err := promotePublishedGenerationLocked(ctx, storeDir, tip, true)
		if err != nil {
			return "", 0, err
		}
		head, promotedMarker, err := readPublicationHead(packsDir)
		if err != nil {
			return "", 0, fmt.Errorf("reread migrated pack publication head: %w", err)
		}
		if head.Generation != promoted || promotedMarker.Generation != head.Generation || promotedMarker.Sequence != head.Sequence {
			return "", 0, errors.New("migrated pack publication identity changed after promotion")
		}
		return head.Generation, head.Sequence, nil
	}
	if err := publishPublicationHead(packsDir, marker); err != nil {
		return "", 0, fmt.Errorf("publish migrated pack generation head: %w", err)
	}
	return marker.Generation, marker.Sequence, nil
}

func openRecoveryArchive(generationDir string) (*tar.Reader, func() error, error) {
	root, err := openPackGenerationRoot(generationDir)
	if err != nil {
		return nil, nil, err
	}
	file, err := openPackGenerationFile(root, recoveryArchiveFilename)
	if err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	if err := root.Close(); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	decoder, err := zstd.NewReader(file, zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxMemory(512<<20))
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("open pack recovery decoder: %w", err)
	}
	closeArchive := func() error {
		decoder.Close()
		return file.Close()
	}
	return tar.NewReader(decoder), closeArchive, nil
}

func validateRecoveryCatalog(catalog RecoveryCatalog, directoryName string) error {
	directoryMatches := catalog.Generation == directoryName
	if strings.HasPrefix(directoryName, ".generation-") {
		directoryMatches = catalog.Generation == "gen-"+strings.TrimPrefix(directoryName, ".generation-")
	}
	if catalog.Version != recoveryVersion || catalog.Kind != recoveryKind || !directoryMatches || !safeGeneration(catalog.Generation) || catalog.CreatedAt == "" {
		return errors.New("invalid pack recovery catalog")
	}
	if _, err := time.Parse(time.RFC3339Nano, catalog.CreatedAt); err != nil {
		return errors.New("invalid pack recovery creation time")
	}
	seen := make(map[string]struct{})
	for _, identity := range catalog.PackFiles {
		if filepath.Base(identity.Path) != identity.Path {
			return fmt.Errorf("invalid nested recovery pack file %q", identity.Path)
		}
	}
	for _, identity := range append(append([]RecoveryFile(nil), catalog.Files...), catalog.PackFiles...) {
		if !safeRecoveryPath(identity.Path) || identity.Bytes < 0 || len(identity.SHA256) != sha256.Size*2 {
			return fmt.Errorf("invalid pack recovery file %q", identity.Path)
		}
		if _, duplicate := seen[identity.Path]; duplicate {
			return fmt.Errorf("duplicate pack recovery file %q", identity.Path)
		}
		seen[identity.Path] = struct{}{}
	}
	return nil
}

func verifyRecoveryDiskFile(root *os.Root, name string, identity RecoveryFile) error {
	file, err := openPackGenerationFile(root, name)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written != identity.Bytes || hex.EncodeToString(hasher.Sum(nil)) != identity.SHA256 {
		return errors.New("size or SHA-256 mismatch")
	}
	return nil
}

func safeRecoveryPath(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.Contains(path, "\\") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	return clean == path && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

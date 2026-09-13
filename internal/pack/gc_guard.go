package pack

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/storage"
)

type survivingObjectReader struct {
	loose  *fold.ObjectStore
	packed *Resolver
}

type generationRemovalGuard struct {
	storeDir   string
	storeID    string
	current    string
	managed    *storage.ManagedSessionDeletionGuard
	objectLock *storage.OperationLock
	resolver   *Resolver
	reader     survivingObjectReader
	candidate  storage.GCCandidate
	proof      storage.ExactRemovalProof
}

func (r survivingObjectReader) OpenObject(ctx context.Context, ref fold.ObjectRef) (io.ReadCloser, error) {
	if r.loose.HasObject(ref) {
		return r.loose.OpenObject(ctx, ref)
	}
	return r.packed.OpenObject(ctx, ref)
}

func (r survivingObjectReader) HasObject(ref fold.ObjectRef) bool {
	return r.loose.HasObject(ref) || r.packed.HasObject(ref)
}

// AuthorizeGenerationRemoval proves that live manifests remain fully
// reconstructable from loose objects and the current pack while holding every
// writer and object mutation lock through the caller's deletion.
func AuthorizeGenerationRemoval(ctx context.Context, storeDir string, candidate storage.GCCandidate) (storage.PackGenerationRemovalGuard, error) {
	if candidate.Kind != storage.CandidatePackGeneration {
		return nil, errors.New("pack-generation authorization requires a pack candidate")
	}
	storeDir = filepath.Clean(storeDir)
	expectedRoot := filepath.Join(storeDir, "packs")
	if filepath.Dir(filepath.Clean(candidate.Path)) != expectedRoot || !safeGeneration(filepath.Base(candidate.Path)) {
		return nil, errors.New("pack-generation candidate is outside the pack store")
	}
	// Older generations may predate durable publication identities. They
	// cannot be deleted automatically, but must not block unrelated cleanup.
	// Malformed or mismatched identities still fail the full checks below.
	if _, err := os.Lstat(filepath.Join(candidate.Path, "published.json")); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	managed, err := storage.AcquireManagedSessionDeletionGuard(ctx, storeDir)
	if err != nil {
		return nil, fmt.Errorf("lock managed sessions before pack cleanup: %w", err)
	}
	objectLock, err := storage.AcquireOperationLock(storeDir, "objects")
	if err != nil {
		_ = managed.Close()
		return nil, fmt.Errorf("lock object store before pack cleanup: %w", err)
	}
	current, err := CurrentGeneration(storeDir)
	if err != nil {
		_ = objectLock.Close()
		_ = managed.Close()
		return nil, fmt.Errorf("read current pack before cleanup: %w", err)
	}
	if filepath.Base(candidate.Path) == current {
		_ = objectLock.Close()
		_ = managed.Close()
		return nil, errors.New("current pack generation cannot be removed")
	}
	storeIdentity, err := readPackStoreIdentity(expectedRoot)
	if err != nil {
		_ = objectLock.Close()
		_ = managed.Close()
		return nil, fmt.Errorf("read durable pack store identity before cleanup: %w", err)
	}
	head, currentMarker, err := readPublicationHead(expectedRoot)
	if err != nil {
		_ = objectLock.Close()
		_ = managed.Close()
		return nil, fmt.Errorf("read pack publication head before cleanup: %w", err)
	}
	if head.Generation != current {
		_ = objectLock.Close()
		_ = managed.Close()
		return nil, fmt.Errorf("pack publication head %s differs from CURRENT %s", head.Generation, current)
	}
	if head.StoreID != storeIdentity.StoreID || currentMarker.StoreID != storeIdentity.StoreID {
		_ = objectLock.Close()
		_ = managed.Close()
		return nil, errors.New("authoritative pack publication is not bound to the durable store identity")
	}
	if _, found, err := findPublishedSuccessor(expectedRoot, head); err != nil || found {
		_ = objectLock.Close()
		_ = managed.Close()
		if err != nil {
			return nil, fmt.Errorf("inspect pending pack publication before cleanup: %w", err)
		}
		return nil, errors.New("pending pack publication prevents generation cleanup")
	}
	resolver, err := openGeneration(filepath.Join(expectedRoot, current), 0, false)
	if err != nil {
		_ = objectLock.Close()
		_ = managed.Close()
		return nil, fmt.Errorf("open surviving current pack: %w", err)
	}
	guard := &generationRemovalGuard{
		storeDir: storeDir, storeID: storeIdentity.StoreID, current: current, managed: managed, objectLock: objectLock, resolver: resolver,
		reader: survivingObjectReader{loose: fold.NewObjectStore(storeDir), packed: resolver}, candidate: candidate,
	}
	if err := validatePackGenerationRemovalOwnership(ctx, candidate, guard.storeID); err != nil {
		return nil, errors.Join(err, guard.Close())
	}
	guard.proof, err = storage.CaptureExactRemovalProof(ctx, candidate, "pack-generation-removal:"+filepath.Base(candidate.Path))
	if err != nil {
		return nil, errors.Join(err, guard.Close())
	}
	if err := guard.Revalidate(ctx); err != nil {
		return nil, errors.Join(err, guard.Close())
	}
	return guard, nil
}

func (g *generationRemovalGuard) Revalidate(ctx context.Context) error {
	if err := g.revalidateSurvivingStorage(ctx); err != nil {
		return err
	}
	if err := validatePackGenerationRemovalOwnership(ctx, g.candidate, g.storeID); err != nil {
		return err
	}
	proof, err := storage.CaptureExactRemovalProof(ctx, g.candidate, g.proof.OperationID)
	if err != nil {
		return err
	}
	if proof != g.proof {
		return errors.New("pack generation candidate changed after exact ownership proof")
	}
	return nil
}

func (g *generationRemovalGuard) RevalidateStaged(ctx context.Context, candidate storage.GCCandidate) error {
	if err := g.revalidateSurvivingStorage(ctx); err != nil {
		return err
	}
	if err := g.validateStagedCandidate(candidate); err != nil {
		return err
	}
	proof, err := storage.CaptureExactRemovalProof(ctx, candidate, g.proof.OperationID)
	if err != nil {
		return err
	}
	if proof.Files != g.proof.Files || proof.ApparentBytes != g.proof.ApparentBytes || proof.TreeSHA256 != g.proof.TreeSHA256 {
		return errors.New("staged pack generation differs from its exact ownership proof")
	}
	return nil
}

func (g *generationRemovalGuard) revalidateSurvivingStorage(ctx context.Context) error {
	if g == nil || g.managed == nil || g.objectLock == nil || g.resolver == nil {
		return errors.New("pack-generation removal guard is closed")
	}
	current, err := CurrentGeneration(g.storeDir)
	if err != nil {
		return err
	}
	if current != g.current || g.resolver.Generation() != g.current {
		return fmt.Errorf("pack CURRENT changed during deletion proof from %s to %s", g.current, current)
	}
	packsDir := filepath.Join(g.storeDir, "packs")
	storeIdentity, err := readPackStoreIdentity(packsDir)
	if err != nil {
		return fmt.Errorf("refresh durable pack store identity during deletion proof: %w", err)
	}
	if storeIdentity.StoreID != g.storeID {
		return errors.New("durable pack store identity changed during deletion proof")
	}
	head, currentMarker, err := readPublicationHead(packsDir)
	if err != nil {
		return fmt.Errorf("refresh pack publication head during deletion proof: %w", err)
	}
	if head.Generation != current {
		return fmt.Errorf("pack publication head changed during deletion proof from %s to %s", current, head.Generation)
	}
	if head.StoreID != g.storeID || currentMarker.StoreID != g.storeID {
		return errors.New("authoritative pack publication store identity changed during deletion proof")
	}
	if _, found, err := findPublishedSuccessor(packsDir, head); err != nil || found {
		if err != nil {
			return err
		}
		return errors.New("pending pack publication appeared during deletion proof")
	}
	references, err := g.managed.Refresh(ctx, g.storeDir)
	if err != nil {
		return err
	}
	report, err := fold.DoctorWithOptions(ctx, g.storeDir, fold.DoctorOptions{Reader: g.reader})
	if err != nil {
		return fmt.Errorf("verify surviving manifest bytes: %w", err)
	}
	if report.IssueCount != 0 || report.VerifiedManifestCount != report.ManifestCount {
		return fmt.Errorf("surviving storage cannot reconstruct %d of %d manifest(s)", report.ManifestCount-report.VerifiedManifestCount, report.ManifestCount)
	}
	for _, reference := range references {
		if err := storage.ValidateManagedSessionReference(reference); err != nil {
			return fmt.Errorf("verify exact managed manifest %s: %w", reference.SessionID, err)
		}
		manifest, err := fold.LoadManifestPath(reference.ManifestPath)
		if err != nil {
			return fmt.Errorf("load managed manifest %s: %w", reference.SessionID, err)
		}
		if manifest.Session.ID != reference.SessionID || manifest.Source.Bytes != reference.BaseBytes || manifest.Source.SHA256 != reference.BaseSHA256 {
			return fmt.Errorf("managed manifest %s changed during pack cleanup proof", reference.SessionID)
		}
		if err := fold.VerifyManifest(ctx, g.reader, manifest); err != nil {
			return fmt.Errorf("reconstruct managed manifest %s without candidate pack: %w", reference.SessionID, err)
		}
	}
	return nil
}

func (g *generationRemovalGuard) validateStagedCandidate(candidate storage.GCCandidate) error {
	if candidate.Kind != g.candidate.Kind || candidate.Files != g.candidate.Files || candidate.ApparentBytes != g.candidate.ApparentBytes {
		return errors.New("staged pack generation metadata differs from the authorized candidate")
	}
	stagingRoot := filepath.Join(g.storeDir, "packs", ".gc-staging")
	relative, err := filepath.Rel(stagingRoot, filepath.Clean(candidate.Path))
	if err != nil {
		return err
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) != 2 || len(parts[0]) != 32 || filepath.Base(parts[0]) != parts[0] || parts[1] != filepath.Base(g.candidate.Path) {
		return errors.New("pack generation was not moved to canonical deletion staging")
	}
	if token, err := hex.DecodeString(parts[0]); err != nil || len(token) != 16 {
		return errors.New("pack generation deletion staging token is invalid")
	}
	return nil
}

func validatePackGenerationRemovalOwnership(ctx context.Context, candidate storage.GCCandidate, expectedStoreID string) error {
	directory := filepath.Clean(candidate.Path)
	marker, err := readPublishedGeneration(directory)
	if err != nil {
		return fmt.Errorf("pack generation has no exact publication identity: %w", err)
	}
	if marker.Generation != filepath.Base(directory) {
		return errors.New("pack generation publication identity names another directory")
	}
	if marker.Version != publishedVersion || marker.Kind != publishedKind || marker.StoreID != expectedStoreID || !validPackStoreID(marker.StoreID) {
		return errors.New("pack generation is not bound to this store's durable identity")
	}
	catalog, err := VerifyRecoveryDirectory(ctx, filepath.Dir(filepath.Dir(directory)), directory)
	if err != nil {
		return fmt.Errorf("pack generation recovery identity is incomplete: %w", err)
	}
	if catalog.Generation != marker.Generation {
		return errors.New("pack generation recovery identity names another generation")
	}
	root, err := openPackGenerationRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	expected := make(map[string]RecoveryFile)
	for _, identity := range catalog.PackFiles {
		expected[identity.Path] = identity
	}
	for _, identity := range catalog.Files {
		switch identity.Path {
		case indexV3MetaFilename, indexV3ObjectsFile, indexV3BlocksFile:
			expected[identity.Path] = identity
		}
	}
	for name, identity := range expected {
		if filepath.Base(name) != name {
			return fmt.Errorf("pack generation owns a nested runtime file: %s", name)
		}
		if err := verifyRecoveryDiskFile(root, name, identity); err != nil {
			return fmt.Errorf("verify exact pack generation file %s: %w", name, err)
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink prevents pack generation removal: %s", entry.Name())
		}
		if _, ok := expected[entry.Name()]; ok {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("owned pack generation file has an unsafe type: %s", entry.Name())
			}
			continue
		}
		switch entry.Name() {
		case recoveryArchiveFilename, publishedMarkerFilename:
			if !info.Mode().IsRegular() {
				return fmt.Errorf("pack publication file has an unsafe type: %s", entry.Name())
			}
		case "leases":
			if !info.IsDir() {
				return errors.New("pack lease path is not a plain directory")
			}
			active, err := storage.DirectoryHasActiveLease(path, false)
			if err != nil || active {
				if err == nil {
					err = errors.New("pack generation still has an active lease")
				}
				return err
			}
			if err := validatePackRemovalLeaseTree(path); err != nil {
				return err
			}
		default:
			if info.IsDir() {
				if err := validateEmptyPackRemovalTree(path); err != nil {
					return fmt.Errorf("unknown nonempty pack generation directory %s: %w", entry.Name(), err)
				}
				continue
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("special file prevents pack generation removal: %s", entry.Name())
			}
			if info.Size() != 0 {
				return fmt.Errorf("unknown nonempty pack generation content prevents removal: %s", entry.Name())
			}
		}
	}
	return nil
}

// validatePackRemovalLeaseTree runs only after the caller has proven with the
// lock that no lease in this directory is held. What remains is a lease leaked
// by a killed reader, which still carries the process-ID payload
// storage.AcquireLease writes. Rejecting that payload as unknown content would
// pin the generation forever, because the writer never produces an empty lease.
// Anything that is not a recognized lease artifact still fails closed.
func validatePackRemovalLeaseTree(root string) error {
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
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("unsafe pack lease content prevents generation removal: %s", path)
		}
		if info.Size() == 0 {
			continue
		}
		if !strings.HasPrefix(entry.Name(), storage.LeaseFilePrefix) || info.Size() > 64 {
			return fmt.Errorf("unknown nonempty pack lease content prevents generation removal: %s", path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !storage.RecognizedLeaseContent(content) {
			return fmt.Errorf("unknown nonempty pack lease content prevents generation removal: %s", path)
		}
	}
	return nil
}

func validateEmptyPackRemovalTree(root string) error {
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
			return fmt.Errorf("symlink prevents pack generation removal: %s", path)
		}
		if info.IsDir() {
			if err := validateEmptyPackRemovalTree(path); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("special file prevents pack generation removal: %s", path)
		}
		if info.Size() != 0 {
			return fmt.Errorf("unknown nonempty pack generation content prevents removal: %s", path)
		}
	}
	return nil
}

func (g *generationRemovalGuard) Close() error {
	if g == nil {
		return nil
	}
	var resolverErr, objectErr, managedErr error
	if g.resolver != nil {
		resolverErr = g.resolver.Close()
		g.resolver = nil
	}
	if g.objectLock != nil {
		objectErr = g.objectLock.Close()
		g.objectLock = nil
	}
	if g.managed != nil {
		managedErr = g.managed.Close()
		g.managed = nil
	}
	return errors.Join(resolverErr, objectErr, managedErr)
}

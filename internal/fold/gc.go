package fold

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samekind/codexfold/internal/storage"
)

type GCResult struct {
	StoreDir                  string                  `json:"store_dir"`
	DryRun                    bool                    `json:"dry_run"`
	Referenced                int                     `json:"referenced_objects"`
	OrphanCount               int                     `json:"orphan_count"`
	OrphanBytes               int64                   `json:"orphan_bytes"`
	RemovedCount              int                     `json:"removed_count"`
	RemovedBytes              int64                   `json:"removed_bytes"`
	Storage                   storage.StorageGCResult `json:"storage"`
	ProjectedReclaimableBytes int64                   `json:"projected_reclaimable_bytes"`
	ActualReclaimedBytes      int64                   `json:"actual_reclaimed_bytes"`
}

type GCOptions struct {
	Apply              bool
	BeforeObjectRemove func(string) error
	// Reader resolves manifest objects during verification. A pack-only store
	// keeps no loose copies, so without the pack resolver every managed
	// manifest verifies as invalid and GC refuses to run at all. Doctor and
	// unfold already accept the same injection.
	Reader ObjectReader
}

func GC(ctx context.Context, storeDir string, apply bool) (GCResult, error) {
	return GCWithOptions(ctx, storeDir, GCOptions{Apply: apply})
}

func GCWithOptions(ctx context.Context, storeDir string, options GCOptions) (GCResult, error) {
	apply := options.Apply
	result := GCResult{StoreDir: storeDir, DryRun: !apply}
	_, invalidManifests, err := referencedManifestObjects(ctx, storeDir, false, nil, options.Reader)
	if err != nil {
		return GCResult{}, err
	}
	if invalidManifests > 0 {
		return GCResult{}, fmt.Errorf("refusing GC with %d invalid manifest(s)", invalidManifests)
	}
	before, err := storage.Scan(ctx, storage.Options{StoreDir: storeDir, AllowMetadataIssues: true})
	if err != nil {
		return GCResult{}, err
	}
	storageResult, err := storage.Collect(ctx, storage.GCOptions{StoreDir: storeDir, Apply: apply})
	if err != nil {
		return GCResult{}, err
	}
	result.Storage = storageResult
	result.ProjectedReclaimableBytes = storageResult.ProjectedReclaimableBytes
	guard, err := storage.AcquireManagedSessionDeletionGuard(ctx, storeDir)
	if err != nil {
		return GCResult{}, fmt.Errorf("refusing loose-object GC without complete managed-session proof: %w", err)
	}
	defer guard.Close()
	var objectLock *storage.OperationLock
	if apply {
		objectLock, err = storage.AcquireOperationLock(storeDir, "objects")
		if err != nil {
			return GCResult{}, fmt.Errorf("lock loose-object GC: %w", err)
		}
		defer objectLock.Close()
	}
	referenced, invalidManifests, err := referencedManifestObjects(ctx, storeDir, true, guard.References, options.Reader)
	if err != nil {
		return GCResult{}, err
	}
	if invalidManifests > 0 {
		return GCResult{}, fmt.Errorf("refusing loose-object GC with %d invalid manifest(s)", invalidManifests)
	}
	result.Referenced = len(referenced)
	err = walkLooseObjectCandidates(storeDir, func(path string, info os.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		digest := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if err := ValidateCanonicalLooseObjectPath(storeDir, path, digest); err != nil {
			return fmt.Errorf("refusing noncanonical loose-object GC candidate: %w", err)
		}
		if _, ok := referenced[digest]; ok {
			return nil
		}
		result.OrphanCount++
		result.OrphanBytes += info.Size()
		if apply {
			initialIdentity, err := CaptureLooseObjectIdentity(path, digest, -1)
			if err != nil {
				return fmt.Errorf("refusing unproved loose-object GC candidate %s: %w", digest, err)
			}
			if options.BeforeObjectRemove != nil {
				if err := options.BeforeObjectRemove(path); err != nil {
					return err
				}
			}
			managed, err := guard.Refresh(ctx, storeDir)
			if err != nil {
				return fmt.Errorf("refresh managed-session deletion proof: %w", err)
			}
			currentReferences, invalid, err := referencedManifestObjects(ctx, storeDir, true, managed, options.Reader)
			if err != nil {
				return err
			}
			if invalid > 0 {
				return fmt.Errorf("refusing loose-object GC with %d invalid manifest(s)", invalid)
			}
			if _, nowReferenced := currentReferences[digest]; nowReferenced {
				return nil
			}
			finalIdentity, err := CaptureLooseObjectIdentity(path, digest, initialIdentity.RawBytes)
			if err != nil {
				return fmt.Errorf("revalidate loose-object GC candidate %s: %w", digest, err)
			}
			if !initialIdentity.Same(finalIdentity) {
				return fmt.Errorf("loose-object GC candidate %s changed after exact proof", digest)
			}
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove orphan object %s: %w", digest, err)
			}
			result.RemovedCount++
			result.RemovedBytes += finalIdentity.StoredBytes
		}
		return nil
	})
	if err != nil {
		return GCResult{}, err
	}
	result.ProjectedReclaimableBytes += result.OrphanBytes
	after, err := storage.Scan(ctx, storage.Options{StoreDir: storeDir, AllowMetadataIssues: true})
	if err != nil {
		return GCResult{}, err
	}
	if before.TotalPhysicalBytes > after.TotalPhysicalBytes {
		result.ActualReclaimedBytes = before.TotalPhysicalBytes - after.TotalPhysicalBytes
	}
	return result, nil
}

func walkLooseObjectCandidates(storeDir string, visit func(path string, info os.FileInfo) error) error {
	root := filepath.Join(storeDir, "objects")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".zst" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return visit(path, info)
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func referencedManifestObjects(ctx context.Context, storeDir string, collect bool, managed []storage.ManagedSessionReference, objects ObjectReader) (map[string]struct{}, int, error) {
	referenced := make(map[string]struct{})
	invalid := 0
	visited := make(map[string]struct{})
	err := walkManifestPaths(storeDir, func(path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		manifest, err := LoadManifestPath(path)
		if err != nil {
			invalid++
			return nil
		}
		visited[filepath.Clean(path)] = struct{}{}
		if collect {
			for _, part := range manifest.Parts {
				referenced[part.Object.SHA256] = struct{}{}
			}
		}
		return nil
	})
	if err != nil {
		return referenced, invalid, err
	}
	if !collect {
		return referenced, invalid, nil
	}
	reader := openObjectReader(storeDir, objects)
	for _, reference := range managed {
		path := filepath.Clean(reference.ManifestPath)
		if validationErr := storage.ValidateManagedSessionReference(reference); validationErr != nil {
			invalid++
			continue
		}
		manifest, loadErr := LoadManifestPath(path)
		if loadErr != nil || manifest.Session.ID != reference.SessionID || manifest.Source.Bytes != reference.BaseBytes || manifest.Source.SHA256 != reference.BaseSHA256 {
			invalid++
			continue
		}
		if verifyErr := verifyStoredManifest(ctx, reader, manifest); verifyErr != nil {
			invalid++
			continue
		}
		if _, ok := visited[path]; !ok {
			for _, part := range manifest.Parts {
				referenced[part.Object.SHA256] = struct{}{}
			}
		}
	}
	return referenced, invalid, err
}

package pack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/storage"
)

type RetireLooseOptions struct {
	Apply        bool
	BeforeRemove func(string) error
}

type RetireLooseResult struct {
	StoreDir              string `json:"store_dir"`
	Generation            string `json:"generation"`
	DryRun                bool   `json:"dry_run"`
	PackManifestCount     int    `json:"pack_manifest_count"`
	VerifiedManifestCount int    `json:"verified_manifest_count"`
	CandidateCount        int    `json:"candidate_count"`
	CandidateBytes        int64  `json:"candidate_bytes"`
	RetiredCount          int    `json:"retired_count"`
	RetiredBytes          int64  `json:"retired_bytes"`
	ActualReclaimedBytes  int64  `json:"actual_reclaimed_bytes"`
	AuditPath             string `json:"audit_path,omitempty"`
}

// RetireLoose removes only loose objects that the current verified pack can read.
func RetireLoose(ctx context.Context, storeDir string, options RetireLooseOptions) (RetireLooseResult, error) {
	result := RetireLooseResult{StoreDir: filepath.Clean(storeDir), DryRun: !options.Apply}
	guard, err := storage.AcquireManagedSessionDeletionGuard(ctx, storeDir)
	if err != nil {
		return RetireLooseResult{}, fmt.Errorf("refusing loose retirement without complete managed-session proof: %w", err)
	}
	defer guard.Close()
	lock, err := storage.AcquireOperationLock(storeDir, "objects")
	if err != nil {
		return RetireLooseResult{}, err
	}
	defer lock.Close()
	before, err := storage.Scan(ctx, storage.Options{StoreDir: storeDir, AllowMetadataIssues: true})
	if err != nil {
		return RetireLooseResult{}, err
	}
	resolver, err := Open(storeDir, OpenOptions{CacheBytes: -1})
	if err != nil {
		return RetireLooseResult{}, err
	}
	defer resolver.Close()
	result.Generation = resolver.Generation()
	packReport, err := Doctor(ctx, storeDir)
	if err != nil {
		return RetireLooseResult{}, err
	}
	if packReport.IssueCount != 0 {
		return RetireLooseResult{}, fmt.Errorf("refusing loose retirement: pack doctor reported %d issue(s)", packReport.IssueCount)
	}
	result.PackManifestCount = packReport.ManifestCount
	proof, err := fold.DoctorWithOptions(ctx, storeDir, fold.DoctorOptions{Reader: resolver})
	if err != nil {
		return RetireLooseResult{}, err
	}
	if proof.IssueCount != 0 || proof.ManifestCount != packReport.ManifestCount || proof.VerifiedManifestCount != proof.ManifestCount {
		return RetireLooseResult{}, errors.New("refusing loose retirement: pack-only fold verification is incomplete")
	}
	result.VerifiedManifestCount = proof.VerifiedManifestCount
	if _, err := verifyManagedSessionManifests(ctx, resolver, guard.References); err != nil {
		return RetireLooseResult{}, err
	}
	objectRoot := filepath.Join(storeDir, "objects")
	err = filepath.WalkDir(objectRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".zst" {
			return nil
		}
		digest := strings.TrimSuffix(entry.Name(), ".zst")
		if err := fold.ValidateCanonicalLooseObjectPath(storeDir, path, digest); err != nil {
			return fmt.Errorf("refusing noncanonical loose retirement candidate: %w", err)
		}
		packedObject, packed, err := resolver.lookupObject(digest)
		if err != nil {
			return err
		}
		if !packed {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		result.CandidateCount++
		result.CandidateBytes += info.Size()
		if !options.Apply {
			return nil
		}
		initialIdentity, err := fold.CaptureLooseObjectIdentity(path, digest, packedObject.RawBytes)
		if err != nil {
			return fmt.Errorf("refusing unproved loose retirement candidate %s: %w", digest, err)
		}
		if options.BeforeRemove != nil {
			if err := options.BeforeRemove(path); err != nil {
				return err
			}
		}
		current, err := CurrentGeneration(storeDir)
		if err != nil {
			return fmt.Errorf("refresh pack CURRENT before loose retirement: %w", err)
		}
		if current != resolver.Generation() {
			return fmt.Errorf("refusing loose retirement: pack CURRENT changed from %s to %s", resolver.Generation(), current)
		}
		managed, err := guard.Refresh(ctx, storeDir)
		if err != nil {
			return fmt.Errorf("refresh managed-session deletion proof: %w", err)
		}
		if len(managed) != len(guard.References) {
			return errors.New("managed-session deletion proof changed before loose retirement")
		}
		if err := verifyPackedLooseRetirementCandidate(ctx, resolver, digest, packedObject.RawBytes); err != nil {
			return err
		}
		finalIdentity, err := fold.CaptureLooseObjectIdentity(path, digest, packedObject.RawBytes)
		if err != nil {
			return fmt.Errorf("revalidate loose retirement candidate %s: %w", digest, err)
		}
		if !initialIdentity.Same(finalIdentity) {
			return fmt.Errorf("loose retirement candidate %s changed after exact proof", digest)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("retire loose object %s: %w", digest, err)
		}
		result.RetiredCount++
		result.RetiredBytes += finalIdentity.StoredBytes
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return RetireLooseResult{}, err
	}
	if !options.Apply {
		return result, nil
	}
	after, err := storage.Scan(ctx, storage.Options{StoreDir: storeDir, AllowMetadataIssues: true})
	if err != nil {
		return RetireLooseResult{}, err
	}
	if before.TotalPhysicalBytes > after.TotalPhysicalBytes {
		result.ActualReclaimedBytes = before.TotalPhysicalBytes - after.TotalPhysicalBytes
	}
	auditPath, err := writeRetireAudit(storeDir, result)
	if err != nil {
		return RetireLooseResult{}, err
	}
	result.AuditPath = auditPath
	return result, nil
}

func verifyPackedLooseRetirementCandidate(ctx context.Context, resolver *Resolver, digest string, rawBytes int64) error {
	reader, err := resolver.OpenObject(ctx, fold.ObjectRef{SHA256: digest, RawBytes: rawBytes})
	if err != nil {
		return fmt.Errorf("open packed loose retirement candidate %s: %w", digest, err)
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(hasher, reader)
	closeErr := reader.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("verify packed loose retirement candidate %s: %w", digest, err)
	}
	if written != rawBytes {
		return fmt.Errorf("packed loose retirement candidate %s bytes %d, want %d", digest, written, rawBytes)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != digest {
		return fmt.Errorf("packed loose retirement candidate %s SHA-256 mismatch", digest)
	}
	return nil
}

func verifyManagedSessionManifests(ctx context.Context, resolver *Resolver, references []storage.ManagedSessionReference) (int, error) {
	verified := 0
	for _, reference := range references {
		if err := storage.ValidateManagedSessionReference(reference); err != nil {
			return 0, fmt.Errorf("verify exact manifest for managed session %s: %w", reference.SessionID, err)
		}
		manifest, err := fold.LoadManifestPath(reference.ManifestPath)
		if err != nil {
			return 0, fmt.Errorf("load current manifest for managed session %s: %w", reference.SessionID, err)
		}
		if manifest.Session.ID != reference.SessionID || manifest.Source.Bytes != reference.BaseBytes || manifest.Source.SHA256 != reference.BaseSHA256 {
			return 0, fmt.Errorf("managed session %s state does not match its current manifest", reference.SessionID)
		}
		if err := fold.VerifyManifest(ctx, resolver, manifest); err != nil {
			return 0, fmt.Errorf("reconstruct managed session %s from current pack: %w", reference.SessionID, err)
		}
		verified++
	}
	return verified, nil
}

func writeRetireAudit(storeDir string, result RetireLooseResult) (string, error) {
	directory := filepath.Join(filepath.Clean(storeDir), "retirements")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(directory, fmt.Sprintf("loose-%s-%d.json", result.Generation, time.Now().UTC().UnixNano()))
	result.AuditPath = path
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

package pack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/storage"
)

type RetireLooseOptions struct {
	Apply bool
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
		if !resolver.HasDigest(digest) {
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
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("retire loose object %s: %w", digest, err)
		}
		result.RetiredCount++
		result.RetiredBytes += info.Size()
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

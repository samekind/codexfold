package fold

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/samekind/codexfold/internal/storage"
)

type UnfoldResult struct {
	SessionID    string                      `json:"session_id"`
	ManifestPath string                      `json:"manifest_path"`
	TargetPath   string                      `json:"target_path"`
	Bytes        int64                       `json:"bytes"`
	SHA256       string                      `json:"sha256"`
	Verified     bool                        `json:"verified"`
	Storage      *storage.MutationAccounting `json:"storage,omitempty"`
}

type UnfoldOptions struct {
	TargetPath string
	Overwrite  bool
	Budget     storage.Checker
	Reader     ObjectReader
}

func Unfold(ctx context.Context, storeDir string, sessionID string, targetPath string, overwrite bool) (UnfoldResult, error) {
	return UnfoldWithOptions(ctx, storeDir, sessionID, UnfoldOptions{TargetPath: targetPath, Overwrite: overwrite})
}

func UnfoldWithOptions(ctx context.Context, storeDir string, sessionID string, options UnfoldOptions) (UnfoldResult, error) {
	manifest, err := LoadManifest(storeDir, sessionID)
	if err != nil {
		return UnfoldResult{}, err
	}
	targetPath := options.TargetPath
	if targetPath == "" {
		targetPath = manifest.Session.RolloutPath
	}
	reclaimableBytes := int64(0)
	if info, err := os.Stat(targetPath); err == nil && !options.Overwrite {
		return UnfoldResult{}, fmt.Errorf("restore target already exists: %s", targetPath)
	} else if err == nil && info.Mode().IsRegular() {
		reclaimableBytes = info.Size()
	} else if err != nil && !os.IsNotExist(err) {
		return UnfoldResult{}, err
	}
	budget := options.Budget
	if budget == nil {
		guard, err := storage.DefaultGuard(storeDir)
		if err != nil {
			return UnfoldResult{}, err
		}
		budget = guard
	}
	storageAssessment, err := budget.Check(ctx, storage.Projection{
		Operation: "unfold", AdditionalPersistentBytes: manifest.Source.Bytes, TemporaryBytes: manifest.Source.Bytes,
		TemporaryPersistentOverlapBytes: manifest.Source.Bytes, ReclaimableBytes: reclaimableBytes,
	})
	if err != nil {
		return UnfoldResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return UnfoldResult{}, fmt.Errorf("create restore directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(targetPath), ".unfold-*.tmp")
	if err != nil {
		return UnfoldResult{}, fmt.Errorf("create restore temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	reader := openObjectReader(storeDir, options.Reader)
	hasher := sha256.New()
	var bytesWritten int64
	for _, part := range manifest.Parts {
		if err := ctx.Err(); err != nil {
			_ = temporary.Close()
			return UnfoldResult{}, err
		}
		stream, err := reader.OpenObject(ctx, part.Object)
		if err != nil {
			_ = temporary.Close()
			return UnfoldResult{}, err
		}
		written, copyErr := io.Copy(io.MultiWriter(temporary, hasher), stream)
		closeErr := stream.Close()
		if copyErr != nil {
			_ = temporary.Close()
			return UnfoldResult{}, fmt.Errorf("write restored object: %w", copyErr)
		}
		if closeErr != nil {
			_ = temporary.Close()
			return UnfoldResult{}, closeErr
		}
		bytesWritten += written
	}
	if err := verifySourceDigest(hasher, bytesWritten, manifest.Source); err != nil {
		_ = temporary.Close()
		return UnfoldResult{}, err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return UnfoldResult{}, fmt.Errorf("sync restored rollout: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return UnfoldResult{}, fmt.Errorf("close restored rollout: %w", err)
	}
	commit := os.Rename
	if options.Overwrite {
		commit = replaceFile
	}
	if err := commit(temporaryPath, targetPath); err != nil {
		return UnfoldResult{}, fmt.Errorf("commit restored rollout: %w", err)
	}
	if err := syncDirectory(filepath.Dir(targetPath)); err != nil {
		return UnfoldResult{}, err
	}
	return UnfoldResult{
		SessionID: sessionID, ManifestPath: ManifestPath(storeDir, sessionID),
		TargetPath: targetPath, Bytes: bytesWritten, SHA256: manifest.Source.SHA256, Verified: true,
		Storage: storage.CompleteAccounting(ctx, storageAssessment, storeDir),
	}, nil
}

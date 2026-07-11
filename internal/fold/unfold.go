package fold

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

type UnfoldResult struct {
	SessionID    string `json:"session_id"`
	ManifestPath string `json:"manifest_path"`
	TargetPath   string `json:"target_path"`
	Bytes        int64  `json:"bytes"`
	SHA256       string `json:"sha256"`
	Verified     bool   `json:"verified"`
}

func Unfold(ctx context.Context, storeDir string, sessionID string, targetPath string, overwrite bool) (UnfoldResult, error) {
	manifest, err := LoadManifest(storeDir, sessionID)
	if err != nil {
		return UnfoldResult{}, err
	}
	if targetPath == "" {
		targetPath = manifest.Session.RolloutPath
	}
	if _, err := os.Stat(targetPath); err == nil && !overwrite {
		return UnfoldResult{}, fmt.Errorf("restore target already exists: %s", targetPath)
	} else if err != nil && !os.IsNotExist(err) {
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
	store := NewObjectStore(storeDir)
	hasher := sha256.New()
	var bytesWritten int64
	for _, part := range manifest.Parts {
		if err := ctx.Err(); err != nil {
			_ = temporary.Close()
			return UnfoldResult{}, err
		}
		data, err := store.Read(part.Object)
		if err != nil {
			_ = temporary.Close()
			return UnfoldResult{}, err
		}
		if _, err := temporary.Write(data); err != nil {
			_ = temporary.Close()
			return UnfoldResult{}, fmt.Errorf("write restored object: %w", err)
		}
		_, _ = hasher.Write(data)
		bytesWritten += int64(len(data))
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
	if overwrite {
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
	}, nil
}

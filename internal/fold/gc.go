package fold

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type GCResult struct {
	StoreDir     string `json:"store_dir"`
	DryRun       bool   `json:"dry_run"`
	Referenced   int    `json:"referenced_objects"`
	OrphanCount  int    `json:"orphan_count"`
	OrphanBytes  int64  `json:"orphan_bytes"`
	RemovedCount int    `json:"removed_count"`
	RemovedBytes int64  `json:"removed_bytes"`
}

func GC(ctx context.Context, storeDir string, apply bool) (GCResult, error) {
	result := GCResult{StoreDir: storeDir, DryRun: !apply}
	manifests, issues, err := loadAllManifests(storeDir)
	if err != nil {
		return GCResult{}, err
	}
	if len(issues) > 0 {
		return GCResult{}, fmt.Errorf("refusing GC with %d invalid manifest(s)", len(issues))
	}
	referenced := make(map[string]struct{})
	for _, manifest := range manifests {
		for _, part := range manifest.Parts {
			referenced[part.Object.SHA256] = struct{}{}
		}
	}
	result.Referenced = len(referenced)
	err = walkObjectFiles(storeDir, func(path string, info os.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		digest := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if _, ok := referenced[digest]; ok {
			return nil
		}
		result.OrphanCount++
		result.OrphanBytes += info.Size()
		if apply {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove orphan object %s: %w", digest, err)
			}
			result.RemovedCount++
			result.RemovedBytes += info.Size()
		}
		return nil
	})
	if err != nil {
		return GCResult{}, err
	}
	return result, nil
}

package mountfs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"
)

type NativePreflightReport struct {
	Files            int    `json:"files"`
	Bytes            int64  `json:"bytes"`
	ValidatedFiles   int    `json:"validated_files"`
	IncrementalFiles int    `json:"incremental_files"`
	CachedFiles      int    `json:"cached_files"`
	ValidatedBytes   int64  `json:"validated_bytes"`
	CachePath        string `json:"cache_path,omitempty"`
	CacheRebuilt     bool   `json:"cache_rebuilt,omitempty"`
}

func (f *Filesystem) ValidateNativeWriterRollouts(ctx context.Context) (NativePreflightReport, error) {
	f.mu.RLock()
	root := f.nativeRoot
	f.mu.RUnlock()
	if root == "" {
		return NativePreflightReport{}, nil
	}
	return validateNativeWriterRollouts(ctx, root)
}

func validateNativeWriterRollouts(ctx context.Context, nativeRoot string) (NativePreflightReport, error) {
	return validateNativeWriterRolloutsCached(ctx, nativeRoot)
}

func ValidateNativeRollout(ctx context.Context, filePath string) (int64, error) {
	path := filepath.Clean(filePath)
	before, err := os.Lstat(path)
	if err != nil {
		return 0, fmt.Errorf("inspect native rollout %s: %w", path, err)
	}
	if !before.Mode().IsRegular() {
		return 0, fmt.Errorf("native rollout %s is not a regular file", path)
	}
	validated, err := validateNativeJSONL(ctx, path)
	if err != nil {
		return validated, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return validated, fmt.Errorf("reinspect native rollout %s: %w", path, err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		return validated, fmt.Errorf("native rollout changed during validation: %s", path)
	}
	return validated, nil
}

func validateNativeJSONL(ctx context.Context, filePath string) (int64, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, err
	}
	reader := bufio.NewReaderSize(file, 1<<20)
	var lineNumber int64
	var bytesRead int64
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return bytesRead, err
		}
		line, readErr := reader.ReadBytes('\n')
		bytesRead += int64(len(line))
		if len(line) != 0 {
			lineNumber++
		}
		if errors.Is(readErr, io.EOF) {
			if len(line) != 0 {
				_ = file.Close()
				return bytesRead, fmt.Errorf("native writer preflight %s line %d is missing its final newline", filePath, lineNumber)
			}
			break
		}
		if readErr != nil {
			_ = file.Close()
			return bytesRead, fmt.Errorf("native writer preflight read %s line %d: %w", filePath, lineNumber, readErr)
		}
		if !utf8.Valid(line) {
			_ = file.Close()
			return bytesRead, fmt.Errorf("native writer preflight %s line %d is not valid UTF-8", filePath, lineNumber)
		}
		if !json.Valid(line) {
			_ = file.Close()
			return bytesRead, fmt.Errorf("native writer preflight %s line %d is not valid JSON", filePath, lineNumber)
		}
	}
	if err := file.Close(); err != nil {
		return bytesRead, err
	}
	return bytesRead, nil
}

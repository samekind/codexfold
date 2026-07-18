package mountfs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	nativePreflightCacheVersion = 1
	nativeFingerprintWindow     = 64 << 10
)

type nativePreflightCache struct {
	Version int                             `json:"version"`
	Entries map[string]nativePreflightEntry `json:"entries"`
}

type nativePreflightEntry struct {
	Size       int64  `json:"size"`
	ModTimeNS  int64  `json:"mod_time_ns"`
	HeadSHA256 string `json:"head_sha256"`
	TailSHA256 string `json:"tail_sha256"`
}

func validateNativeWriterRolloutsCached(ctx context.Context, nativeRoot string) (NativePreflightReport, error) {
	root := filepath.Clean(nativeRoot)
	activeRoot := filepath.Join(root, "sessions")
	cachePath := filepath.Join(root, ".codexfold-native-preflight-v1.json")
	cache, cacheBytes, rebuilt := loadNativePreflightCache(cachePath)
	next := nativePreflightCache{Version: nativePreflightCacheVersion, Entries: make(map[string]nativePreflightEntry)}
	report := NativePreflightReport{CachePath: cachePath, CacheRebuilt: rebuilt}

	err := filepath.WalkDir(activeRoot, func(filePath string, directoryEntry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if directoryEntry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("native writer preflight rejects symlink %s", filePath)
		}
		if directoryEntry.IsDir() {
			return nil
		}
		if !directoryEntry.Type().IsRegular() {
			return fmt.Errorf("native writer preflight rejects non-regular file %s", filePath)
		}
		if strings.HasPrefix(directoryEntry.Name(), "._") || !strings.HasSuffix(directoryEntry.Name(), ".jsonl") {
			return nil
		}
		info, err := directoryEntry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, filePath)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("native writer preflight path escaped native root: %s", filePath)
		}
		key := filepath.ToSlash(relative)
		current := nativePreflightEntry{Size: info.Size(), ModTimeNS: info.ModTime().UnixNano()}
		report.Files++
		report.Bytes += info.Size()

		cached, exists := cache.Entries[key]
		if exists && cached.Size == current.Size && cached.ModTimeNS == current.ModTimeNS {
			next.Entries[key] = cached
			report.CachedFiles++
			return nil
		}

		var validatedBytes int64
		incremental := exists && current.Size > cached.Size && nativePrefixMatches(filePath, cached)
		if incremental {
			validatedBytes, err = validateNativeJSONLFrom(ctx, filePath, cached.Size)
		} else {
			validatedBytes, err = validateNativeJSONL(ctx, filePath)
		}
		if err != nil {
			return err
		}
		after, err := os.Stat(filePath)
		if err != nil {
			return err
		}
		if after.Size() != current.Size || after.ModTime().UnixNano() != current.ModTimeNS {
			return fmt.Errorf("native writer preflight target changed during validation: %s", filePath)
		}
		current.HeadSHA256, current.TailSHA256, err = nativeFingerprints(filePath, current.Size)
		if err != nil {
			return err
		}
		next.Entries[key] = current
		report.ValidatedBytes += validatedBytes
		if incremental {
			report.IncrementalFiles++
		} else {
			report.ValidatedFiles++
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return report, err
	}
	if err := writeNativePreflightCache(cachePath, next, cacheBytes); err != nil {
		return report, err
	}
	return report, nil
}

func loadNativePreflightCache(path string) (nativePreflightCache, []byte, bool) {
	empty := nativePreflightCache{Version: nativePreflightCacheVersion, Entries: make(map[string]nativePreflightEntry)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil, false
	}
	if err != nil {
		return empty, nil, true
	}
	var cache nativePreflightCache
	if json.Unmarshal(data, &cache) != nil || cache.Version != nativePreflightCacheVersion || cache.Entries == nil {
		return empty, data, true
	}
	for key, entry := range cache.Entries {
		if key == "" || filepath.IsAbs(key) || strings.HasPrefix(filepath.Clean(key), "..") || entry.Size < 0 || entry.ModTimeNS < 0 ||
			!validNativeFingerprint(entry.HeadSHA256) || !validNativeFingerprint(entry.TailSHA256) {
			return empty, data, true
		}
	}
	return cache, data, false
}

func validNativeFingerprint(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func writeNativePreflightCache(path string, cache nativePreflightCache, previous []byte) error {
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	if bytes.Equal(data, previous) {
		return nil
	}
	root := filepath.Dir(path)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(root, ".native-preflight-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
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
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(root)
}

func nativePrefixMatches(path string, cached nativePreflightEntry) bool {
	head, tail, err := nativeFingerprints(path, cached.Size)
	return err == nil && head == cached.HeadSHA256 && tail == cached.TailSHA256
}

func nativeFingerprints(path string, logicalSize int64) (string, string, error) {
	if logicalSize < 0 {
		return "", "", errors.New("negative native fingerprint size")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	window := min(logicalSize, int64(nativeFingerprintWindow))
	head := make([]byte, int(window))
	if _, err := file.ReadAt(head, 0); err != nil && !errors.Is(err, io.EOF) {
		return "", "", err
	}
	tail := make([]byte, int(window))
	if _, err := file.ReadAt(tail, logicalSize-window); err != nil && !errors.Is(err, io.EOF) {
		return "", "", err
	}
	headDigest := sha256.Sum256(head)
	tailDigest := sha256.Sum256(tail)
	return hex.EncodeToString(headDigest[:]), hex.EncodeToString(tailDigest[:]), nil
}

func validateNativeJSONLFrom(ctx context.Context, filePath string, offset int64) (int64, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, err
	}
	if offset < 0 {
		_ = file.Close()
		return 0, errors.New("negative native preflight offset")
	}
	if offset > 0 {
		boundary := []byte{0}
		if _, err := file.ReadAt(boundary, offset-1); err != nil || boundary[0] != '\n' {
			_ = file.Close()
			return 0, fmt.Errorf("native writer preflight %s cached offset %d is not a JSONL boundary", filePath, offset)
		}
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		_ = file.Close()
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
				return bytesRead, fmt.Errorf("native writer preflight %s line %d after byte %d is missing its final newline", filePath, lineNumber, offset)
			}
			break
		}
		if readErr != nil {
			_ = file.Close()
			return bytesRead, fmt.Errorf("native writer preflight read %s after byte %d: %w", filePath, offset, readErr)
		}
		if !utf8.Valid(line) {
			_ = file.Close()
			return bytesRead, fmt.Errorf("native writer preflight %s line %d after byte %d is not valid UTF-8", filePath, lineNumber, offset)
		}
		if !json.Valid(line) {
			_ = file.Close()
			return bytesRead, fmt.Errorf("native writer preflight %s line %d after byte %d is not valid JSON", filePath, lineNumber, offset)
		}
	}
	if err := file.Close(); err != nil {
		return bytesRead, err
	}
	return bytesRead, nil
}

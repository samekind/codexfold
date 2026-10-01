package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/samekind/codexfold/internal/storage"
)

type stateInspectionEntry struct {
	state SessionState
	files map[string]os.FileInfo
	at    time.Time
}

// StateInspectionCache is scoped to a live read-only observer. Mutation and
// deletion fences must continue to use uncached inspection. Only unchanged
// primary/catalog/checkpoint identities are reused. A one-minute upper bound
// forces occasional full decoding even if every metadata stamp remains stable;
// the managed reload's ten-second poll still detects real file changes through
// the stamps without decoding every session on every poll.
type StateInspectionCache struct {
	mu      sync.Mutex
	entries map[string]stateInspectionEntry
}

const stateInspectionCacheLifetime = time.Minute

func (c *StateInspectionCache) Discover(root string) ([]SessionState, []SessionStateIssue, error) {
	return discoverSessionStatesDetailed(root, c.Inspect)
}

func inspectionStamp(paths []string) (map[string]os.FileInfo, error) {
	result := make(map[string]os.FileInfo, len(paths))
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			result[path] = nil
			continue
		}
		if err != nil {
			return nil, err
		}
		result[path] = info
	}
	return result, nil
}
func inspectionStampMatches(a, b map[string]os.FileInfo) bool {
	for path, info := range a {
		other, ok := b[path]
		if !ok || !storage.FileMetadataUnchanged(info, other) {
			return false
		}
	}
	return true
}

func (c *StateInspectionCache) Inspect(path string) (SessionState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	path = filepath.Clean(path)
	if c.entries == nil {
		c.entries = make(map[string]stateInspectionEntry)
	}
	if len(c.entries) > 4096 {
		clear(c.entries)
	}
	if cached, ok := c.entries[path]; ok && time.Since(cached.at) < stateInspectionCacheLifetime {
		paths := make([]string, 0, len(cached.files))
		for p := range cached.files {
			paths = append(paths, p)
		}
		current, err := inspectionStamp(paths)
		if err == nil && inspectionStampMatches(cached.files, current) {
			return cached.state, nil
		}
	}
	delete(c.entries, path)
	directory := filepath.Dir(path)
	paths := []string{directory, path, filepath.Join(directory, stateCatalogFilename)}
	readAt := time.Now()
	before, stampErr := inspectionStamp(paths)
	if catalog, err := loadStateCatalog(directory); err == nil {
		paths = append(paths, filepath.Join(directory, stateGenerationsDirectoryName), filepath.Join(directory, stateGenerationsDirectoryName, catalog.Checkpoint))
	}
	fullBefore, err := inspectionStamp(paths)
	cacheable := stampErr == nil && err == nil && inspectionStampMatches(before, fullBefore)
	state, err := InspectSessionState(path)
	if err != nil {
		return state, err
	}
	after, stampErr := inspectionStamp(paths)
	// A stamp still inside its filesystem clock tick cannot show the next
	// change, so it is not reused until that change would be distinguishable.
	if cacheable && stampErr == nil && inspectionStampMatches(fullBefore, after) && inspectionStampSettled(readAt, after) {
		c.entries[path] = stateInspectionEntry{state, after, time.Now()}
	}
	return state, nil
}

func inspectionStampSettled(readAt time.Time, stamp map[string]os.FileInfo) bool {
	infos := make([]os.FileInfo, 0, len(stamp))
	for _, info := range stamp {
		infos = append(infos, info)
	}
	return storage.MetadataSettled(readAt, infos...)
}

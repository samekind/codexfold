//go:build darwin

package mountfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type nativeNamespaceSnapshotEntry struct {
	directory  bool
	objectID   string
	mode       fs.FileMode
	uid        uint32
	gid        uint32
	size       int64
	modTime    int64
	changeTime int64
}

type nativeNamespaceRefreshEntry struct {
	route     string
	directory bool
}

type nativeNamespaceWatcher struct {
	path      string
	directory bool
}

type nativeNamespaceSnapshot map[string]nativeNamespaceSnapshotEntry

func (f *Filesystem) WatchNativeNamespace(ctx context.Context) error {
	f.mu.RLock()
	root := f.nativeRoot
	f.mu.RUnlock()
	if root == "" {
		return nil
	}
	queue, err := unix.Kqueue()
	if err != nil {
		return fmt.Errorf("create native namespace kqueue: %w", err)
	}
	defer unix.Close(queue)
	watchers := make(map[int]nativeNamespaceWatcher)
	watchersByPath := make(map[string]int)
	closeWatchers := func() {
		for descriptor := range watchers {
			_ = unix.Close(descriptor)
		}
		clear(watchers)
		clear(watchersByPath)
	}
	defer closeWatchers()
	rescan := func() (nativeNamespaceSnapshot, error) {
		seen := make(map[string]struct{})
		snapshot, err := scanNativeNamespace(root, func(name string, directory bool) error {
			name = filepath.Clean(name)
			seen[name] = struct{}{}
			if descriptor, exists := watchersByPath[name]; exists {
				watcher := watchers[descriptor]
				watcher.directory = directory
				watchers[descriptor] = watcher
				return nil
			}
			descriptor, err := unix.Open(name, unix.O_EVTONLY|unix.O_CLOEXEC, 0)
			if err != nil {
				if errors.Is(err, unix.ENOENT) {
					return nil
				}
				return err
			}
			event := unix.Kevent_t{
				Ident: uint64(descriptor), Filter: unix.EVFILT_VNODE,
				Flags: unix.EV_ADD | unix.EV_ENABLE | unix.EV_CLEAR,
				Fflags: unix.NOTE_WRITE | unix.NOTE_EXTEND | unix.NOTE_DELETE | unix.NOTE_RENAME |
					unix.NOTE_ATTRIB | unix.NOTE_LINK | unix.NOTE_REVOKE,
			}
			if _, err := unix.Kevent(queue, []unix.Kevent_t{event}, nil, nil); err != nil {
				_ = unix.Close(descriptor)
				return err
			}
			watchers[descriptor] = nativeNamespaceWatcher{path: name, directory: directory}
			watchersByPath[name] = descriptor
			return nil
		})
		if err != nil {
			return nil, err
		}
		for name, descriptor := range watchersByPath {
			if _, exists := seen[name]; exists {
				continue
			}
			_ = unix.Close(descriptor)
			delete(watchers, descriptor)
			delete(watchersByPath, name)
		}
		return snapshot, nil
	}
	snapshot, err := rescan()
	if err != nil {
		return err
	}
	pendingRefreshes := make(map[string]nativeNamespaceRefreshEntry)
	f.bumpNamespaceVersion()
	events := make([]unix.Kevent_t, 64)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		timeout := unix.NsecToTimespec((500 * time.Millisecond).Nanoseconds())
		count, err := unix.Kevent(queue, nil, events, &timeout)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("wait for native namespace event: %w", err)
		}
		if count > 0 {
			changedSet := make(map[string]struct{})
			changedFiles := make(map[string]nativeNamespaceWatcher)
			internalFiles := make(map[string]nativeNamespaceWatcher)
			externalChange := false
			fullRescan := false
			for _, event := range events[:count] {
				watcher, exists := watchers[int(event.Ident)]
				if !exists {
					continue
				}
				route, ok := nativeNamespaceRoute(root, watcher.path)
				if !ok {
					continue
				}
				if !watcher.directory && f.nativeInternalMutationSuppressed(watcher.path) {
					internalFiles[route] = watcher
					continue
				}
				externalChange = true
				if watcher.directory {
					changedSet[route] = struct{}{}
					fullRescan = true
				} else {
					changedFiles[route] = watcher
				}
				if !watcher.directory || event.Fflags&(unix.NOTE_DELETE|unix.NOTE_RENAME) != 0 {
					changedSet[filepath.ToSlash(filepath.Dir(route))] = struct{}{}
				}
				if event.Fflags&(unix.NOTE_DELETE|unix.NOTE_RENAME|unix.NOTE_REVOKE) != 0 {
					fullRescan = true
				}
			}
			current := cloneNativeNamespaceSnapshot(snapshot)
			if fullRescan {
				current, err = rescan()
				if err != nil {
					return err
				}
			} else {
				for route, watcher := range changedFiles {
					entry, exists, entryErr := nativeNamespaceEntry(watcher.path)
					if entryErr != nil {
						return fmt.Errorf("refresh native namespace entry %s: %w", route, entryErr)
					}
					if exists {
						current[route] = entry
					} else {
						delete(current, route)
					}
				}
				for route, watcher := range internalFiles {
					entry, exists, entryErr := nativeNamespaceEntry(watcher.path)
					if entryErr != nil {
						return fmt.Errorf("refresh internal native namespace entry %s: %w", route, entryErr)
					}
					if exists {
						current[route] = entry
					} else {
						delete(current, route)
					}
				}
			}
			comparison := snapshot
			if len(internalFiles) != 0 {
				comparison = cloneNativeNamespaceSnapshot(snapshot)
				for route := range internalFiles {
					if entry, exists := current[route]; exists {
						comparison[route] = entry
					} else {
						delete(comparison, route)
					}
				}
			}
			if externalChange {
				mergeNativeNamespaceRefreshEntries(pendingRefreshes, nativeNamespaceRefreshDelta(comparison, current))
			}
			snapshot = current
			if !externalChange {
				continue
			}
			changed := make([]string, 0, len(changedSet))
			for route := range changedSet {
				changed = append(changed, route)
			}
			f.bumpDirectoryGenerations(changed)
			f.bumpNamespaceVersion()
		}
		// A mount can be briefly unavailable while FSKit starts or remounts. Keep
		// failed name-cache repairs pending instead of terminating the daemon.
		retryNativeNamespaceRefreshes(pendingRefreshes, f.refreshNativeNamespacePath)
	}
}

func scanNativeNamespace(root string, watchEntry func(string, bool) error) (nativeNamespaceSnapshot, error) {
	snapshot := make(nativeNamespaceSnapshot)
	for _, namespace := range []string{"sessions", "archived_sessions"} {
		base := filepath.Join(root, namespace)
		err := filepath.WalkDir(base, func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if errors.Is(walkErr, os.ErrNotExist) {
					return nil
				}
				return walkErr
			}
			route, ok := nativeNamespaceRoute(root, name)
			if !ok {
				return nil
			}
			rootDirectory := route == "/sessions" || route == "/archived_sessions"
			if !rootDirectory && !nativeNamespaceRefreshCandidate(route) {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			info, err := entry.Info()
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				return nil
			}
			if watchEntry != nil {
				if err := watchEntry(name, info.IsDir()); err != nil {
					return err
				}
				info, err = os.Lstat(name)
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
			}
			if rootDirectory {
				return nil
			}
			snapshot[route] = nativeNamespaceEntryFromInfo(info)
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("scan native namespace %s: %w", namespace, err)
		}
	}
	return snapshot, nil
}

func nativeNamespaceEntry(name string) (nativeNamespaceSnapshotEntry, bool, error) {
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nativeNamespaceSnapshotEntry{}, false, nil
	}
	if err != nil {
		return nativeNamespaceSnapshotEntry{}, false, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nativeNamespaceSnapshotEntry{}, false, nil
	}
	return nativeNamespaceEntryFromInfo(info), true, nil
}

func nativeNamespaceEntryFromInfo(info os.FileInfo) nativeNamespaceSnapshotEntry {
	objectID := fileObjectIdentity(info)
	if objectID == "" {
		objectID = fmt.Sprintf("fallback:%d:%d", info.Size(), info.ModTime().UnixNano())
	}
	entry := nativeNamespaceSnapshotEntry{directory: info.IsDir(), objectID: objectID}
	if info.IsDir() {
		return entry
	}
	uid, gid, _, changeTime := fileOwnershipAndTimes(info)
	entry.mode = info.Mode()
	entry.uid = uid
	entry.gid = gid
	entry.size = info.Size()
	entry.modTime = info.ModTime().UnixNano()
	entry.changeTime = changeTime.UnixNano()
	return entry
}

func cloneNativeNamespaceSnapshot(snapshot nativeNamespaceSnapshot) nativeNamespaceSnapshot {
	cloned := make(nativeNamespaceSnapshot, len(snapshot))
	for route, entry := range snapshot {
		cloned[route] = entry
	}
	return cloned
}

func nativeNamespaceRefreshCandidate(route string) bool {
	if !canonicalNamespacePath(route) {
		return false
	}
	name := path.Base(route)
	return name != ".DS_Store" && !strings.HasPrefix(name, ".")
}

func nativeNamespaceRefreshDelta(previous nativeNamespaceSnapshot, current nativeNamespaceSnapshot) []nativeNamespaceRefreshEntry {
	entries := make([]nativeNamespaceRefreshEntry, 0)
	for route, entry := range current {
		old, exists := previous[route]
		if exists && old == entry {
			continue
		}
		entries = append(entries, nativeNamespaceRefreshEntry{route: route, directory: entry.directory})
	}
	sortNativeNamespaceRefreshEntries(entries)
	return entries
}

func mergeNativeNamespaceRefreshEntries(
	pending map[string]nativeNamespaceRefreshEntry,
	entries []nativeNamespaceRefreshEntry,
) {
	for _, entry := range entries {
		pending[entry.route] = entry
	}
}

func retryNativeNamespaceRefreshes(
	pending map[string]nativeNamespaceRefreshEntry,
	refresh func(string, bool) error,
) {
	entries := make([]nativeNamespaceRefreshEntry, 0, len(pending))
	for _, entry := range pending {
		entries = append(entries, entry)
	}
	sortNativeNamespaceRefreshEntries(entries)
	for _, entry := range entries {
		if err := refresh(entry.route, entry.directory); err == nil {
			delete(pending, entry.route)
		}
	}
}

func sortNativeNamespaceRefreshEntries(entries []nativeNamespaceRefreshEntry) {
	sort.Slice(entries, func(left int, right int) bool {
		leftDepth := strings.Count(entries[left].route, "/")
		rightDepth := strings.Count(entries[right].route, "/")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		if entries[left].directory != entries[right].directory {
			return entries[left].directory
		}
		return entries[left].route < entries[right].route
	})
}

func nativeNamespaceRoute(root string, nativePath string) (string, bool) {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(nativePath))
	if err != nil || relative == ".." || relative == "." || filepath.IsAbs(relative) ||
		len(relative) > 3 && relative[:3] == ".."+string(filepath.Separator) {
		return "", false
	}
	route := "/" + filepath.ToSlash(relative)
	if !canonicalNamespacePath(route) {
		return "", false
	}
	return route, true
}

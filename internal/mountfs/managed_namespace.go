package mountfs

// Managed file names are independent of Codex's thread index. In particular,
// renaming a rollout to a backup must not cause the index poll to resurrect it.
// This small durable namespace also gives replacement files their own identity:
// old descriptors and backups continue to reference the old managed session.
import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
)

type managedName struct {
	Path    string `json:"path"` // empty means unlinked/replaced, never rediscover
	Origin  string `json:"origin"`
	Pending bool   `json:"pending,omitempty"`
}

type nativeRenameCleanup struct {
	ObjectID string `json:"object_id"`
	Size     int64  `json:"size"`
	ModTime  int64  `json:"mod_time_ns"`
}

type managedNamespace struct {
	Version int                            `json:"version"`
	Names   map[string]managedName         `json:"names"`
	Cleanup map[string]nativeRenameCleanup `json:"cleanup,omitempty"`
	Native  map[string]bool                `json:"native,omitempty"`
	Move    *nativeRenameIntent            `json:"move,omitempty"`
}

type nativeRenameIntent struct {
	Source    string `json:"source"`
	Target    string `json:"target"`
	ObjectID  string `json:"object_id"`
	Displaced string `json:"displaced,omitempty"`
}

func namespaceFilePath(name string) bool {
	return cleanPath(name) == name && name != "" &&
		(strings.HasPrefix(name, "/sessions/") || strings.HasPrefix(name, "/archived_sessions/"))
}

func readManagedNamespace(store string) (managedNamespace, error) {
	n := managedNamespace{Version: 1, Names: make(map[string]managedName), Cleanup: make(map[string]nativeRenameCleanup), Native: make(map[string]bool)}
	data, err := os.ReadFile(filepath.Join(store, "fs", "namespace.json"))
	if errors.Is(err, os.ErrNotExist) {
		return n, nil
	}
	if err != nil {
		return n, err
	}
	if err := json.Unmarshal(data, &n); err != nil {
		return n, fmt.Errorf("decode managed names: %w", err)
	}
	if n.Version != 1 || n.Names == nil {
		return n, errors.New("invalid managed namespace")
	}
	paths := make(map[string]string)
	for id, name := range n.Names {
		if !safeSessionID(id) || !namespaceFilePath(name.Origin) || (name.Path != "" && !namespaceFilePath(name.Path)) {
			return n, errors.New("unsafe managed namespace entry")
		}
		if name.Path != "" {
			if previous := paths[name.Path]; previous != "" {
				return n, errors.New("duplicate managed namespace path")
			}
			paths[name.Path] = id
		}
	}
	for name, cleanup := range n.Cleanup {
		if !namespaceFilePath(name) || cleanup.ObjectID == "" || cleanup.Size < 0 {
			return n, errors.New("unsafe native rename cleanup")
		}
	}
	if n.Cleanup == nil {
		n.Cleanup = make(map[string]nativeRenameCleanup)
	}
	if n.Native == nil {
		n.Native = make(map[string]bool)
	}
	for name := range n.Native {
		if !namespaceFilePath(name) {
			return n, errors.New("unsafe native replacement path")
		}
	}
	if n.Move != nil && (!namespaceFilePath(n.Move.Source) || !namespaceFilePath(n.Move.Target) || n.Move.ObjectID == "") {
		return n, errors.New("unsafe native rename intent")
	}
	return n, nil
}

// ManagedNamespaceRoutes includes empty entries so callers can distinguish a
// deliberately hidden file from a session whose route has not been discovered.
func ManagedNamespaceRoutes(store string) (map[string]string, error) {
	n, err := readManagedNamespace(store)
	if err != nil {
		return nil, err
	}
	routes := make(map[string]string, len(n.Names))
	for id, name := range n.Names {
		routes[id] = name.Path
	}
	return routes, nil
}

// ManagedNamespacePaths includes native replacements awaiting background fold.
// Their logical Codex ID must not be enrolled again under a different inode.
func ManagedNamespacePaths(store string) (map[string]bool, error) {
	n, err := readManagedNamespace(store)
	if err != nil {
		return nil, err
	}
	paths := make(map[string]bool)
	for _, entry := range n.Names {
		if entry.Path != "" {
			paths[entry.Path] = true
		}
	}
	for name := range n.Native {
		paths[name] = true
	}
	return paths, nil
}

func (f *Filesystem) SetManagedStore(store string) error {
	if !filepath.IsAbs(store) {
		return errors.New("absolute managed store is required")
	}
	n, err := readManagedNamespace(store)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.namespaceStore, f.managedNamespace = filepath.Clean(store), n
	if n.Move != nil {
		source, sourceErr := os.Lstat(nativePathFromRoot(f.nativeRoot, n.Move.Source))
		target, targetErr := os.Lstat(nativePathFromRoot(f.nativeRoot, n.Move.Target))
		if targetErr == nil && fileObjectIdentity(target) == n.Move.ObjectID {
			applyNativeRename(&n)
		} else if sourceErr == nil && fileObjectIdentity(source) == n.Move.ObjectID {
			n.Move = nil
		} else {
			f.mu.Unlock()
			return errors.New("native rename recovery cannot identify source or target; both preserved")
		}
		if err := f.publishManagedNamespaceLocked(n); err != nil {
			f.mu.Unlock()
			return err
		}
	}
	for id, entry := range n.Names {
		// No rename from an earlier daemon process can still be preparing.
		entry.Pending = false
		n.Names[id] = entry
	}
	f.managedNamespace = n
	// A committed rename may have crashed before unlinking its old native
	// temporary. Only the exact unchanged source inode may be removed on replay.
	for name, cleanup := range n.Cleanup {
		if err := f.cleanupRenamedNativeLocked(name, cleanup); err != nil {
			f.mu.Unlock()
			return err
		}
	}
	if len(n.Cleanup) != 0 {
		n.Cleanup = make(map[string]nativeRenameCleanup)
		err = f.publishManagedNamespaceLocked(n)
	}
	f.mu.Unlock()
	return err
}

func (f *Filesystem) namespaceRouteLocked(id, fallback string) (string, bool) {
	if name, ok := f.managedNamespace.Names[id]; ok {
		return name.Path, name.Path != ""
	}
	return fallback, true
}

func (f *Filesystem) namespaceReplacementTargetLocked(name string) bool {
	for _, entry := range f.managedNamespace.Names {
		if entry.Origin == name {
			return true
		}
	}
	return false
}

func (f *Filesystem) cloneManagedNamespaceLocked() managedNamespace {
	n := managedNamespace{Version: 1, Names: make(map[string]managedName), Cleanup: make(map[string]nativeRenameCleanup), Native: make(map[string]bool), Move: f.managedNamespace.Move}
	for id, name := range f.managedNamespace.Names {
		n.Names[id] = name
	}
	for name, cleanup := range f.managedNamespace.Cleanup {
		n.Cleanup[name] = cleanup
	}
	for name, value := range f.managedNamespace.Native {
		n.Native[name] = value
	}
	return n
}

func (f *Filesystem) publishManagedNamespaceLocked(n managedNamespace) error {
	if f.namespaceStore == "" {
		return errors.New("managed file rename requires a configured store")
	}
	directory := filepath.Join(f.namespaceStore, "fs")
	data, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".namespace-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), filepath.Join(directory, "namespace.json")); err != nil {
		return err
	}
	// The rename is already visible, so memory must agree even if directory
	// synchronization fails and the caller reports the durability error.
	f.managedNamespace = n
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// renameManagedLocked consumes f.mu. No source bytes are copied or rewritten.
func (f *Filesystem) renameManagedLocked(id, oldPath, newPath string) syscall.Errno {
	defer f.mu.Unlock()
	if _, directory := f.directories[newPath]; directory {
		return syscall.EISDIR
	}
	if info, err := os.Lstat(nativePathFromRoot(f.nativeRoot, newPath)); f.nativeRoot != "" && err == nil && info.IsDir() {
		return syscall.EISDIR
	}
	var cleanup *nativeRenameCleanup
	if f.nativeRoot != "" {
		target := nativePathFromRoot(f.nativeRoot, newPath)
		if info, err := os.Lstat(target); err == nil && info.Mode().IsRegular() {
			// Append transactions reopen by path. Never redirect an existing
			// native writer into the restored managed file or lose its tail.
			if f.nativePathBusyLocked(newPath) || f.nativeSnapshotPathLocked(target) {
				return syscall.EBUSY
			}
			cleanup = &nativeRenameCleanup{ObjectID: fileObjectIdentity(info), Size: info.Size(), ModTime: info.ModTime().UnixNano()}
		}
	}
	if f.namespaceStore == "" {
		// Legacy in-memory fixtures and nonpersistent adapters still support
		// archive/unarchive, but never pretend arbitrary backups are durable.
		if !canonicalSessionPath(newPath) {
			return syscall.ENOTSUP
		}
		if _, exists := f.paths[newPath]; exists {
			return syscall.EEXIST
		}
	}
	if err := moveManagedXattrCarrier(f.nativeRoot, oldPath, newPath); err != nil {
		return errnoFor(err)
	}
	if f.namespaceStore != "" {
		n := f.cloneManagedNamespaceLocked()
		origin := oldPath
		if entry, ok := n.Names[id]; ok {
			origin = entry.Origin
		}
		n.Names[id] = managedName{Path: newPath, Origin: origin}
		delete(n.Native, newPath)
		if cleanup != nil {
			n.Cleanup[newPath] = *cleanup
		}
		if displaced := f.paths[newPath]; displaced != "" && displaced != id {
			entry := n.Names[displaced]
			if entry.Origin == "" {
				entry.Origin = newPath
			}
			entry.Path = ""
			n.Names[displaced] = entry
		}
		if err := f.publishManagedNamespaceLocked(n); err != nil {
			_ = moveManagedXattrCarrier(f.nativeRoot, newPath, oldPath)
			return errnoFor(err)
		}
		if cleanup != nil {
			delete(f.nativeAppends, filepath.Clean(nativePathFromRoot(f.nativeRoot, newPath)))
			if err := f.cleanupRenamedNativeLocked(newPath, *cleanup); err == nil {
				n = f.cloneManagedNamespaceLocked()
				delete(n.Cleanup, newPath)
				_ = f.publishManagedNamespaceLocked(n)
			}
		}
	}
	delete(f.paths, oldPath)
	f.paths[newPath] = id
	now := time.Now()
	f.bumpDirectoryGenerationLocked(path.Dir(oldPath), now)
	f.bumpDirectoryGenerationLocked(path.Dir(newPath), now)
	for _, h := range f.handles {
		if h.path == oldPath {
			h.path = newPath
		}
	}
	f.bumpNamespaceVersion()
	return 0
}

func (f *Filesystem) nativeSnapshotPathLocked(target string) bool {
	for _, session := range f.sessions {
		if snapshot := session.State().NativeSnapshot.Path; snapshot != "" && filepath.Clean(snapshot) == filepath.Clean(target) {
			return true
		}
	}
	return false
}

func (f *Filesystem) cleanupRenamedNativeLocked(name string, cleanup nativeRenameCleanup) error {
	if f.nativeRoot == "" {
		return errors.New("native root is required for rename replay")
	}
	root, err := os.OpenRoot(f.nativeRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	relative := filepath.FromSlash(strings.TrimPrefix(name, "/"))
	info, err := root.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || fileObjectIdentity(info) != cleanup.ObjectID || info.Size() != cleanup.Size || info.ModTime().UnixNano() != cleanup.ModTime {
		return fmt.Errorf("renamed native source changed; preserving %s", name)
	}
	if err := root.Remove(relative); err != nil {
		return err
	}
	dir, err := root.Open(filepath.Dir(relative))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Native replacement is a namespace operation, not a compression job. Record
// intent first, use the native rename, then publish native authority. A restart
// completes the intent by matching the source inode, never by guessing bytes.
func (f *Filesystem) importNativeRename(oldPath, newPath string) syscall.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.namespaceStore == "" {
		return syscall.ENOTSUP
	}
	source := nativePathFromRoot(f.nativeRoot, oldPath)
	target := nativePathFromRoot(f.nativeRoot, newPath)
	info, err := os.Lstat(source)
	if err != nil {
		return errnoFor(err)
	}
	if !info.Mode().IsRegular() {
		return syscall.EINVAL
	}
	if _, isDir := f.directories[newPath]; isDir {
		return syscall.EISDIR
	}
	if dest, err := os.Lstat(target); err == nil && dest.IsDir() {
		return syscall.EISDIR
	}
	if f.nativePathBusyLocked(newPath) || f.nativeSnapshotPathLocked(target) {
		return syscall.EBUSY
	}
	displaced := f.paths[newPath]
	n := f.cloneManagedNamespaceLocked()
	n.Move = &nativeRenameIntent{Source: oldPath, Target: newPath, ObjectID: fileObjectIdentity(info), Displaced: displaced}
	if err := f.publishManagedNamespaceLocked(n); err != nil {
		return errnoFor(err)
	}
	state := f.nativeAppends[filepath.Clean(source)]
	if state != nil {
		err = state.Relocate(target)
	} else {
		err = os.Rename(source, target)
	}
	if err != nil {
		n.Move = nil
		_ = f.publishManagedNamespaceLocked(n)
		return errnoFor(err)
	}
	applyNativeRename(&n)
	// The durable intent is sufficient for recovery even if the final metadata
	// sync fails after the OS rename. Do not report failure for a completed move.
	if err := f.publishManagedNamespaceLocked(n); err != nil {
		f.managedNamespace = n
	}
	delete(f.paths, newPath)
	delete(f.nativeAppends, filepath.Clean(source))
	delete(f.nativeAppends, filepath.Clean(target))
	if state != nil {
		f.nativeAppends[filepath.Clean(target)] = state
	}
	for _, h := range f.handles {
		if h.nativePath == source {
			h.nativePath = target
			h.path = newPath
		}
	}
	f.bumpDirectoryGenerationLocked(path.Dir(oldPath), time.Now())
	f.bumpDirectoryGenerationLocked(path.Dir(newPath), time.Now())
	f.bumpNamespaceVersion()
	return 0
}

func applyNativeRename(n *managedNamespace) {
	m := n.Move
	if m.Displaced != "" {
		entry := n.Names[m.Displaced]
		if entry.Origin == "" {
			entry.Origin = m.Target
		}
		entry.Path = ""
		n.Names[m.Displaced] = entry
	}
	delete(n.Native, m.Source)
	n.Native[m.Target] = true
	n.Move = nil
}

// FoldNativeReplacements is background-only. Native open files are left alone;
// a changed source simply remains native until a later stable attempt.
func (f *Filesystem) FoldNativeReplacements(ctx context.Context, prepare func(context.Context, string, string) (*vfs.Session, io.Closer, error)) error {
	f.mu.RLock()
	var names []string
	for name := range f.managedNamespace.Native {
		names = append(names, name)
	}
	root := f.nativeRoot
	f.mu.RUnlock()
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		f.mu.RLock()
		busy := f.nativePathBusyLocked(name)
		f.mu.RUnlock()
		if busy {
			continue
		}
		source := nativePathFromRoot(root, name)
		before, err := os.Lstat(source)
		if errors.Is(err, os.ErrNotExist) {
			f.renameMu.Lock()
			f.mu.Lock()
			if _, statErr := os.Lstat(source); errors.Is(statErr, os.ErrNotExist) && f.managedNamespace.Native[name] {
				n := f.cloneManagedNamespaceLocked()
				delete(n.Native, name)
				err = f.publishManagedNamespaceLocked(n)
			} else {
				err = nil
			}
			f.mu.Unlock()
			f.renameMu.Unlock()
			if err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		id := "file-" + hex.EncodeToString(random[:])
		f.mu.Lock()
		n := f.cloneManagedNamespaceLocked()
		n.Names[id] = managedName{Origin: name, Pending: true}
		err = f.publishManagedNamespaceLocked(n)
		f.mu.Unlock()
		if err != nil {
			return err
		}
		session, closer, prepareErr := prepare(ctx, source, id)
		owner := newSessionOwner(closer)
		f.renameMu.Lock()
		f.mu.Lock()
		after, statErr := os.Lstat(source)
		unchanged := statErr == nil && os.SameFile(before, after) && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
		n = f.cloneManagedNamespaceLocked()
		if prepareErr != nil || session == nil || !unchanged || !n.Native[name] || f.nativePathBusyLocked(name) {
			n.Names[id] = managedName{Origin: name}
			err = f.publishManagedNamespaceLocked(n)
			f.mu.Unlock()
			f.renameMu.Unlock()
			owner.retire()
			if err != nil {
				return err
			}
			if prepareErr != nil && unchanged {
				return prepareErr
			}
			continue
		}
		n.Names[id] = managedName{Origin: name, Path: name}
		delete(n.Native, name)
		cleanup := nativeRenameCleanup{ObjectID: fileObjectIdentity(after), Size: after.Size(), ModTime: after.ModTime().UnixNano()}
		n.Cleanup[name] = cleanup
		if err := f.publishManagedNamespaceLocked(n); err != nil {
			f.mu.Unlock()
			f.renameMu.Unlock()
			owner.retire()
			return err
		}
		f.sessions[id], f.owners[id], f.paths[name] = session, owner, id
		delete(f.nativeAppends, filepath.Clean(source))
		f.ensureDirectoryChainLocked(path.Dir(name))
		f.bumpNamespaceVersion()
		if err := f.cleanupRenamedNativeLocked(name, cleanup); err == nil {
			n = f.cloneManagedNamespaceLocked()
			delete(n.Cleanup, name)
			_ = f.publishManagedNamespaceLocked(n)
		}
		f.mu.Unlock()
		f.renameMu.Unlock()
	}
	return nil
}

type namespaceObjectReader struct {
	loose      *fold.ObjectStore
	packed     vfs.ObjectReader
	mu         sync.Mutex
	cache      map[string][]byte
	cacheBytes int
}

// NamespaceReader serves freshly replaced files before the next pack build.
// Callers doing pack-only retirement proofs must keep using the pack resolver.
func NamespaceReader(store string, packed vfs.ObjectReader) vfs.ObjectReader {
	return &namespaceObjectReader{loose: fold.NewObjectStore(store), packed: packed}
}

// RetireReplacedFiles publishes ordinary deletion authority for overwritten
// inodes, never for named backups. Existing leases continue to delay physical
// removal through the existing deletion recovery/purge protocol.
func (f *Filesystem) RetireReplacedFiles(ctx context.Context) error {
	f.renameMu.Lock()
	defer f.renameMu.Unlock()
	f.mu.RLock()
	store := f.namespaceStore
	names := make(map[string]managedName)
	for id, entry := range f.managedNamespace.Names {
		if _, deleted := f.deletedManagedSessions[id]; !deleted && entry.Path == "" && !entry.Pending {
			names[id] = entry
		}
	}
	f.mu.RUnlock()
	for id, entry := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, err := vfs.InspectSessionState(filepath.Join(store, "fs", "sessions", id, "state.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		guard, acquired, err := vfs.TryAcquireWriterLeaseGuard(store, id)
		if err != nil {
			return err
		}
		if !acquired {
			continue
		}
		_, err = vfs.PublishSessionDeletion(store, state, entry.Origin)
		guard.Close()
		if errors.Is(err, storage.ErrOperationLockHeld) || errors.Is(err, vfs.ErrSessionDeletionBusy) {
			continue
		}
		if err != nil {
			return err
		}
		f.mu.Lock()
		f.deletedManagedSessions[id] = struct{}{}
		owner := f.owners[id]
		delete(f.sessions, id)
		delete(f.owners, id)
		f.mu.Unlock()
		if err := owner.retire(); err != nil {
			return err
		}
	}
	return nil
}

func (r *namespaceObjectReader) ReadAt(ctx context.Context, ref fold.ObjectRef, dest []byte, offset int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if offset < 0 {
		return 0, errors.New("negative object offset")
	}
	if has, ok := r.packed.(interface{ HasObject(fold.ObjectRef) bool }); ok && has.HasObject(ref) {
		return r.packed.ReadAt(ctx, ref, dest, offset)
	}
	r.mu.Lock()
	data := r.cache[ref.SHA256]
	r.mu.Unlock()
	var err error
	if data == nil {
		data, err = r.loose.Read(ref)
		if err == nil && len(data) <= 1<<20 {
			r.mu.Lock()
			if r.cache == nil || r.cacheBytes+len(data) > 1<<20 {
				r.cache = make(map[string][]byte)
				r.cacheBytes = 0
			}
			if r.cache[ref.SHA256] == nil {
				r.cache[ref.SHA256] = data
				r.cacheBytes += len(data)
			}
			r.mu.Unlock()
		}
	}
	if err != nil {
		if r.packed != nil {
			return r.packed.ReadAt(ctx, ref, dest, offset)
		}
		return 0, err
	}
	if offset >= int64(len(data)) {
		return 0, io.EOF
	}
	n := copy(dest, data[offset:])
	if n < len(dest) {
		return n, io.EOF
	}
	return n, nil
}

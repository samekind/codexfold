package mountfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jstar0/codexfold/internal/vfs"
)

type Attr struct {
	Mode    uint32    `json:"mode"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

type fileHandle struct {
	mu      sync.Mutex
	session *vfs.Session
	native  *os.File
	read    *vfs.ReadHandle
	write   *vfs.WriteHandle
	append  bool
}

type Filesystem struct {
	mu          sync.RWMutex
	loadMu      sync.Mutex
	sessions    map[string]*vfs.Session
	paths       map[string]string
	retained    map[string]string
	nativeFirst map[string]struct{}
	directories map[string]struct{}
	handles     map[uint64]*fileHandle
	next        uint64
	loader      func(string) (*vfs.Session, error)
	canonical   bool
	nativeRoot  string
}

func New() *Filesystem {
	return &Filesystem{sessions: make(map[string]*vfs.Session), handles: make(map[uint64]*fileHandle), next: 1}
}

func NewCanonical() *Filesystem {
	return &Filesystem{
		sessions: make(map[string]*vfs.Session), paths: make(map[string]string),
		retained: make(map[string]string), nativeFirst: make(map[string]struct{}),
		directories: map[string]struct{}{`/`: {}, `/sessions`: {}, `/archived_sessions`: {}},
		handles:     make(map[uint64]*fileHandle), next: 1, canonical: true,
	}
}

func (f *Filesystem) SetNativeRoot(root string) {
	if root != "" {
		root = filepath.Clean(root)
	}
	f.mu.Lock()
	f.nativeRoot = root
	for retained := range f.retained {
		delete(f.retained, retained)
	}
	for sessionID, session := range f.sessions {
		f.registerRetainedPathLocked(sessionID, session)
	}
	f.mu.Unlock()
}

func (f *Filesystem) AddSession(sessionID string, session *vfs.Session) error {
	if sessionID == "" || strings.ContainsAny(sessionID, "/\\\x00") || session == nil {
		return errors.New("safe session ID and session are required")
	}
	f.mu.Lock()
	if _, exists := f.sessions[sessionID]; exists {
		f.mu.Unlock()
		return errors.New("session is already mounted")
	}
	f.sessions[sessionID] = session
	f.mu.Unlock()
	return nil
}

func (f *Filesystem) UpsertSession(sessionID string, session *vfs.Session) error {
	if sessionID == "" || strings.ContainsAny(sessionID, "/\\\x00") || session == nil {
		return errors.New("safe session ID and session are required")
	}
	f.mu.Lock()
	f.sessions[sessionID] = session
	f.mu.Unlock()
	return nil
}

func (f *Filesystem) AddSessionAt(sessionID string, name string, session *vfs.Session) error {
	cleaned := cleanPath(name)
	if !f.canonical || !safeSessionID(sessionID) || session == nil || !canonicalSessionPath(cleaned) {
		return errors.New("canonical filesystem, safe session ID, path, and session are required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.sessions[sessionID]; exists {
		return errors.New("session is already mounted")
	}
	if _, exists := f.paths[cleaned]; exists {
		return errors.New("session path is already mounted")
	}
	f.ensureDirectoryChainLocked(path.Dir(cleaned))
	f.sessions[sessionID] = session
	f.paths[cleaned] = sessionID
	delete(f.nativeFirst, sessionID)
	f.registerRetainedPathLocked(sessionID, session)
	return nil
}

func (f *Filesystem) UpsertSessionAt(sessionID string, name string, session *vfs.Session) error {
	cleaned := cleanPath(name)
	if !f.canonical || !safeSessionID(sessionID) || session == nil || !canonicalSessionPath(cleaned) {
		return errors.New("canonical filesystem, safe session ID, path, and session are required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureDirectoryChainLocked(path.Dir(cleaned))
	var previousPath string
	for route, currentID := range f.paths {
		if currentID == sessionID {
			previousPath = route
			delete(f.paths, route)
		}
	}
	if err := moveManagedMetadata(f.nativeRoot, previousPath, cleaned); err != nil {
		if previousPath != "" {
			f.paths[previousPath] = sessionID
		}
		return err
	}
	f.sessions[sessionID] = session
	f.paths[cleaned] = sessionID
	delete(f.nativeFirst, sessionID)
	f.registerRetainedPathLocked(sessionID, session)
	return nil
}

func (f *Filesystem) MoveSessionAt(sessionID string, name string) error {
	cleaned := cleanPath(name)
	if !f.canonical || !safeSessionID(sessionID) || !canonicalSessionPath(cleaned) {
		return errors.New("canonical filesystem, safe session ID, and path are required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.sessions[sessionID]; !exists {
		return os.ErrNotExist
	}
	f.ensureDirectoryChainLocked(path.Dir(cleaned))
	var previousPath string
	for route, currentID := range f.paths {
		if currentID == sessionID {
			previousPath = route
			delete(f.paths, route)
		}
	}
	if err := moveManagedMetadata(f.nativeRoot, previousPath, cleaned); err != nil {
		if previousPath != "" {
			f.paths[previousPath] = sessionID
		}
		return err
	}
	f.paths[cleaned] = sessionID
	return nil
}

func (f *Filesystem) PreferNativeSession(sessionID string) error {
	if !f.canonical || !safeSessionID(sessionID) {
		return errors.New("canonical filesystem and safe session ID are required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.sessions[sessionID]; !exists {
		return os.ErrNotExist
	}
	f.nativeFirst[sessionID] = struct{}{}
	return nil
}

func (f *Filesystem) RemoveSession(sessionID string) error {
	if !safeSessionID(sessionID) {
		return errors.New("safe session ID is required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.sessions[sessionID]; !exists {
		return os.ErrNotExist
	}
	delete(f.sessions, sessionID)
	delete(f.nativeFirst, sessionID)
	for route, currentID := range f.paths {
		if currentID == sessionID {
			delete(f.paths, route)
		}
	}
	for retained, currentID := range f.retained {
		if currentID == sessionID {
			delete(f.retained, retained)
		}
	}
	return nil
}

func (f *Filesystem) SetSessionLoader(loader func(string) (*vfs.Session, error)) {
	f.mu.Lock()
	f.loader = loader
	f.mu.Unlock()
}

func (f *Filesystem) ReadDir(name string) ([]string, syscall.Errno) {
	cleaned := cleanPath(name)
	if !f.canonical && cleaned != "/" {
		return nil, syscall.ENOTDIR
	}
	f.mu.RLock()
	if f.canonical {
		if !canonicalNamespacePath(cleaned) {
			f.mu.RUnlock()
			return nil, syscall.ENOTDIR
		}
		_, virtualDirectory := f.directories[cleaned]
		nativeRoot := f.nativeRoot
		if !virtualDirectory && nativeRoot == "" {
			f.mu.RUnlock()
			return nil, syscall.ENOTDIR
		}
		if !virtualDirectory && nativeRoot != "" {
			info, err := os.Stat(nativePathFromRoot(nativeRoot, cleaned))
			if err != nil || !info.IsDir() {
				f.mu.RUnlock()
				return nil, syscall.ENOTDIR
			}
		}
		entrySet := make(map[string]struct{})
		hiddenEntries := make(map[string]struct{})
		for retained := range f.retained {
			if path.Dir(retained) == cleaned {
				hiddenEntries[path.Base(retained)] = struct{}{}
			}
		}
		for directory := range f.directories {
			if directory != cleaned && path.Dir(directory) == cleaned {
				entrySet[path.Base(directory)] = struct{}{}
			}
		}
		for route := range f.paths {
			if path.Dir(route) == cleaned {
				entrySet[path.Base(route)] = struct{}{}
			}
		}
		f.mu.RUnlock()
		if nativeRoot != "" && cleaned != "/" {
			if nativeEntries, err := os.ReadDir(nativePathFromRoot(nativeRoot, cleaned)); err == nil {
				for _, entry := range nativeEntries {
					if _, hidden := hiddenEntries[entry.Name()]; hidden {
						continue
					}
					entrySet[entry.Name()] = struct{}{}
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, errnoFor(err)
			}
		}
		entries := make([]string, 0, len(entrySet))
		for entry := range entrySet {
			entries = append(entries, entry)
		}
		sort.Strings(entries)
		return entries, 0
	}
	entries := make([]string, 0, len(f.sessions))
	for sessionID := range f.sessions {
		entries = append(entries, sessionID+".jsonl")
	}
	f.mu.RUnlock()
	sort.Strings(entries)
	return entries, 0
}

func (f *Filesystem) Getattr(name string) (Attr, syscall.Errno) {
	cleaned := cleanPath(name)
	if cleaned == "/" {
		return Attr{Mode: syscall.S_IFDIR | 0o700}, 0
	}
	if f.canonical {
		f.mu.RLock()
		_, directory := f.directories[cleaned]
		f.mu.RUnlock()
		if directory {
			return Attr{Mode: syscall.S_IFDIR | 0o700}, 0
		}
		if session, errno := f.sessionForPath(cleaned); errno == 0 {
			return sessionAttr(session)
		}
		if nativePath, ok := f.nativePath(cleaned); ok {
			info, err := os.Stat(nativePath)
			if err == nil {
				if info.IsDir() {
					return Attr{Mode: syscall.S_IFDIR | 0o700, ModTime: info.ModTime()}, 0
				}
				return Attr{Mode: syscall.S_IFREG | 0o600, Size: info.Size(), ModTime: info.ModTime()}, 0
			}
			if !errors.Is(err, os.ErrNotExist) {
				return Attr{}, errnoFor(err)
			}
		}
	}
	session, errno := f.sessionForPath(cleaned)
	if errno != 0 {
		return Attr{}, errno
	}
	return sessionAttr(session)
}

func sessionAttr(session *vfs.Session) (Attr, syscall.Errno) {
	info, err := session.VisibleInfo()
	if err != nil {
		return Attr{}, errnoFor(err)
	}
	return Attr{Mode: syscall.S_IFREG | 0o600, Size: info.Size, ModTime: info.ModTime}, 0
}

func (f *Filesystem) Open(name string, flags int) (uint64, syscall.Errno) {
	session, errno := f.sessionForPath(name)
	if errno != 0 {
		if !f.canonical {
			return 0, errno
		}
		nativePath, ok := f.nativePath(cleanPath(name))
		if !ok {
			return 0, errno
		}
		native, err := os.OpenFile(nativePath, flags, 0o600)
		if err != nil {
			return 0, errnoFor(err)
		}
		f.mu.Lock()
		handleID := f.next
		f.next++
		f.handles[handleID] = &fileHandle{native: native, append: flags&os.O_APPEND != 0}
		f.mu.Unlock()
		return handleID, 0
	}
	handle := &fileHandle{session: session, append: flags&os.O_APPEND != 0}
	access := flags & (os.O_WRONLY | os.O_RDWR)
	if access != os.O_WRONLY {
		reader, err := session.OpenReader()
		if err != nil {
			return 0, errnoFor(err)
		}
		handle.read = reader
	}
	if access == os.O_WRONLY || access == os.O_RDWR {
		writer, err := session.OpenWriter()
		if err != nil {
			if handle.read != nil {
				_ = handle.read.Close()
			}
			return 0, errnoFor(err)
		}
		handle.write = writer
		if flags&os.O_TRUNC != 0 {
			if err := writer.Truncate(context.Background(), 0); err != nil {
				_ = writer.Close()
				if handle.read != nil {
					_ = handle.read.Close()
				}
				return 0, errnoFor(err)
			}
		}
	}
	f.mu.Lock()
	handleID := f.next
	f.next++
	f.handles[handleID] = handle
	f.mu.Unlock()
	return handleID, 0
}

func (f *Filesystem) Read(handleID uint64, destination []byte, offset int64) (int, syscall.Errno) {
	handle, errno := f.handle(handleID)
	if errno != 0 {
		return 0, syscall.EBADF
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.native != nil {
		n, err := handle.native.ReadAt(destination, offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return n, errnoFor(err)
		}
		return n, 0
	}
	if handle.read == nil {
		return 0, syscall.EBADF
	}
	n, err := handle.read.ReadAt(context.Background(), destination, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, errnoFor(err)
	}
	return n, 0
}

func (f *Filesystem) Write(handleID uint64, data []byte, offset int64) (int, syscall.Errno) {
	handle, errno := f.handle(handleID)
	if errno != 0 {
		return 0, syscall.EBADF
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.native != nil {
		var n int
		var err error
		if handle.append {
			n, err = handle.native.Write(data)
		} else {
			n, err = handle.native.WriteAt(data, offset)
		}
		return n, errnoFor(err)
	}
	if handle.write == nil {
		return 0, syscall.EBADF
	}
	var n int
	var err error
	if handle.append {
		n, err = handle.write.Append(context.Background(), data)
	} else {
		info, infoErr := handle.session.VisibleInfo()
		if infoErr != nil {
			return 0, errnoFor(infoErr)
		}
		if offset == info.Size {
			n, err = handle.write.Append(context.Background(), data)
		} else {
			n, err = handle.write.WriteAt(context.Background(), data, offset)
		}
	}
	if err != nil {
		return n, errnoFor(err)
	}
	if handle.read != nil {
		if errno := refreshReader(handle); errno != 0 {
			return n, errno
		}
	}
	return n, 0
}

func (f *Filesystem) Truncate(handleID uint64, size int64) syscall.Errno {
	handle, errno := f.handle(handleID)
	if errno != 0 {
		return syscall.EBADF
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.native != nil {
		return errnoFor(handle.native.Truncate(size))
	}
	if handle.write == nil {
		return syscall.EBADF
	}
	if err := handle.write.Truncate(context.Background(), size); err != nil {
		return errnoFor(err)
	}
	if handle.read != nil {
		return refreshReader(handle)
	}
	return 0
}

func (f *Filesystem) TruncatePath(name string, size int64) syscall.Errno {
	session, errno := f.sessionForPath(name)
	if errno != 0 {
		if f.canonical {
			if nativePath, ok := f.nativePath(cleanPath(name)); ok {
				return errnoFor(os.Truncate(nativePath, size))
			}
		}
		return errno
	}
	if handle := f.lockActiveWriter(session); handle != nil {
		defer handle.mu.Unlock()
		if err := handle.write.Truncate(context.Background(), size); err != nil {
			return errnoFor(err)
		}
		if handle.read != nil {
			return refreshReader(handle)
		}
		return 0
	}
	writer, err := session.OpenWriter()
	if err != nil {
		return errnoFor(err)
	}
	truncateErr := writer.Truncate(context.Background(), size)
	closeErr := writer.Close()
	if truncateErr != nil {
		return errnoFor(truncateErr)
	}
	return errnoFor(closeErr)
}

func (f *Filesystem) lockActiveWriter(session *vfs.Session) *fileHandle {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, handle := range f.handles {
		if handle.session == session && handle.write != nil {
			handle.mu.Lock()
			return handle
		}
	}
	return nil
}

func (f *Filesystem) Fsync(handleID uint64) syscall.Errno {
	handle, errno := f.handle(handleID)
	if errno != 0 {
		return errno
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.native != nil {
		return errnoFor(handle.native.Sync())
	}
	if handle.write == nil {
		return 0
	}
	return errnoFor(handle.write.Sync())
}

func (f *Filesystem) Flush(handleID uint64) syscall.Errno {
	_, errno := f.handle(handleID)
	return errno
}

func (f *Filesystem) Release(handleID uint64) syscall.Errno {
	f.mu.Lock()
	handle, ok := f.handles[handleID]
	if ok {
		delete(f.handles, handleID)
	}
	f.mu.Unlock()
	if !ok {
		return syscall.EBADF
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.native != nil {
		return errnoFor(handle.native.Close())
	}
	var result syscall.Errno
	if handle.read != nil {
		if err := handle.read.Close(); err != nil {
			result = errnoFor(err)
		}
	}
	if handle.write != nil {
		if err := handle.write.Close(); err != nil && result == 0 {
			result = errnoFor(err)
		}
	}
	return result
}

func refreshReader(handle *fileHandle) syscall.Errno {
	reader, err := handle.session.OpenReader()
	if err != nil {
		return errnoFor(err)
	}
	old := handle.read
	handle.read = reader
	if old != nil {
		if err := old.Close(); err != nil {
			return errnoFor(err)
		}
	}
	return 0
}

func (f *Filesystem) Mkdir(name string, _ uint32) syscall.Errno {
	cleaned := cleanPath(name)
	if !f.canonical || cleaned == "" || cleaned == "/" || !canonicalNamespacePath(cleaned) {
		return syscall.EPERM
	}
	f.mu.Lock()
	if _, exists := f.directories[cleaned]; exists {
		f.mu.Unlock()
		return syscall.EEXIST
	}
	if _, exists := f.paths[cleaned]; exists {
		f.mu.Unlock()
		return syscall.EEXIST
	}
	root := f.nativeRoot
	_, virtualParent := f.directories[path.Dir(cleaned)]
	f.mu.Unlock()
	if !virtualParent && root == "" {
		return syscall.ENOENT
	}
	if root != "" {
		if err := os.Mkdir(nativePathFromRoot(root, cleaned), 0o700); err != nil {
			return errnoFor(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.directories[cleaned] = struct{}{}
	return 0
}

func (f *Filesystem) Rename(oldName string, newName string) syscall.Errno {
	if !f.canonical {
		return syscall.EPERM
	}
	oldPath, newPath := cleanPath(oldName), cleanPath(newName)
	if !canonicalSessionPath(oldPath) || !canonicalSessionPath(newPath) {
		return syscall.EPERM
	}
	f.mu.Lock()
	sessionID, exists := f.paths[oldPath]
	if !exists {
		if _, hidden := f.retained[oldPath]; hidden {
			f.mu.Unlock()
			return syscall.ENOENT
		}
		root := f.nativeRoot
		f.mu.Unlock()
		if root == "" {
			return syscall.ENOENT
		}
		oldNative := nativePathFromRoot(root, oldPath)
		newNative := nativePathFromRoot(root, newPath)
		if err := os.Rename(oldNative, newNative); err != nil {
			return errnoFor(err)
		}
		return 0
	}
	defer f.mu.Unlock()
	if _, exists := f.directories[path.Dir(newPath)]; !exists {
		root := f.nativeRoot
		info, err := os.Stat(nativePathFromRoot(root, path.Dir(newPath)))
		if root == "" || err != nil || !info.IsDir() {
			return syscall.ENOENT
		}
	}
	if _, exists := f.paths[newPath]; exists {
		return syscall.EEXIST
	}
	if err := moveManagedXattrCarrier(f.nativeRoot, oldPath, newPath); err != nil {
		return errnoFor(err)
	}
	delete(f.paths, oldPath)
	f.paths[newPath] = sessionID
	return 0
}

func (f *Filesystem) Unlink(name string) syscall.Errno {
	if !f.canonical {
		return syscall.EPERM
	}
	cleaned := cleanPath(name)
	f.mu.RLock()
	_, managed := f.paths[cleaned]
	f.mu.RUnlock()
	if managed {
		return syscall.EPERM
	}
	nativePath, ok := f.nativePath(cleaned)
	if !ok {
		return syscall.ENOENT
	}
	err := os.Remove(nativePath)
	return errnoFor(err)
}

func (f *Filesystem) sessionForPath(name string) (*vfs.Session, syscall.Errno) {
	cleaned := cleanPath(name)
	if f.canonical {
		f.mu.RLock()
		sessionID := f.paths[cleaned]
		session := f.sessions[sessionID]
		_, nativeFirst := f.nativeFirst[sessionID]
		root := f.nativeRoot
		_, retained := f.retained[cleaned]
		f.mu.RUnlock()
		if session == nil {
			return nil, syscall.ENOENT
		}
		if nativeFirst && root != "" && !retained {
			if info, err := os.Stat(nativePathFromRoot(root, cleaned)); err == nil && !info.IsDir() {
				return nil, syscall.ENOENT
			}
		}
		return session, 0
	}
	if cleaned == "/" || strings.Count(cleaned, "/") != 1 || !strings.HasSuffix(cleaned, ".jsonl") {
		return nil, syscall.ENOENT
	}
	sessionID := strings.TrimSuffix(strings.TrimPrefix(cleaned, "/"), ".jsonl")
	f.mu.RLock()
	session := f.sessions[sessionID]
	loader := f.loader
	f.mu.RUnlock()
	if session != nil {
		return session, 0
	}
	if loader == nil {
		return nil, syscall.ENOENT
	}
	f.loadMu.Lock()
	defer f.loadMu.Unlock()
	f.mu.RLock()
	session = f.sessions[sessionID]
	loader = f.loader
	f.mu.RUnlock()
	if session != nil {
		return session, 0
	}
	if loader == nil {
		return nil, syscall.ENOENT
	}
	loaded, err := loader(sessionID)
	if err != nil {
		return nil, errnoFor(err)
	}
	if loaded == nil {
		return nil, syscall.EIO
	}
	f.mu.Lock()
	if session = f.sessions[sessionID]; session == nil {
		f.sessions[sessionID] = loaded
		session = loaded
	}
	f.mu.Unlock()
	return session, 0
}

func safeSessionID(sessionID string) bool {
	return sessionID != "" && !strings.ContainsAny(sessionID, "/\\\x00")
}

func canonicalSessionPath(name string) bool {
	if name == "" || !strings.HasSuffix(name, ".jsonl") {
		return false
	}
	return strings.HasPrefix(name, "/sessions/") || strings.HasPrefix(name, "/archived_sessions/")
}

func canonicalNamespacePath(name string) bool {
	return name == "/" || name == "/sessions" || name == "/archived_sessions" ||
		strings.HasPrefix(name, "/sessions/") || strings.HasPrefix(name, "/archived_sessions/")
}

func moveAppleDoubleSidecar(root string, oldPath string, newPath string) error {
	if root == "" || oldPath == "" || newPath == "" || !canonicalSessionPath(oldPath) || !canonicalSessionPath(newPath) {
		return nil
	}
	oldSidecar := filepath.Join(nativePathFromRoot(root, path.Dir(oldPath)), "._"+path.Base(oldPath))
	newSidecar := filepath.Join(nativePathFromRoot(root, path.Dir(newPath)), "._"+path.Base(newPath))
	if _, err := os.Lstat(oldSidecar); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return os.Rename(oldSidecar, newSidecar)
}

func managedXattrCarrier(root string, name string) string {
	digest := sha256.Sum256([]byte(cleanPath(name)))
	return filepath.Join(root, ".codexfold-xattrs", hex.EncodeToString(digest[:]))
}

func moveManagedMetadata(root string, oldPath string, newPath string) error {
	if err := moveAppleDoubleSidecar(root, oldPath, newPath); err != nil {
		return err
	}
	return moveManagedXattrCarrier(root, oldPath, newPath)
}

func moveManagedXattrCarrier(root string, oldPath string, newPath string) error {
	if root == "" || oldPath == "" || newPath == "" || !canonicalSessionPath(oldPath) || !canonicalSessionPath(newPath) {
		return nil
	}
	oldCarrier := managedXattrCarrier(root, oldPath)
	newCarrier := managedXattrCarrier(root, newPath)
	if _, err := os.Lstat(oldCarrier); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return os.Rename(oldCarrier, newCarrier)
}

func (f *Filesystem) ensureDirectoryChainLocked(directory string) {
	for directory != "." && directory != "/" && directory != "" {
		f.directories[directory] = struct{}{}
		directory = path.Dir(directory)
	}
	f.directories["/"] = struct{}{}
}

func (f *Filesystem) handle(handleID uint64) (*fileHandle, syscall.Errno) {
	f.mu.RLock()
	handle := f.handles[handleID]
	f.mu.RUnlock()
	if handle == nil {
		return nil, syscall.EBADF
	}
	return handle, 0
}

func (f *Filesystem) nativePath(name string) (string, bool) {
	f.mu.RLock()
	root := f.nativeRoot
	_, retained := f.retained[name]
	f.mu.RUnlock()
	if !f.canonical || root == "" || retained || name == "" || name == "/" || !canonicalNamespacePath(name) {
		return "", false
	}
	return nativePathFromRoot(root, name), true
}

func (f *Filesystem) registerRetainedPathLocked(sessionID string, session *vfs.Session) {
	for retained, currentID := range f.retained {
		if currentID == sessionID {
			delete(f.retained, retained)
		}
	}
	if f.nativeRoot == "" || session == nil {
		return
	}
	snapshot := filepath.Clean(session.State().NativeSnapshot.Path)
	relative, err := filepath.Rel(f.nativeRoot, snapshot)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return
	}
	retained := cleanPath(filepath.ToSlash(relative))
	if canonicalSessionPath(retained) {
		f.retained[retained] = sessionID
	}
}

func nativePathFromRoot(root string, name string) string {
	return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(name, "/")))
}

func cleanPath(name string) string {
	if name == "" || strings.ContainsRune(name, '\x00') || strings.Contains(name, "..") {
		return ""
	}
	return path.Clean("/" + strings.TrimPrefix(name, "/"))
}

func errnoFor(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	switch {
	case errors.Is(err, vfs.ErrWriterBusy):
		return syscall.EBUSY
	case errors.Is(err, os.ErrNotExist):
		return syscall.ENOENT
	case errors.Is(err, os.ErrPermission):
		return syscall.EACCES
	case errors.Is(err, context.Canceled):
		return syscall.EINTR
	default:
		return syscall.EIO
	}
}

package mountfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/jstar0/codexfold/internal/fskitproto"
	"github.com/jstar0/codexfold/internal/vfs"
)

type Attr struct {
	Mode       uint32    `json:"mode"`
	UID        uint32    `json:"uid"`
	GID        uint32    `json:"gid"`
	Size       int64     `json:"size"`
	ModTime    time.Time `json:"mod_time"`
	ChangeTime time.Time `json:"change_time"`
	AccessTime time.Time `json:"access_time"`
}

type SetAttrRequest struct {
	Valid      uint32
	Mode       uint32
	UID        uint32
	GID        uint32
	AccessTime time.Time
	ModTime    time.Time
}

type fileHandle struct {
	mu           sync.Mutex
	path         string
	session      *vfs.Session
	native       *os.File
	nativePath   string
	nativeAppend *nativeAppendState
	read         *vfs.ReadHandle
	write        *vfs.WriteHandle
	append       bool
	appendStream bool
	appendFloor  int64
	appendOffset int64
}

type Filesystem struct {
	mu                sync.RWMutex
	loadMu            sync.Mutex
	sessions          map[string]*vfs.Session
	paths             map[string]string
	retained          map[string]string
	nativeFirst       map[string]struct{}
	directories       map[string]struct{}
	handles           map[uint64]*fileHandle
	next              uint64
	loader            func(string) (*vfs.Session, error)
	canonical         bool
	nativeRoot        string
	nativeJournalRoot string
	nativeAppends     map[string]*nativeAppendState
	namespaceVersion  atomic.Uint64
}

func New() *Filesystem {
	filesystem := &Filesystem{sessions: make(map[string]*vfs.Session), handles: make(map[uint64]*fileHandle), next: 1}
	filesystem.namespaceVersion.Store(1)
	return filesystem
}

func NewCanonical() *Filesystem {
	filesystem := &Filesystem{
		sessions: make(map[string]*vfs.Session), paths: make(map[string]string),
		retained: make(map[string]string), nativeFirst: make(map[string]struct{}),
		directories: map[string]struct{}{`/`: {}, `/sessions`: {}, `/archived_sessions`: {}},
		handles:     make(map[uint64]*fileHandle), nativeAppends: make(map[string]*nativeAppendState),
		next: 1, canonical: true,
	}
	filesystem.namespaceVersion.Store(1)
	return filesystem
}

func (f *Filesystem) NamespaceVersion() uint64 {
	return f.namespaceVersion.Load()
}

func (f *Filesystem) bumpNamespaceVersion() {
	f.namespaceVersion.Add(1)
}

func (f *Filesystem) SetNativeRoot(root string) {
	if root != "" {
		root = filepath.Clean(root)
	}
	f.mu.Lock()
	if f.nativeRoot != root {
		f.nativeAppends = make(map[string]*nativeAppendState)
	}
	f.nativeRoot = root
	f.nativeJournalRoot = ""
	if root != "" {
		f.nativeJournalRoot = filepath.Join(root, ".codexfold-native-journal")
	}
	for retained := range f.retained {
		delete(f.retained, retained)
	}
	for sessionID, session := range f.sessions {
		f.registerRetainedPathLocked(sessionID, session)
	}
	f.mu.Unlock()
}

func (f *Filesystem) RecoverNativeAppendTransactions() error {
	f.mu.RLock()
	root := f.nativeRoot
	journalRoot := f.nativeJournalRoot
	f.mu.RUnlock()
	if root == "" {
		return nil
	}
	if err := recoverNativeAppendTransactions(root, journalRoot); err != nil {
		return err
	}
	f.mu.Lock()
	f.nativeAppends = make(map[string]*nativeAppendState)
	f.mu.Unlock()
	return nil
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
	f.bumpNamespaceVersion()
	return nil
}

func (f *Filesystem) UpsertSession(sessionID string, session *vfs.Session) error {
	if sessionID == "" || strings.ContainsAny(sessionID, "/\\\x00") || session == nil {
		return errors.New("safe session ID and session are required")
	}
	f.mu.Lock()
	f.sessions[sessionID] = session
	f.mu.Unlock()
	f.bumpNamespaceVersion()
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
	f.bumpNamespaceVersion()
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
	f.bumpNamespaceVersion()
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
	f.bumpNamespaceVersion()
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
	f.bumpNamespaceVersion()
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
		if nativePath, ok := f.nativeMetadataPath(cleaned); ok {
			if info, err := os.Lstat(nativePath); err == nil {
				return attrFromFileInfo(info, 0), 0
			}
		}
		return syntheticAttr(syscall.S_IFDIR | 0o700), 0
	}
	if f.canonical {
		f.mu.RLock()
		_, directory := f.directories[cleaned]
		f.mu.RUnlock()
		if directory {
			if nativePath, ok := f.nativeMetadataPath(cleaned); ok {
				if info, err := os.Lstat(nativePath); err == nil {
					return attrFromFileInfo(info, 0), 0
				}
			}
			return syntheticAttr(syscall.S_IFDIR | 0o700), 0
		}
		if session, errno := f.sessionForPath(cleaned); errno == 0 {
			return sessionAttr(session)
		}
		if nativePath, ok := f.nativePath(cleaned); ok {
			info, err := os.Lstat(nativePath)
			if err == nil {
				size := info.Size()
				if state := f.nativeAppendState(nativePath); state != nil {
					size = state.VisibleSize()
				}
				return attrFromFileInfo(info, size), 0
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
	metadata, err := os.Lstat(session.MetadataPath())
	if err != nil {
		return Attr{}, errnoFor(err)
	}
	return attrFromFileInfo(metadata, info.Size), 0
}

func (f *Filesystem) SetAttributes(name string, request SetAttrRequest) syscall.Errno {
	metadataPath, _, errno := f.metadataPath(name)
	if errno != 0 {
		return errno
	}
	info, err := os.Lstat(metadataPath)
	if err != nil {
		return errnoFor(err)
	}
	if request.Valid&fskitproto.SetAttrUID != 0 || request.Valid&fskitproto.SetAttrGID != 0 {
		uid, gid, _, _ := fileOwnershipAndTimes(info)
		if request.Valid&fskitproto.SetAttrUID != 0 {
			uid = request.UID
		}
		if request.Valid&fskitproto.SetAttrGID != 0 {
			gid = request.GID
		}
		if err := os.Chown(metadataPath, int(uid), int(gid)); err != nil {
			return errnoFor(err)
		}
	}
	if request.Valid&fskitproto.SetAttrMode != 0 {
		if err := os.Chmod(metadataPath, os.FileMode(request.Mode)&os.ModePerm); err != nil {
			return errnoFor(err)
		}
	}
	if request.Valid&(fskitproto.SetAttrAccessTime|fskitproto.SetAttrModifyTime) != 0 {
		_, _, accessTime, _ := fileOwnershipAndTimes(info)
		modifyTime := info.ModTime()
		if request.Valid&fskitproto.SetAttrAccessTime != 0 {
			accessTime = request.AccessTime
		}
		if request.Valid&fskitproto.SetAttrModifyTime != 0 {
			modifyTime = request.ModTime
		}
		if err := os.Chtimes(metadataPath, accessTime, modifyTime); err != nil {
			return errnoFor(err)
		}
	}
	return 0
}

func (f *Filesystem) GetXattr(name string, attribute string) ([]byte, syscall.Errno) {
	xattrPath, managed, errno := f.xattrPath(name, false)
	if errno != 0 {
		return nil, errno
	}
	value, err := platformGetXattr(xattrPath, attribute)
	if err != nil {
		if managed && errors.Is(err, os.ErrNotExist) {
			return nil, xattrMissingErrno()
		}
		return nil, errnoFor(err)
	}
	return value, 0
}

func (f *Filesystem) SetXattr(name string, attribute string, value []byte, policy fskitproto.XattrPolicy) syscall.Errno {
	if attribute == "" || strings.ContainsRune(attribute, '\x00') {
		return syscall.EINVAL
	}
	createCarrier := policy != fskitproto.XattrDelete
	xattrPath, managed, errno := f.xattrPath(name, createCarrier)
	if errno != 0 {
		return errno
	}
	if policy == fskitproto.XattrDelete {
		if managed {
			if _, err := os.Lstat(xattrPath); errors.Is(err, os.ErrNotExist) {
				return xattrMissingErrno()
			} else if err != nil {
				return errnoFor(err)
			}
		}
		return errnoFor(platformRemoveXattr(xattrPath, attribute))
	}
	return errnoFor(platformSetXattr(xattrPath, attribute, value, policy))
}

func (f *Filesystem) ListXattrs(name string) ([]string, syscall.Errno) {
	xattrPath, managed, errno := f.xattrPath(name, false)
	if errno != 0 {
		return nil, errno
	}
	attributes, err := platformListXattrs(xattrPath)
	if err != nil {
		if managed && errors.Is(err, os.ErrNotExist) {
			return []string{}, 0
		}
		return nil, errnoFor(err)
	}
	sort.Strings(attributes)
	return attributes, 0
}

func syntheticAttr(mode uint32) Attr {
	now := time.Now()
	return Attr{
		Mode: mode, UID: uint32(os.Getuid()), GID: uint32(os.Getgid()),
		ModTime: now, ChangeTime: now, AccessTime: now,
	}
}

func attrFromFileInfo(info os.FileInfo, size int64) Attr {
	mode := uint32(info.Mode().Perm())
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		mode |= syscall.S_IFLNK
	case info.IsDir():
		mode |= syscall.S_IFDIR
	default:
		mode |= syscall.S_IFREG
	}
	if size == 0 && !info.IsDir() {
		size = info.Size()
	}
	uid, gid, accessTime, changeTime := fileOwnershipAndTimes(info)
	return Attr{
		Mode: mode, UID: uid, GID: gid, Size: size,
		ModTime: info.ModTime(), ChangeTime: changeTime, AccessTime: accessTime,
	}
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
		// FUSE may split or retry one append syscall as positional writes. Keep
		// append and truncate semantics in the transaction layer instead of the
		// backing descriptor.
		nativeFlags := flags &^ (os.O_APPEND | os.O_TRUNC)
		native, err := os.OpenFile(nativePath, nativeFlags, 0o600)
		if err != nil {
			return 0, errnoFor(err)
		}
		state, err := f.loadNativeAppendState(nativePath)
		if err != nil {
			_ = native.Close()
			return 0, errnoFor(err)
		}
		access := flags & (os.O_WRONLY | os.O_RDWR)
		writable := access == os.O_WRONLY || access == os.O_RDWR
		if writable && flags&os.O_TRUNC != 0 {
			if err := state.Truncate(0); err != nil {
				_ = native.Close()
				return 0, errnoFor(err)
			}
		}
		f.mu.Lock()
		handleID := f.next
		f.next++
		f.handles[handleID] = &fileHandle{
			path: cleanPath(name), native: native, nativePath: nativePath, nativeAppend: state,
			// macOS may strip O_APPEND before invoking FUSE. Every writable
			// canonical JSONL handle therefore uses positional transaction staging.
			append: writable && nativeTransactionPath(cleanPath(name)),
		}
		f.mu.Unlock()
		return handleID, 0
	}
	handle := &fileHandle{path: cleanPath(name), session: session, append: flags&os.O_APPEND != 0}
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
		n, err := handle.nativeAppend.ReadAt(handle.native, destination, offset)
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
			n, err = handle.nativeAppend.Stage(data, offset)
		} else {
			if handle.nativeAppend.HasPending() {
				return 0, syscall.EBUSY
			}
			n, err = handle.native.WriteAt(data, offset)
			if err == nil {
				err = handle.nativeAppend.Refresh()
			}
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
			if err == nil {
				if !handle.appendStream {
					handle.appendFloor = offset
				}
				handle.appendStream = true
				handle.appendOffset = offset + int64(n)
			}
		} else if handle.appendStream &&
			offset >= handle.appendFloor && offset < handle.appendOffset &&
			info.Size == handle.appendOffset && completeJSONL(data) {
			n, err = handle.write.Append(context.Background(), data)
			if err == nil {
				handle.appendOffset += int64(n)
			}
		} else {
			handle.appendStream = false
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

func (f *Filesystem) UseRandomWrites(handleID uint64) syscall.Errno {
	handle, errno := f.handle(handleID)
	if errno != 0 {
		return syscall.EBADF
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.native != nil && handle.append {
		if err := handle.nativeAppend.Commit(); err != nil {
			return errnoFor(err)
		}
	}
	handle.append = false
	handle.appendStream = false
	return 0
}

func (f *Filesystem) Truncate(handleID uint64, size int64) syscall.Errno {
	handle, errno := f.handle(handleID)
	if errno != 0 {
		return syscall.EBADF
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.native != nil {
		return errnoFor(handle.nativeAppend.Truncate(size))
	}
	if handle.write == nil {
		return syscall.EBADF
	}
	if err := handle.write.Truncate(context.Background(), size); err != nil {
		return errnoFor(err)
	}
	if handle.appendStream && size != handle.appendOffset {
		handle.appendStream = false
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
				state, err := f.loadNativeAppendState(nativePath)
				if err != nil {
					return errnoFor(err)
				}
				return errnoFor(state.Truncate(size))
			}
		}
		return errno
	}
	if handle := f.lockActiveWriter(session); handle != nil {
		defer handle.mu.Unlock()
		if err := handle.write.Truncate(context.Background(), size); err != nil {
			return errnoFor(err)
		}
		if handle.appendStream && size != handle.appendOffset {
			handle.appendStream = false
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

func completeJSONL(data []byte) bool {
	if len(data) == 0 || data[len(data)-1] != '\n' || !utf8.Valid(data) {
		return false
	}
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		if end <= 0 || !json.Valid(data[:end]) {
			return false
		}
		data = data[end+1:]
	}
	return true
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
		if handle.append {
			return errnoFor(handle.nativeAppend.CommitAvailable())
		}
		return errnoFor(handle.native.Sync())
	}
	if handle.write == nil {
		return 0
	}
	return errnoFor(handle.write.Sync())
}

func (f *Filesystem) Flush(handleID uint64) syscall.Errno {
	handle, errno := f.handle(handleID)
	if errno != 0 {
		return errno
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.native != nil && handle.append {
		if f.nativeAppendIsLastWriter(handleID, handle.nativeAppend) {
			return errnoFor(handle.nativeAppend.Commit())
		}
		return errnoFor(handle.nativeAppend.CommitAvailable())
	}
	return 0
}

func (f *Filesystem) Release(handleID uint64) syscall.Errno {
	f.mu.Lock()
	handle, ok := f.handles[handleID]
	lastNativeWriter := false
	if ok {
		lastNativeWriter = f.nativeAppendIsLastWriterLocked(handleID, handle.nativeAppend)
		delete(f.handles, handleID)
	}
	f.mu.Unlock()
	if !ok {
		return syscall.EBADF
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.native != nil {
		var commitErr error
		if handle.append {
			if lastNativeWriter {
				commitErr = handle.nativeAppend.Commit()
			} else {
				commitErr = handle.nativeAppend.CommitAvailable()
			}
		}
		return errnoFor(errors.Join(commitErr, handle.native.Close()))
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

func (f *Filesystem) nativeAppendIsLastWriter(handleID uint64, state *nativeAppendState) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.nativeAppendIsLastWriterLocked(handleID, state)
}

func (f *Filesystem) nativeAppendIsLastWriterLocked(handleID uint64, state *nativeAppendState) bool {
	if state == nil {
		return false
	}
	for currentID, current := range f.handles {
		if currentID != handleID && current.nativeAppend == state && current.append {
			return false
		}
	}
	return true
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
	f.bumpNamespaceVersion()
	return 0
}

func (f *Filesystem) Rename(oldName string, newName string) syscall.Errno {
	if !f.canonical {
		return syscall.EPERM
	}
	oldPath, newPath := cleanPath(oldName), cleanPath(newName)
	openUnlinkRename := canonicalSessionPath(oldPath) && fskitOpenUnlinkPath(newPath) && path.Dir(oldPath) == path.Dir(newPath)
	if !canonicalSessionPath(oldPath) || (!canonicalSessionPath(newPath) && !openUnlinkRename) {
		return syscall.EPERM
	}
	f.mu.Lock()
	sessionID, exists := f.paths[oldPath]
	if !exists {
		if _, hidden := f.retained[oldPath]; hidden {
			f.mu.Unlock()
			return syscall.ENOENT
		}
		if f.nativePathBusyLocked(oldPath) && !openUnlinkRename {
			f.mu.Unlock()
			return syscall.EBUSY
		}
		root := f.nativeRoot
		oldNative := nativePathFromRoot(root, oldPath)
		newNative := nativePathFromRoot(root, newPath)
		appendState := f.nativeAppends[filepath.Clean(oldNative)]
		f.mu.Unlock()
		if root == "" {
			return syscall.ENOENT
		}
		var renameErr error
		if appendState != nil {
			renameErr = appendState.Relocate(newNative)
		} else {
			renameErr = os.Rename(oldNative, newNative)
		}
		if renameErr != nil {
			return errnoFor(renameErr)
		}
		f.mu.Lock()
		delete(f.nativeAppends, filepath.Clean(oldNative))
		if appendState != nil {
			f.nativeAppends[filepath.Clean(newNative)] = appendState
		} else {
			delete(f.nativeAppends, filepath.Clean(newNative))
		}
		for _, handle := range f.handles {
			if handle.path == oldPath {
				handle.path = newPath
				handle.nativePath = newNative
			}
		}
		f.mu.Unlock()
		f.bumpNamespaceVersion()
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
	for _, handle := range f.handles {
		if handle.path == oldPath {
			handle.path = newPath
		}
	}
	f.bumpNamespaceVersion()
	return 0
}

func (f *Filesystem) Unlink(name string) syscall.Errno {
	if !f.canonical {
		return syscall.EPERM
	}
	cleaned := cleanPath(name)
	f.mu.RLock()
	_, managed := f.paths[cleaned]
	busy := f.nativePathBusyLocked(cleaned)
	f.mu.RUnlock()
	if managed {
		if fskitOpenUnlinkPath(cleaned) {
			f.mu.Lock()
			delete(f.paths, cleaned)
			delete(f.retained, cleaned)
			f.mu.Unlock()
			f.bumpNamespaceVersion()
			return 0
		}
		return syscall.EPERM
	}
	if busy {
		return syscall.EBUSY
	}
	nativePath, ok := f.nativePath(cleaned)
	if !ok {
		return syscall.ENOENT
	}
	err := os.Remove(nativePath)
	if err == nil {
		f.mu.Lock()
		delete(f.nativeAppends, filepath.Clean(nativePath))
		f.mu.Unlock()
		f.bumpNamespaceVersion()
	}
	return errnoFor(err)
}

func (f *Filesystem) Rmdir(name string) syscall.Errno {
	if !f.canonical {
		return syscall.EPERM
	}
	cleaned := cleanPath(name)
	if cleaned == "/" || cleaned == "/sessions" || cleaned == "/archived_sessions" || !canonicalNamespacePath(cleaned) {
		return syscall.EPERM
	}
	f.mu.RLock()
	for route := range f.paths {
		if path.Dir(route) == cleaned || strings.HasPrefix(route, cleaned+"/") {
			f.mu.RUnlock()
			return syscall.ENOTEMPTY
		}
	}
	root := f.nativeRoot
	f.mu.RUnlock()
	if root == "" {
		return syscall.ENOENT
	}
	if err := os.Remove(nativePathFromRoot(root, cleaned)); err != nil {
		return errnoFor(err)
	}
	f.mu.Lock()
	delete(f.directories, cleaned)
	f.mu.Unlock()
	f.bumpNamespaceVersion()
	return 0
}

func (f *Filesystem) SyncAll() syscall.Errno {
	f.mu.RLock()
	handles := make([]uint64, 0, len(f.handles))
	for handleID := range f.handles {
		handles = append(handles, handleID)
	}
	f.mu.RUnlock()
	for _, handleID := range handles {
		if errno := f.Fsync(handleID); errno != 0 && errno != syscall.EBADF {
			return errno
		}
	}
	return 0
}

func (f *Filesystem) loadNativeAppendState(nativePath string) (*nativeAppendState, error) {
	cleaned := filepath.Clean(nativePath)
	f.mu.RLock()
	state := f.nativeAppends[cleaned]
	journalRoot := f.nativeJournalRoot
	f.mu.RUnlock()
	if state != nil {
		if err := state.RefreshIfIdle(); err != nil {
			return nil, err
		}
		return state, nil
	}
	created, err := newNativeAppendState(cleaned, journalRoot)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	if state = f.nativeAppends[cleaned]; state == nil {
		f.nativeAppends[cleaned] = created
		state = created
	}
	f.mu.Unlock()
	if state != created {
		if err := state.RefreshIfIdle(); err != nil {
			return nil, err
		}
	}
	return state, nil
}

func (f *Filesystem) nativeAppendState(nativePath string) *nativeAppendState {
	f.mu.RLock()
	state := f.nativeAppends[filepath.Clean(nativePath)]
	f.mu.RUnlock()
	return state
}

func (f *Filesystem) nativePathBusyLocked(name string) bool {
	nativePath := filepath.Clean(nativePathFromRoot(f.nativeRoot, name))
	if state := f.nativeAppends[nativePath]; state != nil && state.HasPending() {
		return true
	}
	for _, handle := range f.handles {
		if filepath.Clean(handle.nativePath) == nativePath {
			return true
		}
	}
	return false
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

func fskitOpenUnlinkPath(name string) bool {
	base := path.Base(name)
	return canonicalNamespacePath(name) && strings.HasPrefix(base, ".nfs.") && len(base) > len(".nfs.")
}

func nativeTransactionPath(name string) bool {
	return canonicalSessionPath(name) && !strings.HasPrefix(path.Base(name), "._")
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

func (f *Filesystem) HandlePath(handleID uint64) (string, syscall.Errno) {
	f.mu.RLock()
	handle := f.handles[handleID]
	if handle == nil {
		f.mu.RUnlock()
		return "", syscall.EBADF
	}
	name := handle.path
	f.mu.RUnlock()
	return name, 0
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

func (f *Filesystem) nativeMetadataPath(name string) (string, bool) {
	f.mu.RLock()
	root := f.nativeRoot
	f.mu.RUnlock()
	if !f.canonical || root == "" || !canonicalNamespacePath(name) {
		return "", false
	}
	if name == "/" {
		return root, true
	}
	return nativePathFromRoot(root, name), true
}

func (f *Filesystem) metadataPath(name string) (string, bool, syscall.Errno) {
	cleaned := cleanPath(name)
	if session, errno := f.sessionForPath(cleaned); errno == 0 {
		return session.MetadataPath(), true, 0
	}
	if nativePath, ok := f.nativeMetadataPath(cleaned); ok {
		if _, err := os.Lstat(nativePath); err != nil {
			return "", false, errnoFor(err)
		}
		return nativePath, false, 0
	}
	return "", false, syscall.ENOENT
}

func (f *Filesystem) xattrPath(name string, create bool) (string, bool, syscall.Errno) {
	cleaned := cleanPath(name)
	if _, errno := f.sessionForPath(cleaned); errno == 0 {
		f.mu.RLock()
		root := f.nativeRoot
		f.mu.RUnlock()
		if root == "" {
			return "", true, syscall.ENOTSUP
		}
		carrier := managedXattrCarrier(root, cleaned)
		if create {
			if err := os.MkdirAll(filepath.Dir(carrier), 0o700); err != nil {
				return "", true, errnoFor(err)
			}
			file, err := os.OpenFile(carrier, os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				return "", true, errnoFor(err)
			}
			if err := file.Close(); err != nil {
				return "", true, errnoFor(err)
			}
		}
		return carrier, true, 0
	}
	if nativePath, ok := f.nativeMetadataPath(cleaned); ok {
		if _, err := os.Lstat(nativePath); err != nil {
			return "", false, errnoFor(err)
		}
		return nativePath, false, 0
	}
	return "", false, syscall.ENOENT
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
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
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

package mountfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
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
	read    *vfs.ReadHandle
	write   *vfs.WriteHandle
	append  bool
}

type Filesystem struct {
	mu       sync.RWMutex
	loadMu   sync.Mutex
	sessions map[string]*vfs.Session
	handles  map[uint64]*fileHandle
	next     uint64
	loader   func(string) (*vfs.Session, error)
}

func New() *Filesystem {
	return &Filesystem{sessions: make(map[string]*vfs.Session), handles: make(map[uint64]*fileHandle), next: 1}
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

func (f *Filesystem) SetSessionLoader(loader func(string) (*vfs.Session, error)) {
	f.mu.Lock()
	f.loader = loader
	f.mu.Unlock()
}

func (f *Filesystem) ReadDir(name string) ([]string, syscall.Errno) {
	if cleanPath(name) != "/" {
		return nil, syscall.ENOTDIR
	}
	f.mu.RLock()
	entries := make([]string, 0, len(f.sessions))
	for sessionID := range f.sessions {
		entries = append(entries, sessionID+".jsonl")
	}
	f.mu.RUnlock()
	sort.Strings(entries)
	return entries, 0
}

func (f *Filesystem) Getattr(name string) (Attr, syscall.Errno) {
	if cleanPath(name) == "/" {
		return Attr{Mode: syscall.S_IFDIR | 0o700}, 0
	}
	session, errno := f.sessionForPath(name)
	if errno != 0 {
		return Attr{}, errno
	}
	info, err := session.VisibleInfo()
	if err != nil {
		return Attr{}, errnoFor(err)
	}
	return Attr{Mode: syscall.S_IFREG | 0o600, Size: info.Size, ModTime: info.ModTime}, 0
}

func (f *Filesystem) Open(name string, flags int) (uint64, syscall.Errno) {
	session, errno := f.sessionForPath(name)
	if errno != 0 {
		return 0, errno
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
	if errno != 0 || handle.read == nil {
		return 0, syscall.EBADF
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	n, err := handle.read.ReadAt(context.Background(), destination, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, errnoFor(err)
	}
	return n, 0
}

func (f *Filesystem) Write(handleID uint64, data []byte, offset int64) (int, syscall.Errno) {
	handle, errno := f.handle(handleID)
	if errno != 0 || handle.write == nil {
		return 0, syscall.EBADF
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
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
	if errno != 0 || handle.write == nil {
		return syscall.EBADF
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
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

func (f *Filesystem) Rename(string, string) syscall.Errno { return syscall.EPERM }
func (f *Filesystem) Unlink(string) syscall.Errno         { return syscall.EPERM }

func (f *Filesystem) sessionForPath(name string) (*vfs.Session, syscall.Errno) {
	cleaned := cleanPath(name)
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

func (f *Filesystem) handle(handleID uint64) (*fileHandle, syscall.Errno) {
	f.mu.RLock()
	handle := f.handles[handleID]
	f.mu.RUnlock()
	if handle == nil {
		return nil, syscall.EBADF
	}
	return handle, 0
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

package mountfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/samekind/codexfold/internal/nativezst"
)

// ErrNativeZstBusy means a handle is open on the file; it is retried later.
var ErrNativeZstBusy = errors.New("native .zst is open")

// SetNativeZstSource lets the mount serve Codex .zst files whose data blocks
// were folded into store. Without it a folded file fails with EIO instead of
// returning its released, all-zero blocks.
func (f *Filesystem) SetNativeZstSource(source *nativezst.Source) {
	f.mu.Lock()
	f.nativeZst = source
	f.mu.Unlock()
}

// openFoldedNative opens a folded .zst. folded is false for every other file,
// which then takes the ordinary native path.
func (f *Filesystem) openFoldedNative(nativePath string, flags int) (*nativezst.Reader, syscall.Errno, bool) {
	if !strings.HasSuffix(nativePath, nativezst.Suffix) {
		return nil, 0, false
	}
	marker, folded, err := nativezst.ReadMarker(nativePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false
	}
	if err != nil {
		return nil, syscall.EIO, true
	}
	if !folded {
		return nil, 0, false
	}
	// Codex only reads, renames and deletes a compressed rollout.
	if access := flags & (os.O_WRONLY | os.O_RDWR); access != os.O_RDONLY || flags&os.O_TRUNC != 0 {
		return nil, syscall.EPERM, true
	}
	f.mu.RLock()
	source := f.nativeZst
	f.mu.RUnlock()
	if source == nil {
		return nil, syscall.EIO, true
	}
	reader, err := source.Open(marker)
	if err != nil {
		return nil, syscall.EIO, true
	}
	return reader, 0, true
}

func (f *Filesystem) nativeZstRoot() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.nativeRoot
}

// ScanNativeZst walks the native tree with renames held off, so a folded file
// cannot move between directories while its marker is being counted.
func (f *Filesystem) ScanNativeZst(ctx context.Context) (nativezst.Scan, error) {
	root := f.nativeZstRoot()
	if root == "" {
		return nativezst.Scan{}, errors.New("native root is not configured")
	}
	f.renameMu.RLock()
	defer f.renameMu.RUnlock()
	return nativezst.ScanNative(ctx, root)
}

func (f *Filesystem) nativeZstName(path string) (string, error) {
	root := f.nativeZstRoot()
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
		return "", errors.New("native .zst lies outside the native root")
	}
	return "/" + filepath.ToSlash(relative), nil
}

// withNativeZstExclusive runs mutate while no open, rename or unlink can
// reach path, and only if no handle is open on it.
func (f *Filesystem) withNativeZstExclusive(path string, mutate func() error) error {
	name, err := f.nativeZstName(path)
	if err != nil {
		return err
	}
	f.renameMu.Lock()
	defer f.renameMu.Unlock()
	f.mu.RLock()
	busy := f.nativePathBusyLocked(name)
	f.mu.RUnlock()
	if busy {
		return ErrNativeZstBusy
	}
	f.markNativeInternalMutation(path)
	return mutate()
}

// ConvertNativeZst releases the blocks of a prepared file.
func (f *Filesystem) ConvertNativeZst(store string, prepared nativezst.Prepared) error {
	return f.withNativeZstExclusive(prepared.Path, func() error {
		return nativezst.Convert(store, prepared)
	})
}

// FinishNativeZst completes a conversion that stopped after its marker. The
// served bytes are proven before the lock is taken.
func (f *Filesystem) FinishNativeZst(ctx context.Context, source *nativezst.Source, file nativezst.NativeFile) error {
	if err := nativezst.Prove(ctx, source, file.Marker); err != nil {
		return err
	}
	return f.withNativeZstExclusive(file.Path, func() error {
		return nativezst.FinishRelease(file.Path, file.Info)
	})
}

// IsFoldedHandle reports whether an open handle serves a folded .zst.
func (f *Filesystem) IsFoldedHandle(handleID uint64) bool {
	handle, errno := f.handle(handleID)
	if errno != 0 {
		return false
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	return handle.folded != nil
}

package mountfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/vfs"
)

// A read-only managed handle holds a resolver bound to one pack generation. When
// that generation is replaced, the same resolver fails identically forever, so a
// plain retry cannot recover. The read must reload the session once and retry
// against the current generation instead of reporting EIO to Codex.
func TestFilesystemReadReloadsAStaleSessionOnceInsteadOfReportingEIO(t *testing.T) {
	source := []byte("reloaded-history\n")
	stale := mountSessionFixture(t, "stale", source)
	fresh := mountSessionFixture(t, "stale", source)
	filesystem := New()
	filesystem.loadRetryInterval = time.Millisecond
	filesystem.loadRetrySleep = func(time.Duration) {}
	loads := 0
	filesystem.SetSessionLoader(func(string) (*vfs.Session, error) {
		loads++
		if loads == 1 {
			return stale, nil
		}
		return fresh, nil
	})
	handleID, errno := filesystem.Open("/stale.jsonl", os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open errno=%v", errno)
	}
	t.Cleanup(func() { _ = filesystem.Release(handleID) })

	// Replace the open handle's reader with one whose generation is gone, which
	// is what an open resolver sees after its generation is collected.
	filesystem.mu.RLock()
	handle := filesystem.handles[handleID]
	filesystem.mu.RUnlock()
	handle.read = collectedGenerationReader(t, source)

	destination := make([]byte, len(source))
	n, errno := filesystem.Read(handleID, destination, 0)
	if errno != 0 {
		t.Fatalf("read after generation replacement errno=%v (%d), want a reloaded success", errno, int(errno))
	}
	if string(destination[:n]) != string(source) {
		t.Fatalf("reloaded bytes = %q, want %q", destination[:n], source)
	}
	if loads < 2 {
		t.Fatalf("loader calls = %d, want the session to be reloaded", loads)
	}
}

// The reload path must refuse a handle that owns a writer: replacing its session
// would abandon append state.
func TestFilesystemReadDoesNotReloadAWritableHandle(t *testing.T) {
	filesystem := New()
	handle := &fileHandle{write: &vfs.WriteHandle{}}
	if _, errno := filesystem.rereadThroughReloadedSession(handle, make([]byte, 4), 0); errno != syscall.EIO {
		t.Fatalf("errno = %v, want EIO refusal for a writable handle", errno)
	}
}

// A classified read failure keeps its exact errno and must not trigger a reload.
func TestFilesystemReadKeepsClassifiedErrnoWithoutReloading(t *testing.T) {
	filesystem := New()
	loads := 0
	filesystem.SetSessionLoader(func(string) (*vfs.Session, error) {
		loads++
		return nil, os.ErrNotExist
	})
	if _, errno := filesystem.Getattr("/absent.jsonl"); errno != syscall.ENOENT {
		t.Fatalf("errno = %v, want ENOENT", errno)
	}
	if loads != 1 {
		t.Fatalf("loader calls = %d, want 1", loads)
	}
}

// collectedGenerationReader builds a read handle whose object reader reports the
// unclassified failure a collected pack generation produces.
func collectedGenerationReader(t *testing.T, source []byte) *vfs.ReadHandle {
	t.Helper()
	root := t.TempDir()
	digest := sha256.Sum256(source)
	hexDigest := hex.EncodeToString(digest[:])
	nativePath := filepath.Join(root, "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := fold.Manifest{
		Version: fold.ManifestVersion, Kind: fold.ManifestKind,
		Session: fold.ManifestSession{ID: "collected", RolloutPath: nativePath},
		Source:  fold.ManifestSource{Bytes: int64(len(source)), SHA256: hexDigest},
		Parts:   []fold.Part{{Kind: fold.PartResidual, Object: fold.ObjectRef{SHA256: hexDigest, RawBytes: int64(len(source))}}},
	}
	persistMountManifestFixture(t, fold.ManifestPath(root, manifest.Session.ID), manifest)
	session, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest,
		Reader:         collectedObjectReader{},
		NativeSnapshot: vfs.NativeFile{Path: nativePath, Bytes: int64(len(source)), SHA256: hexDigest},
	})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := session.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

// collectedObjectReader reports the exact error class a replaced pack generation
// produces: not a syscall errno, not os.ErrNotExist, so errnoFor maps it to EIO.
type collectedObjectReader struct{}

func (collectedObjectReader) ReadAt(_ context.Context, ref fold.ObjectRef, _ []byte, _ int64) (int, error) {
	return 0, fmt.Errorf("object %s is not packed", ref.SHA256)
}

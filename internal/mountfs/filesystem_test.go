package mountfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/jstar0/codexfold/internal/fold"
	"github.com/jstar0/codexfold/internal/vfs"
)

func TestFilesystemListsStatsReadsAndAppendsSession(t *testing.T) {
	filesystem, source := mountFixture(t)
	entries, errno := filesystem.ReadDir("/")
	if errno != 0 || len(entries) != 1 || entries[0] != "session.jsonl" {
		t.Fatalf("ReadDir = %#v errno=%v", entries, errno)
	}
	attribute, errno := filesystem.Getattr("/session.jsonl")
	if errno != 0 || attribute.Mode&syscall.S_IFREG == 0 || attribute.Size != int64(len(source)) {
		t.Fatalf("Getattr = %#v errno=%v", attribute, errno)
	}

	readHandle, errno := filesystem.Open("/session.jsonl", os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open read errno=%v", errno)
	}
	buffer := make([]byte, len(source))
	n, errno := filesystem.Read(readHandle, buffer, 0)
	if errno != 0 || !bytes.Equal(buffer[:n], source) {
		t.Fatalf("Read = %d errno=%v bytes=%q", n, errno, buffer[:n])
	}
	if errno := filesystem.Release(readHandle); errno != 0 {
		t.Fatalf("Release read errno=%v", errno)
	}

	writeHandle, errno := filesystem.Open("/session.jsonl", os.O_WRONLY|os.O_APPEND)
	if errno != 0 {
		t.Fatalf("Open append errno=%v", errno)
	}
	if n, errno := filesystem.Write(writeHandle, []byte("-tail"), 0); errno != 0 || n != 5 {
		t.Fatalf("Write append = %d errno=%v", n, errno)
	}
	if errno := filesystem.Fsync(writeHandle); errno != 0 {
		t.Fatalf("Fsync errno=%v", errno)
	}
	_ = filesystem.Release(writeHandle)

	newHandle, errno := filesystem.Open("/session.jsonl", os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open current errno=%v", errno)
	}
	current := make([]byte, len(source)+5)
	n, errno = filesystem.Read(newHandle, current, 0)
	if errno != 0 || !bytes.Equal(current[:n], append(append([]byte(nil), source...), []byte("-tail")...)) {
		t.Fatalf("current read differs: n=%d errno=%v bytes=%q", n, errno, current[:n])
	}
	_ = filesystem.Release(newHandle)
}

func TestFilesystemRandomWriteTruncateAndWriterExclusion(t *testing.T) {
	filesystem, source := mountFixture(t)
	first, errno := filesystem.Open("/session.jsonl", os.O_RDWR)
	if errno != 0 {
		t.Fatalf("Open first writer errno=%v", errno)
	}
	if _, errno := filesystem.Open("/session.jsonl", os.O_WRONLY); errno != syscall.EBUSY {
		t.Fatalf("second writer errno=%v, want EBUSY", errno)
	}
	if n, errno := filesystem.Write(first, []byte("PATCH"), 2); errno != 0 || n != 5 {
		t.Fatalf("random Write = %d errno=%v", n, errno)
	}
	current := make([]byte, len(source))
	if n, errno := filesystem.Read(first, current, 0); errno != 0 || n != len(source) || string(current[2:7]) != "PATCH" {
		t.Fatalf("read-after-write = %d errno=%v bytes=%q", n, errno, current)
	}
	if errno := filesystem.Truncate(first, int64(len(source)-3)); errno != 0 {
		t.Fatalf("Truncate errno=%v", errno)
	}
	_ = filesystem.Release(first)
	attribute, errno := filesystem.Getattr("/session.jsonl")
	if errno != 0 || attribute.Size != int64(len(source)-3) {
		t.Fatalf("truncated attribute=%#v errno=%v", attribute, errno)
	}
}

func TestFilesystemWriteAtVisibleEOFUsesDeltaWithoutCopyOnWrite(t *testing.T) {
	source := []byte("first\nsecond\nthird\n")
	session := mountSessionFixture(t, "session", source)
	filesystem := New()
	if err := filesystem.AddSession("session", session); err != nil {
		t.Fatal(err)
	}
	handle, errno := filesystem.Open("/session.jsonl", os.O_RDWR)
	if errno != 0 {
		t.Fatalf("Open writer errno=%v", errno)
	}
	t.Cleanup(func() { _ = filesystem.Release(handle) })
	tail := []byte("tail\n")
	if n, errno := filesystem.Write(handle, tail, int64(len(source))); errno != 0 || n != len(tail) {
		t.Fatalf("Write at EOF = %d errno=%v", n, errno)
	}
	if state := session.State(); state.BackingPath != "" {
		t.Fatalf("EOF write created copy-on-write backing %q", state.BackingPath)
	} else if info, err := os.Stat(state.DeltaPath); err != nil || info.Size() != int64(len(tail)) {
		t.Fatalf("delta after EOF write: info=%#v err=%v", info, err)
	}
	current := make([]byte, len(source)+len(tail))
	if n, errno := filesystem.Read(handle, current, 0); errno != 0 || n != len(current) {
		t.Fatalf("Read after EOF write = %d errno=%v", n, errno)
	}
	want := append(append([]byte(nil), source...), tail...)
	if !bytes.Equal(current, want) {
		t.Fatalf("visible bytes differ: got=%q want=%q", current, want)
	}
}

func TestFilesystemPathTruncateUsesTheActiveWriter(t *testing.T) {
	filesystem, source := mountFixture(t)
	handle, errno := filesystem.Open("/session.jsonl", os.O_RDWR)
	if errno != 0 {
		t.Fatalf("Open writer errno=%v", errno)
	}
	t.Cleanup(func() { _ = filesystem.Release(handle) })
	wantSize := int64(len(source) - 3)
	if errno := filesystem.TruncatePath("/session.jsonl", wantSize); errno != 0 {
		t.Fatalf("TruncatePath with active writer errno=%v", errno)
	}
	attribute, errno := filesystem.Getattr("/session.jsonl")
	if errno != 0 || attribute.Size != wantSize {
		t.Fatalf("Getattr after path truncate = %#v errno=%v", attribute, errno)
	}
}

func TestFilesystemLoadsAMissingSessionOnceOnFirstAccess(t *testing.T) {
	source := []byte("loaded")
	session := mountSessionFixture(t, "loaded", source)
	filesystem := New()
	loads := 0
	filesystem.SetSessionLoader(func(sessionID string) (*vfs.Session, error) {
		loads++
		if sessionID != "loaded" {
			return nil, os.ErrNotExist
		}
		return session, nil
	})
	for attempt := 0; attempt < 2; attempt++ {
		attribute, errno := filesystem.Getattr("/loaded.jsonl")
		if errno != 0 || attribute.Size != int64(len(source)) {
			t.Fatalf("Getattr attempt %d = %#v errno=%v", attempt, attribute, errno)
		}
	}
	if loads != 1 {
		t.Fatalf("session loader calls = %d, want 1", loads)
	}
}

func TestFilesystemRejectsUnsafeAndManagementMutations(t *testing.T) {
	filesystem, _ := mountFixture(t)
	if _, errno := filesystem.Open("/../session.jsonl", os.O_RDONLY); errno != syscall.ENOENT {
		t.Fatalf("unsafe path errno=%v", errno)
	}
	if errno := filesystem.Rename("/session.jsonl", "/other.jsonl"); errno != syscall.EPERM {
		t.Fatalf("Rename errno=%v, want EPERM", errno)
	}
	if errno := filesystem.Unlink("/session.jsonl"); errno != syscall.EPERM {
		t.Fatalf("Unlink errno=%v, want EPERM", errno)
	}
}

func TestFilesystemUpsertChangesNewOpensWithoutInvalidatingExistingHandles(t *testing.T) {
	first := mountSessionFixture(t, "first-session", []byte("first"))
	second := mountSessionFixture(t, "second-session", []byte("second"))
	filesystem := New()
	if err := filesystem.AddSession("session", first); err != nil {
		t.Fatal(err)
	}
	oldHandle, errno := filesystem.Open("/session.jsonl", os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open old handle: %v", errno)
	}
	if err := filesystem.UpsertSession("session", second); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	newHandle, errno := filesystem.Open("/session.jsonl", os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open new handle: %v", errno)
	}
	oldBytes := make([]byte, 5)
	if n, errno := filesystem.Read(oldHandle, oldBytes, 0); errno != 0 || n != 5 || string(oldBytes) != "first" {
		t.Fatalf("old handle changed: n=%d errno=%v bytes=%q", n, errno, oldBytes)
	}
	newBytes := make([]byte, 6)
	if n, errno := filesystem.Read(newHandle, newBytes, 0); errno != 0 || n != 6 || string(newBytes) != "second" {
		t.Fatalf("new handle did not use replacement: n=%d errno=%v bytes=%q", n, errno, newBytes)
	}
	_ = filesystem.Release(oldHandle)
	_ = filesystem.Release(newHandle)
}

func TestMountWithoutFuseBuildReturnsPrerequisiteError(t *testing.T) {
	if Available() {
		t.Skip("FUSE-enabled builds are covered by the gated real mount test")
	}
	err := Mount(context.Background(), HostOptions{MountPoint: t.TempDir(), Filesystem: New()})
	if !errors.Is(err, ErrPrerequisite) {
		t.Fatalf("Mount error = %v, want ErrPrerequisite", err)
	}
}

type mountReader map[string][]byte

func (r mountReader) ReadAt(_ context.Context, ref fold.ObjectRef, destination []byte, offset int64) (int, error) {
	data := r[ref.SHA256]
	if offset >= int64(len(data)) {
		return 0, io.EOF
	}
	n := copy(destination, data[offset:])
	if n < len(destination) {
		return n, io.EOF
	}
	return n, nil
}

func mountFixture(t *testing.T) (*Filesystem, []byte) {
	t.Helper()
	root := t.TempDir()
	source := []byte("first\nsecond\nthird\n")
	digest := sha256.Sum256(source)
	hexDigest := hex.EncodeToString(digest[:])
	nativePath := filepath.Join(root, "native.jsonl")
	if err := os.WriteFile(nativePath, source, 0o600); err != nil {
		t.Fatalf("write native: %v", err)
	}
	manifest := fold.Manifest{Version: fold.ManifestVersion, Kind: fold.ManifestKind, Session: fold.ManifestSession{ID: "session", RolloutPath: nativePath}, Source: fold.ManifestSource{Bytes: int64(len(source)), SHA256: hexDigest}, Parts: []fold.Part{{Kind: fold.PartResidual, Object: fold.ObjectRef{SHA256: hexDigest, RawBytes: int64(len(source))}}}}
	session, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{Root: root, ManifestPath: filepath.Join(root, "manifest.json"), Manifest: manifest, Reader: mountReader{hexDigest: source}, NativeSnapshot: vfs.NativeFile{Path: nativePath, Bytes: int64(len(source)), SHA256: hexDigest}})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	filesystem := New()
	if err := filesystem.AddSession("session", session); err != nil {
		t.Fatalf("AddSession: %v", err)
	}
	return filesystem, source
}

func mountSessionFixture(t *testing.T, sessionID string, source []byte) *vfs.Session {
	t.Helper()
	root := t.TempDir()
	digest := sha256.Sum256(source)
	hexDigest := hex.EncodeToString(digest[:])
	nativePath := filepath.Join(root, "native.jsonl")
	if err := os.WriteFile(nativePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := fold.Manifest{Version: fold.ManifestVersion, Kind: fold.ManifestKind, Session: fold.ManifestSession{ID: sessionID, RolloutPath: nativePath}, Source: fold.ManifestSource{Bytes: int64(len(source)), SHA256: hexDigest}, Parts: []fold.Part{{Kind: fold.PartResidual, Object: fold.ObjectRef{SHA256: hexDigest, RawBytes: int64(len(source))}}}}
	session, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{Root: root, ManifestPath: filepath.Join(root, "manifest.json"), Manifest: manifest, Reader: mountReader{hexDigest: source}, NativeSnapshot: vfs.NativeFile{Path: nativePath, Bytes: int64(len(source)), SHA256: hexDigest}})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

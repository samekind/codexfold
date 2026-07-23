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
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jstar0/codexfold/internal/fold"
	"github.com/jstar0/codexfold/internal/vfs"
)

func TestFilesystemIOIdleRequiresNoActiveIOAndCompletedIdleWindow(t *testing.T) {
	filesystem := New()
	filesystem.lastIO.Store(time.Now().Add(-time.Second).UnixNano())
	if !filesystem.IOIdleFor(500 * time.Millisecond) {
		t.Fatal("filesystem did not report an elapsed idle window")
	}
	endIO := filesystem.beginIO()
	if filesystem.IOIdleFor(0) {
		t.Fatal("filesystem reported idle while I/O was active")
	}
	endIO()
	if filesystem.IOIdleFor(time.Second) {
		t.Fatal("filesystem reported idle immediately after I/O")
	}
	if !filesystem.IOIdleFor(0) {
		t.Fatal("filesystem did not report idle with a zero window")
	}
}

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

func TestFilesystemStaleTailOffsetAppendsCompleteJSONLRecord(t *testing.T) {
	source := []byte("{\"record\":0}\n")
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
	first := []byte("{\"record\":1}\n")
	second := []byte("{\"record\":2}\n")
	staleEOF := int64(len(source))
	if n, errno := filesystem.Write(handle, first, staleEOF); errno != 0 || n != len(first) {
		t.Fatalf("first append = %d errno=%v", n, errno)
	}
	if n, errno := filesystem.Write(handle, second, staleEOF); errno != 0 || n != len(second) {
		t.Fatalf("stale-offset append = %d errno=%v", n, errno)
	}
	if state := session.State(); state.BackingPath != "" {
		t.Fatalf("stale JSONL tail offset created copy-on-write backing %q", state.BackingPath)
	}
	want := append(append(append([]byte(nil), source...), first...), second...)
	current := make([]byte, len(want))
	if n, errno := filesystem.Read(handle, current, 0); errno != 0 || n != len(want) {
		t.Fatalf("Read after stale-offset append = %d errno=%v", n, errno)
	}
	if !bytes.Equal(current, want) {
		t.Fatalf("visible bytes differ: got=%q want=%q", current, want)
	}
}

func TestFilesystemStaleTailOffsetWithArbitraryBytesUsesCopyOnWrite(t *testing.T) {
	source := []byte("{\"record\":0}\n")
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
	first := []byte("{\"record\":1}\n")
	staleEOF := int64(len(source))
	if n, errno := filesystem.Write(handle, first, staleEOF); errno != 0 || n != len(first) {
		t.Fatalf("first append = %d errno=%v", n, errno)
	}
	if n, errno := filesystem.Write(handle, []byte("PATCH"), staleEOF); errno != 0 || n != len("PATCH") {
		t.Fatalf("random write = %d errno=%v", n, errno)
	}
	if state := session.State(); state.BackingPath == "" {
		t.Fatal("arbitrary stale-offset write did not create copy-on-write backing")
	}
}

func TestNativePassthroughPreservesOutOfOrderAppendChunksByOffset(t *testing.T) {
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	route := "/sessions/2026/07/16/rollout-repro.jsonl"
	nativePath := nativePathFromRoot(nativeRoot, route)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	base := []byte("{\"record\":0}\n")
	if err := os.WriteFile(nativePath, base, 0o600); err != nil {
		t.Fatal(err)
	}

	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	large := append([]byte("{\"large\":\""), bytes.Repeat([]byte("x"), 40*1024)...)
	large = append(large, []byte("\"}\n")...)
	small := []byte("{\"record\":2}\n")
	split := 32 * 1024

	largeHandle, errno := filesystem.Open(route, os.O_WRONLY|os.O_APPEND)
	if errno != 0 {
		t.Fatalf("open large writer: %v", errno)
	}
	defer filesystem.Release(largeHandle)
	smallHandle, errno := filesystem.Open(route, os.O_WRONLY|os.O_APPEND)
	if errno != 0 {
		t.Fatalf("open small writer: %v", errno)
	}
	defer filesystem.Release(smallHandle)

	baseOffset := int64(len(base))
	if n, errno := filesystem.Write(largeHandle, large[:split], baseOffset); errno != 0 || n != split {
		t.Fatalf("write large prefix: n=%d errno=%v", n, errno)
	}
	if n, errno := filesystem.Write(smallHandle, small, baseOffset+int64(len(large))); errno != 0 || n != len(small) {
		t.Fatalf("write later record: n=%d errno=%v", n, errno)
	}
	if n, errno := filesystem.Write(largeHandle, large[split:], baseOffset+int64(split)); errno != 0 || n != len(large)-split {
		t.Fatalf("write large suffix: n=%d errno=%v", n, errno)
	}
	if errno := filesystem.Fsync(largeHandle); errno != 0 {
		t.Fatalf("commit out-of-order chunks: %v", errno)
	}

	got, err := os.ReadFile(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append(append([]byte(nil), base...), large...), small...)
	if !bytes.Equal(got, want) {
		t.Fatalf("native append chunks interleaved: got=%d bytes want=%d bytes", len(got), len(want))
	}
}

func TestNativePassthroughRetryAtOverlappingOffsetIsIdempotent(t *testing.T) {
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	route := "/sessions/2026/07/16/rollout-retry.jsonl"
	nativePath := nativePathFromRoot(nativeRoot, route)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	base := []byte("{\"record\":0}\n")
	if err := os.WriteFile(nativePath, base, 0o600); err != nil {
		t.Fatal(err)
	}

	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	handle, errno := filesystem.Open(route, os.O_WRONLY|os.O_APPEND)
	if errno != 0 {
		t.Fatalf("open writer: %v", errno)
	}
	defer filesystem.Release(handle)
	record := []byte("{\"payload\":{\"internal_chat_message_metadata_passthrough\":{\"turn_id\":\"turn\"}}}\n")
	retry := record[len(record)-71:]
	baseOffset := int64(len(base))
	if n, errno := filesystem.Write(handle, record, baseOffset); errno != 0 || n != len(record) {
		t.Fatalf("write record: n=%d errno=%v", n, errno)
	}
	if n, errno := filesystem.Write(handle, retry, baseOffset+int64(len(record)-len(retry))); errno != 0 || n != len(retry) {
		t.Fatalf("retry suffix: n=%d errno=%v", n, errno)
	}
	if errno := filesystem.Fsync(handle); errno != 0 {
		t.Fatalf("commit overlapping retry: %v", errno)
	}

	got, err := os.ReadFile(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), base...), record...)
	if !bytes.Equal(got, want) {
		t.Fatalf("overlapping retry was appended: got=%q want=%q", got, want)
	}
}

func TestNativePassthroughStagesAppendUntilFsync(t *testing.T) {
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	route := "/sessions/2026/07/16/rollout-staged.jsonl"
	nativePath := nativePathFromRoot(nativeRoot, route)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	base := []byte("{\"record\":0}\n")
	if err := os.WriteFile(nativePath, base, 0o600); err != nil {
		t.Fatal(err)
	}

	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	writer, errno := filesystem.Open(route, os.O_WRONLY|os.O_APPEND)
	if errno != 0 {
		t.Fatalf("open writer: %v", errno)
	}
	defer filesystem.Release(writer)
	record := []byte("{\"record\":1}\n")
	if n, errno := filesystem.Write(writer, record, int64(len(base))); errno != 0 || n != len(record) {
		t.Fatalf("stage append: n=%d errno=%v", n, errno)
	}

	backing, err := os.ReadFile(nativePath)
	if err != nil || !bytes.Equal(backing, base) {
		t.Fatalf("uncommitted bytes reached backing: got=%q err=%v", backing, err)
	}
	attribute, errno := filesystem.Getattr(route)
	if errno != 0 || attribute.Size != int64(len(base)+len(record)) {
		t.Fatalf("visible staged size=%d errno=%v", attribute.Size, errno)
	}
	reader, errno := filesystem.Open(route, os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open staged reader: %v", errno)
	}
	defer filesystem.Release(reader)
	visible := make([]byte, len(base)+len(record))
	if n, errno := filesystem.Read(reader, visible, 0); errno != 0 || n != len(visible) {
		t.Fatalf("read staged bytes: n=%d errno=%v", n, errno)
	}
	want := append(append([]byte(nil), base...), record...)
	if !bytes.Equal(visible, want) {
		t.Fatalf("staged visible bytes=%q want=%q", visible, want)
	}

	if errno := filesystem.Fsync(writer); errno != 0 {
		t.Fatalf("commit staged append: %v", errno)
	}
	backing, err = os.ReadFile(nativePath)
	if err != nil || !bytes.Equal(backing, want) {
		t.Fatalf("committed backing=%q err=%v", backing, err)
	}
}

func TestNativePassthroughInvalidAppendFailsClosed(t *testing.T) {
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native")
	route := "/sessions/2026/07/16/rollout-invalid.jsonl"
	nativePath := nativePathFromRoot(nativeRoot, route)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	base := []byte("{\"record\":0}\n")
	if err := os.WriteFile(nativePath, base, 0o600); err != nil {
		t.Fatal(err)
	}

	filesystem := NewCanonical()
	filesystem.SetNativeRoot(nativeRoot)
	writer, errno := filesystem.Open(route, os.O_WRONLY|os.O_APPEND)
	if errno != 0 {
		t.Fatalf("open writer: %v", errno)
	}
	defer filesystem.Release(writer)
	if n, errno := filesystem.Write(writer, []byte("not-json\n"), int64(len(base))); errno != 0 || n != len("not-json\n") {
		t.Fatalf("stage invalid append: n=%d errno=%v", n, errno)
	}
	if errno := filesystem.Fsync(writer); errno != syscall.EIO {
		t.Fatalf("invalid append fsync errno=%v, want EIO", errno)
	}
	backing, err := os.ReadFile(nativePath)
	if err != nil || !bytes.Equal(backing, base) {
		t.Fatalf("invalid append changed backing: got=%q err=%v", backing, err)
	}
	attribute, errno := filesystem.Getattr(route)
	if errno != 0 || attribute.Size != int64(len(base)) {
		t.Fatalf("invalid append remained visible: size=%d errno=%v", attribute.Size, errno)
	}

	record := []byte("{\"record\":1}\n")
	if n, errno := filesystem.Write(writer, record, int64(len(base))); errno != 0 || n != len(record) {
		t.Fatalf("retry valid append: n=%d errno=%v", n, errno)
	}
	if errno := filesystem.Fsync(writer); errno != 0 {
		t.Fatalf("commit valid retry: %v", errno)
	}
	want := append(append([]byte(nil), base...), record...)
	backing, err = os.ReadFile(nativePath)
	if err != nil || !bytes.Equal(backing, want) {
		t.Fatalf("valid retry backing=%q err=%v", backing, err)
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

func TestCanonicalFilesystemMovesManagedSessionBetweenArchiveAndActivePaths(t *testing.T) {
	source := []byte("canonical-session\n")
	session := mountSessionFixture(t, "session", source)
	filesystem := NewCanonical()
	filename := "rollout-2026-07-12T14-28-28-session.jsonl"
	archivedPath := "/archived_sessions/" + filename
	if err := filesystem.AddSessionAt("session", archivedPath, session); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"/sessions/2026", "/sessions/2026/07", "/sessions/2026/07/12"} {
		if errno := filesystem.Mkdir(directory, 0o700); errno != 0 {
			t.Fatalf("Mkdir %s errno=%v", directory, errno)
		}
	}
	activePath := "/sessions/2026/07/12/" + filename
	if errno := filesystem.Rename(archivedPath, activePath); errno != 0 {
		t.Fatalf("Rename errno=%v", errno)
	}
	if _, errno := filesystem.Getattr(archivedPath); errno != syscall.ENOENT {
		t.Fatalf("archived path errno=%v, want ENOENT", errno)
	}
	attribute, errno := filesystem.Getattr(activePath)
	if errno != 0 || attribute.Mode&syscall.S_IFREG == 0 || attribute.Size != int64(len(source)) {
		t.Fatalf("active Getattr = %#v errno=%v", attribute, errno)
	}
	entries, errno := filesystem.ReadDir("/sessions/2026/07/12")
	if errno != 0 || len(entries) != 1 || entries[0] != filename {
		t.Fatalf("active ReadDir = %#v errno=%v", entries, errno)
	}
	handle, errno := filesystem.Open(activePath, os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open active errno=%v", errno)
	}
	defer filesystem.Release(handle)
	got := make([]byte, len(source))
	if n, errno := filesystem.Read(handle, got, 0); errno != 0 || n != len(source) || !bytes.Equal(got, source) {
		t.Fatalf("Read active = %d errno=%v bytes=%q", n, errno, got)
	}
}

func TestCanonicalFilesystemManagedSessionMasksRetainedSnapshotAtCurrentRoute(t *testing.T) {
	root := t.TempDir()
	filename := "rollout-2026-07-12T14-28-28-session.jsonl"
	archivedPath := "/archived_sessions/" + filename
	nativePath := filepath.Join(root, "archived_sessions", filename)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	base := []byte("native-base\n")
	if err := os.WriteFile(nativePath, base, 0o600); err != nil {
		t.Fatal(err)
	}
	session := mountSessionWithNativeSnapshot(t, "session", base, nativePath)
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	tail := []byte("managed-tail\n")
	if _, err := writer.Append(context.Background(), tail); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	filesystem := NewCanonical()
	filesystem.SetNativeRoot(root)
	if err := filesystem.AddSessionAt("session", archivedPath, session); err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), base...), tail...)
	attribute, errno := filesystem.Getattr(archivedPath)
	if errno != 0 || attribute.Size != int64(len(want)) {
		t.Fatalf("managed Getattr = %#v errno=%v, want size %d", attribute, errno, len(want))
	}
	handle, errno := filesystem.Open(archivedPath, os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("managed Open errno=%v", errno)
	}
	defer filesystem.Release(handle)
	got := make([]byte, len(want))
	if n, errno := filesystem.Read(handle, got, 0); errno != 0 || n != len(want) || !bytes.Equal(got, want) {
		t.Fatalf("managed Read = %d errno=%v bytes=%q want=%q", n, errno, got, want)
	}
}

func TestCanonicalFilesystemNativePreferenceFallsBackToManagedWithoutPathLoss(t *testing.T) {
	root := t.TempDir()
	route := "/sessions/2026/07/14/rollout-retirement.jsonl"
	nativePath := nativePathFromRoot(root, route)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	managedBytes := []byte("managed-current\n")
	nativeBytes := []byte("native-current\n")
	if err := os.WriteFile(nativePath, nativeBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	filesystem := NewCanonical()
	filesystem.SetNativeRoot(root)
	if err := filesystem.AddSessionAt("session", route, mountSessionFixture(t, "session", managedBytes)); err != nil {
		t.Fatal(err)
	}
	read := func() []byte {
		t.Helper()
		handle, errno := filesystem.Open(route, os.O_RDONLY)
		if errno != 0 {
			t.Fatalf("Open errno=%v", errno)
		}
		defer filesystem.Release(handle)
		attribute, errno := filesystem.Getattr(route)
		if errno != 0 {
			t.Fatalf("Getattr errno=%v", errno)
		}
		data := make([]byte, attribute.Size)
		n, errno := filesystem.Read(handle, data, 0)
		if errno != 0 || n != len(data) {
			t.Fatalf("Read n=%d errno=%v size=%d", n, errno, len(data))
		}
		return data
	}

	if got := read(); !bytes.Equal(got, managedBytes) {
		t.Fatalf("initial bytes = %q, want managed %q", got, managedBytes)
	}
	if err := filesystem.PreferNativeSession("session"); err != nil {
		t.Fatal(err)
	}
	if got := read(); !bytes.Equal(got, nativeBytes) {
		t.Fatalf("preferred bytes = %q, want native %q", got, nativeBytes)
	}
	if err := os.Remove(nativePath); err != nil {
		t.Fatal(err)
	}
	if got := read(); !bytes.Equal(got, managedBytes) {
		t.Fatalf("fallback bytes = %q, want managed %q", got, managedBytes)
	}
	if err := os.WriteFile(nativePath, nativeBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := read(); !bytes.Equal(got, nativeBytes) {
		t.Fatalf("restored native bytes = %q, want %q", got, nativeBytes)
	}
}

func TestCanonicalFilesystemHidesRetainedSnapshotAfterManagedRouteMoves(t *testing.T) {
	root := t.TempDir()
	filename := "rollout-2026-07-12T14-28-28-session.jsonl"
	archivedPath := "/archived_sessions/" + filename
	activePath := "/sessions/2026/07/12/" + filename
	nativePath := filepath.Join(root, "archived_sessions", filename)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	base := []byte("retained-base\n")
	if err := os.WriteFile(nativePath, base, 0o600); err != nil {
		t.Fatal(err)
	}
	session := mountSessionWithNativeSnapshot(t, "session", base, nativePath)
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(root)
	if err := filesystem.AddSessionAt("session", archivedPath, session); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"/sessions/2026", "/sessions/2026/07", "/sessions/2026/07/12"} {
		if errno := filesystem.Mkdir(directory, 0o700); errno != 0 {
			t.Fatalf("Mkdir %s errno=%v", directory, errno)
		}
	}
	if errno := filesystem.Rename(archivedPath, activePath); errno != 0 {
		t.Fatalf("Rename errno=%v", errno)
	}
	if _, errno := filesystem.Getattr(archivedPath); errno != syscall.ENOENT {
		t.Fatalf("retained archived Getattr errno=%v, want ENOENT", errno)
	}
	if _, errno := filesystem.Open(archivedPath, os.O_RDONLY); errno != syscall.ENOENT {
		t.Fatalf("retained archived Open errno=%v, want ENOENT", errno)
	}
	entries, errno := filesystem.ReadDir("/archived_sessions")
	if errno != 0 {
		t.Fatalf("archived ReadDir errno=%v", errno)
	}
	for _, entry := range entries {
		if entry == filename {
			t.Fatalf("retained snapshot leaked into archived directory: %#v", entries)
		}
	}
}

func TestCanonicalFilesystemMovesManagedSessionIntoExistingNativeDirectoryAfterRestart(t *testing.T) {
	root := t.TempDir()
	activeDirectory := filepath.Join(root, "sessions", "2026", "07", "12")
	if err := os.MkdirAll(activeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "archived_sessions"), 0o700); err != nil {
		t.Fatal(err)
	}

	session := mountSessionFixture(t, "session", []byte("restart-route\n"))
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(root)
	filename := "rollout-2026-07-12T14-28-28-session.jsonl"
	archivedPath := "/archived_sessions/" + filename
	activePath := "/sessions/2026/07/12/" + filename
	if err := filesystem.AddSessionAt("session", archivedPath, session); err != nil {
		t.Fatal(err)
	}
	if errno := filesystem.Rename(archivedPath, activePath); errno != 0 {
		t.Fatalf("Rename into existing native directory errno=%v", errno)
	}
	if _, errno := filesystem.Getattr(archivedPath); errno != syscall.ENOENT {
		t.Fatalf("archived path errno=%v, want ENOENT", errno)
	}
	if _, errno := filesystem.Getattr(activePath); errno != 0 {
		t.Fatalf("active path errno=%v", errno)
	}
	if len(filesystem.paths) != 1 || filesystem.paths[activePath] != "session" {
		t.Fatalf("managed routes after rename = %#v", filesystem.paths)
	}
	for _, nativePath := range []string{
		nativePathFromRoot(root, archivedPath),
		nativePathFromRoot(root, activePath),
	} {
		if _, err := os.Stat(nativePath); !os.IsNotExist(err) {
			t.Fatalf("managed session was duplicated into native root: path=%s err=%v", nativePath, err)
		}
	}
}

func TestCanonicalFilesystemKeepsEmptyNativeRootDisabled(t *testing.T) {
	filesystem := NewCanonical()
	filesystem.SetNativeRoot("")
	if filesystem.nativeRoot != "" {
		t.Fatalf("empty native root became %q", filesystem.nativeRoot)
	}
	if _, ok := filesystem.nativePath("/sessions/2026/07/12/rollout.jsonl"); ok {
		t.Fatal("empty native root should not resolve a backing path")
	}
}

func TestCanonicalFilesystemHidesNativeFilesOutsideSessionNamespace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "archived_sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "outside.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(root)
	entries, errno := filesystem.ReadDir("/")
	if errno != 0 || len(entries) != 2 || entries[0] != "archived_sessions" || entries[1] != "sessions" {
		t.Fatalf("canonical root entries = %#v errno=%v", entries, errno)
	}
	if _, errno := filesystem.Getattr("/outside.txt"); errno != syscall.ENOENT {
		t.Fatalf("outside Getattr errno=%v, want ENOENT", errno)
	}
	if _, errno := filesystem.Open("/outside.txt", os.O_RDONLY); errno != syscall.ENOENT {
		t.Fatalf("outside Open errno=%v, want ENOENT", errno)
	}
	if errno := filesystem.Mkdir("/outside", 0o700); errno != syscall.EPERM {
		t.Fatalf("outside Mkdir errno=%v, want EPERM", errno)
	}
}

func TestCanonicalFilesystemMovesAppleDoubleSidecarWithManagedRoute(t *testing.T) {
	root := t.TempDir()
	oldDirectory := filepath.Join(root, "archived_sessions")
	newDirectory := filepath.Join(root, "sessions", "2026", "07", "12")
	if err := os.MkdirAll(oldDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	filename := "rollout-session.jsonl"
	oldPath := "/archived_sessions/" + filename
	newPath := "/sessions/2026/07/12/" + filename
	oldSidecar := filepath.Join(oldDirectory, "._"+filename)
	if err := os.WriteFile(oldSidecar, []byte("appledouble-metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(root)
	if err := filesystem.AddSessionAt("session", oldPath, mountSessionFixture(t, "session", []byte("session\n"))); err != nil {
		t.Fatal(err)
	}
	if errno := filesystem.MoveSessionAt("session", newPath); errno != nil {
		t.Fatalf("MoveSessionAt errno=%v", errno)
	}
	if _, err := os.Stat(oldSidecar); !os.IsNotExist(err) {
		t.Fatalf("old AppleDouble sidecar remained: %v", err)
	}
	newSidecar := filepath.Join(newDirectory, "._"+filename)
	if got, err := os.ReadFile(newSidecar); err != nil || string(got) != "appledouble-metadata" {
		t.Fatalf("new AppleDouble sidecar = %q err=%v", got, err)
	}
	entries, errno := filesystem.ReadDir(filepath.Dir(newPath))
	if errno != 0 || len(entries) != 2 || entries[0] != "._"+filename || entries[1] != filename {
		t.Fatalf("session directory entries = %#v errno=%v", entries, errno)
	}
}

func TestCanonicalFilesystemUpsertMovesExistingSessionRoute(t *testing.T) {
	session := mountSessionFixture(t, "session", []byte("route-update\n"))
	filesystem := NewCanonical()
	oldPath := "/archived_sessions/rollout-session.jsonl"
	newPath := "/sessions/2026/07/12/rollout-session.jsonl"
	if err := filesystem.AddSessionAt("session", oldPath, session); err != nil {
		t.Fatal(err)
	}
	if err := filesystem.UpsertSessionAt("session", newPath, session); err != nil {
		t.Fatal(err)
	}
	if _, errno := filesystem.Getattr(oldPath); errno != syscall.ENOENT {
		t.Fatalf("old route errno=%v, want ENOENT", errno)
	}
	if _, errno := filesystem.Getattr(newPath); errno != 0 {
		t.Fatalf("new route errno=%v", errno)
	}
}

func TestCanonicalFilesystemRemoveSessionRevealsNativeFile(t *testing.T) {
	root := t.TempDir()
	route := "/archived_sessions/rollout-session.jsonl"
	nativePath := nativePathFromRoot(root, route)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativePath, []byte("native\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(root)
	if err := filesystem.AddSessionAt("session", route, mountSessionFixture(t, "managed", []byte("managed\n"))); err != nil {
		t.Fatal(err)
	}
	if err := filesystem.RemoveSession("session"); err != nil {
		t.Fatal(err)
	}
	handle, errno := filesystem.Open(route, os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open native after removal errno=%v", errno)
	}
	defer filesystem.Release(handle)
	got := make([]byte, len("native\n"))
	if n, errno := filesystem.Read(handle, got, 0); errno != 0 || n != len(got) || string(got) != "native\n" {
		t.Fatalf("native after removal = %q n=%d errno=%v", got, n, errno)
	}
}

func TestCanonicalFilesystemPassesThroughNativeSessionFiles(t *testing.T) {
	root := t.TempDir()
	nativeDirectory := filepath.Join(root, "sessions", "2026", "07", "12")
	if err := os.MkdirAll(nativeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "archived_sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	nativePath := filepath.Join(nativeDirectory, "native.jsonl")
	source := []byte("native-session\n")
	if err := os.WriteFile(nativePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(root)
	entries, errno := filesystem.ReadDir("/sessions/2026/07/12")
	if errno != 0 || len(entries) != 1 || entries[0] != "native.jsonl" {
		t.Fatalf("native ReadDir = %#v errno=%v", entries, errno)
	}
	handle, errno := filesystem.Open("/sessions/2026/07/12/native.jsonl", os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("native Open errno=%v", errno)
	}
	got := make([]byte, len(source))
	if n, errno := filesystem.Read(handle, got, 0); errno != 0 || n != len(source) || !bytes.Equal(got, source) {
		t.Fatalf("native Read = %d errno=%v bytes=%q", n, errno, got)
	}
	if errno := filesystem.Release(handle); errno != 0 {
		t.Fatalf("native Release errno=%v", errno)
	}

	createdPath := "/sessions/2026/07/12/created.jsonl"
	created, errno := filesystem.Open(createdPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if errno != 0 {
		t.Fatalf("native create Open errno=%v", errno)
	}
	createdBytes := []byte("{\"created\":\"session\"}\n")
	if n, errno := filesystem.Write(created, createdBytes, 0); errno != 0 || n != len(createdBytes) {
		t.Fatalf("native create Write = %d errno=%v", n, errno)
	}
	if errno := filesystem.Release(created); errno != 0 {
		t.Fatalf("native create Release errno=%v", errno)
	}
	if got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(createdPath, "/")))); err != nil || !bytes.Equal(got, createdBytes) {
		t.Fatalf("native created bytes = %q err=%v", got, err)
	}

	renamedPath := "/archived_sessions/created.jsonl"
	if errno := filesystem.Rename(createdPath, renamedPath); errno != 0 {
		t.Fatalf("native Rename errno=%v", errno)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(createdPath, "/")))); !os.IsNotExist(err) {
		t.Fatalf("native source remained after rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(renamedPath, "/")))); err != nil {
		t.Fatalf("native destination missing after rename: %v", err)
	}
	if errno := filesystem.Unlink(renamedPath); errno != 0 {
		t.Fatalf("native Unlink errno=%v", errno)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(renamedPath, "/")))); !os.IsNotExist(err) {
		t.Fatalf("native destination remained after unlink: %v", err)
	}
}

func TestCanonicalFilesystemNativeStreamReadFallsBackForVirtualAndPendingFiles(t *testing.T) {
	root := t.TempDir()
	pathName := "/sessions/2026/07/12/stream.jsonl"
	nativePath := nativePathFromRoot(root, pathName)
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativePath, []byte("native-content\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	filesystem := NewCanonical()
	filesystem.SetNativeRoot(root)
	nativeHandle, errno := filesystem.Open(pathName, os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open native handle errno=%v", errno)
	}
	defer filesystem.Release(nativeHandle)
	var streamed []byte
	handled, n, err := filesystem.StreamNativeRead(nativeHandle, 7, 64, func(file *os.File, offset int64, length int) (int, error) {
		streamed = make([]byte, length)
		read, err := file.ReadAt(streamed, offset)
		streamed = streamed[:read]
		return read, err
	})
	if !handled || err != nil || n != len(streamed) || string(streamed) != "content\n" {
		t.Fatalf("native stream handled=%t n=%d err=%v bytes=%q", handled, n, err, streamed)
	}

	writer, errno := filesystem.Open(pathName, os.O_WRONLY|os.O_APPEND)
	if errno != 0 {
		t.Fatalf("open append handle errno=%v", errno)
	}
	pending := []byte("{\"pending\":true}\n")
	if written, errno := filesystem.Write(writer, pending, int64(len("native-content\n"))); errno != 0 || written != len(pending) {
		t.Fatalf("stage append written=%d errno=%v", written, errno)
	}
	handled, _, err = filesystem.StreamNativeRead(nativeHandle, 0, 64, func(_ *os.File, _ int64, _ int) (int, error) {
		t.Fatal("pending append unexpectedly used native stream")
		return 0, nil
	})
	if handled || err != nil {
		t.Fatalf("pending append stream handled=%t err=%v, want fallback", handled, err)
	}
	var pendingStream []byte
	pendingTotals := make(map[int]struct{})
	n, err = filesystem.StreamBufferedRead(nativeHandle, 0, 64, 4, func(total int, chunk []byte) error {
		pendingTotals[total] = struct{}{}
		pendingStream = append(pendingStream, chunk...)
		return nil
	})
	wantPending := append([]byte("native-content\n"), pending...)
	if err != nil || n != len(wantPending) || !bytes.Equal(pendingStream, wantPending) || len(pendingTotals) != 1 {
		t.Fatalf("pending buffered stream n=%d err=%v totals=%v bytes=%q", n, err, pendingTotals, pendingStream)
	}
	if errno := filesystem.Release(writer); errno != 0 {
		t.Fatalf("release append handle errno=%v", errno)
	}

	virtualPath := "/sessions/2026/07/12/virtual.jsonl"
	if err := filesystem.AddSessionAt("virtual", virtualPath, mountSessionFixture(t, "virtual", []byte("virtual\n"))); err != nil {
		t.Fatal(err)
	}
	virtualHandle, errno := filesystem.Open(virtualPath, os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open virtual handle errno=%v", errno)
	}
	defer filesystem.Release(virtualHandle)
	handled, _, err = filesystem.StreamNativeRead(virtualHandle, 0, 64, func(_ *os.File, _ int64, _ int) (int, error) {
		t.Fatal("virtual session unexpectedly used native stream")
		return 0, nil
	})
	if handled || err != nil {
		t.Fatalf("virtual stream handled=%t err=%v, want fallback", handled, err)
	}
	var virtualStream []byte
	var virtualTotals []int
	n, err = filesystem.StreamBufferedRead(virtualHandle, 1, 64, 3, func(total int, chunk []byte) error {
		virtualTotals = append(virtualTotals, total)
		virtualStream = append(virtualStream, chunk...)
		return nil
	})
	if err != nil || n != len("irtual\n") || string(virtualStream) != "irtual\n" || !slices.Equal(virtualTotals, []int{7, 7, 7}) {
		t.Fatalf("virtual buffered stream n=%d err=%v totals=%v bytes=%q", n, err, virtualTotals, virtualStream)
	}
}

func TestCanonicalFilesystemSupportsFSKitOpenUnlinkStaging(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "sessions", "2026", "07", "12")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "archived_sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(root)

	original := "/sessions/2026/07/12/open.jsonl"
	hidden := "/sessions/2026/07/12/.nfs.20051026.83fd"
	base := []byte("{\"record\":0}\n")
	if err := os.WriteFile(filepath.Join(directory, "open.jsonl"), base, 0o600); err != nil {
		t.Fatal(err)
	}
	handle, errno := filesystem.Open(original, os.O_RDWR)
	if errno != 0 {
		t.Fatalf("open errno=%v", errno)
	}
	if errno := filesystem.Rename(original, hidden); errno != 0 {
		t.Fatalf("open-unlink rename errno=%v", errno)
	}
	if got, errno := filesystem.HandlePath(handle); errno != 0 || got != hidden {
		t.Fatalf("handle path = %q errno=%v, want %q", got, errno, hidden)
	}
	appended := []byte("{\"record\":1}\n")
	if n, errno := filesystem.Write(handle, appended, int64(len(base))); errno != 0 || n != len(appended) {
		t.Fatalf("write after hidden rename = %d errno=%v", n, errno)
	}
	if errno := filesystem.Release(handle); errno != 0 {
		t.Fatalf("release errno=%v", errno)
	}
	hiddenNative := filepath.Join(directory, filepath.Base(hidden))
	want := append(append([]byte(nil), base...), appended...)
	if got, err := os.ReadFile(hiddenNative); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("hidden bytes = %q err=%v, want %q", got, err, want)
	}
	if errno := filesystem.Unlink(hidden); errno != 0 {
		t.Fatalf("unlink hidden errno=%v", errno)
	}
	if _, err := os.Stat(hiddenNative); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hidden file remained: %v", err)
	}

	second := "/sessions/2026/07/12/second.jsonl"
	if err := os.WriteFile(filepath.Join(directory, "second.jsonl"), base, 0o600); err != nil {
		t.Fatal(err)
	}
	if errno := filesystem.Rename(second, "/sessions/2026/07/12/.not-nfs"); errno != syscall.EPERM {
		t.Fatalf("non-FSKit hidden rename errno=%v, want EPERM", errno)
	}
	if errno := filesystem.Rename(second, "/archived_sessions/.nfs.20051026.83fd"); errno != syscall.EPERM {
		t.Fatalf("cross-directory open-unlink rename errno=%v, want EPERM", errno)
	}
}

func TestCanonicalFilesystemRenamesOpenNativeReader(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "sessions", "2026", "07", "12")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "archived_sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(root)

	original := "/sessions/2026/07/12/open-reader.jsonl"
	renamed := "/sessions/2026/07/12/renamed-reader.jsonl"
	want := []byte("{\"record\":0}\n")
	if err := os.WriteFile(filepath.Join(directory, "open-reader.jsonl"), want, 0o600); err != nil {
		t.Fatal(err)
	}
	handle, errno := filesystem.Open(original, os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open errno=%v", errno)
	}
	defer filesystem.Release(handle)

	if errno := filesystem.Rename(original, renamed); errno != 0 {
		t.Fatalf("rename open reader errno=%v", errno)
	}
	if got, errno := filesystem.HandlePath(handle); errno != 0 || got != renamed {
		t.Fatalf("handle path = %q errno=%v, want %q", got, errno, renamed)
	}
	buffer := make([]byte, len(want))
	if n, errno := filesystem.Read(handle, buffer, 0); errno != 0 || n != len(want) || !bytes.Equal(buffer, want) {
		t.Fatalf("read after rename = %q n=%d errno=%v, want %q", buffer, n, errno, want)
	}
	if _, err := os.Stat(filepath.Join(directory, "open-reader.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source remained after rename: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(directory, "renamed-reader.jsonl")); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("renamed bytes = %q err=%v, want %q", got, err, want)
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

func TestFilesystemOwnedGenerationClosesAfterItsLastHandle(t *testing.T) {
	first := mountSessionFixture(t, "first-session", []byte("first"))
	second := mountSessionFixture(t, "second-session", []byte("second"))
	firstCloser := &countingCloser{}
	secondCloser := &countingCloser{}
	filesystem := New()
	if err := filesystem.AddSessionOwned("session", first, firstCloser); err != nil {
		t.Fatal(err)
	}
	oldHandle, errno := filesystem.Open("/session.jsonl", os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open old handle: %v", errno)
	}
	if err := filesystem.UpsertSessionOwned("session", second, secondCloser); err != nil {
		t.Fatalf("upsert owned generation: %v", err)
	}
	if firstCloser.calls != 0 {
		t.Fatal("old generation closed while its read handle remained open")
	}
	newHandle, errno := filesystem.Open("/session.jsonl", os.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open new handle: %v", errno)
	}
	oldBytes := make([]byte, 5)
	if n, errno := filesystem.Read(oldHandle, oldBytes, 0); errno != 0 || n != len(oldBytes) || string(oldBytes) != "first" {
		t.Fatalf("old generation read: n=%d errno=%v bytes=%q", n, errno, oldBytes)
	}
	newBytes := make([]byte, 6)
	if n, errno := filesystem.Read(newHandle, newBytes, 0); errno != 0 || n != len(newBytes) || string(newBytes) != "second" {
		t.Fatalf("new generation read: n=%d errno=%v bytes=%q", n, errno, newBytes)
	}
	if err := filesystem.RemoveSession("session"); err != nil {
		t.Fatalf("remove current generation: %v", err)
	}
	if secondCloser.calls != 0 {
		t.Fatal("current generation closed while its read handle remained open")
	}
	if errno := filesystem.Release(oldHandle); errno != 0 {
		t.Fatalf("release old handle: %v", errno)
	}
	if firstCloser.calls != 1 {
		t.Fatalf("old generation close calls=%d, want 1", firstCloser.calls)
	}
	if errno := filesystem.Release(newHandle); errno != 0 {
		t.Fatalf("release new handle: %v", errno)
	}
	if secondCloser.calls != 1 {
		t.Fatalf("new generation close calls=%d, want 1", secondCloser.calls)
	}
}

func TestFilesystemOwnedGenerationClosesImmediatelyWithoutHandles(t *testing.T) {
	firstCloser := &countingCloser{}
	secondCloser := &countingCloser{}
	filesystem := New()
	if err := filesystem.AddSessionOwned("session", mountSessionFixture(t, "first-session", []byte("first")), firstCloser); err != nil {
		t.Fatal(err)
	}
	if err := filesystem.UpsertSessionOwned("session", mountSessionFixture(t, "second-session", []byte("second")), secondCloser); err != nil {
		t.Fatal(err)
	}
	if firstCloser.calls != 1 {
		t.Fatalf("replaced generation close calls=%d, want 1", firstCloser.calls)
	}
	if err := filesystem.CloseSessions(); err != nil {
		t.Fatal(err)
	}
	if secondCloser.calls != 1 {
		t.Fatalf("current generation close calls=%d, want 1", secondCloser.calls)
	}
}

func TestFilesystemRejectedOwnedSessionClosesIncomingOwner(t *testing.T) {
	closer := &countingCloser{}
	filesystem := New()
	err := filesystem.AddSessionOwned("", mountSessionFixture(t, "session", []byte("session")), closer)
	if err == nil {
		t.Fatal("invalid owned session was accepted")
	}
	if closer.calls != 1 {
		t.Fatalf("rejected owner close calls=%d, want 1", closer.calls)
	}
}

type countingCloser struct {
	calls int
}

func (c *countingCloser) Close() error {
	c.calls++
	return nil
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

func TestCanonicalSyntheticDirectoryAttributesStayStableUntilContentsChange(t *testing.T) {
	filesystem := NewCanonical()
	first, errno := filesystem.Getattr("/sessions")
	if errno != 0 {
		t.Fatalf("first Getattr errno=%v", errno)
	}
	second, errno := filesystem.Getattr("/sessions")
	if errno != 0 {
		t.Fatalf("second Getattr errno=%v", errno)
	}
	if first.ObjectID != second.ObjectID || !first.ModTime.Equal(second.ModTime) || !first.ChangeTime.Equal(second.ChangeTime) {
		t.Fatalf("unchanged synthetic directory attributes drifted: first=%#v second=%#v", first, second)
	}

	archived, errno := filesystem.Getattr("/archived_sessions")
	if errno != 0 {
		t.Fatalf("archived Getattr errno=%v", errno)
	}
	filesystem.bumpDirectoryGeneration("/sessions")
	changed, errno := filesystem.Getattr("/sessions")
	if errno != 0 {
		t.Fatalf("changed Getattr errno=%v", errno)
	}
	if changed.ObjectID != first.ObjectID {
		t.Fatalf("directory content change replaced stable object identity %q with %q", first.ObjectID, changed.ObjectID)
	}
	if changed.DirectoryGeneration <= first.DirectoryGeneration || !changed.ModTime.After(first.ModTime) || !changed.ChangeTime.After(first.ChangeTime) {
		t.Fatalf("directory content generation did not advance attributes: first=%#v changed=%#v", first, changed)
	}
	archivedAfter, errno := filesystem.Getattr("/archived_sessions")
	if errno != 0 {
		t.Fatalf("archived Getattr after unrelated change errno=%v", errno)
	}
	if archivedAfter.ObjectID != archived.ObjectID {
		t.Fatalf("unrelated directory identity changed from %q to %q", archived.ObjectID, archivedAfter.ObjectID)
	}
}

func TestCanonicalNativeDirectoryObjectIdentityUsesExplicitContentGeneration(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "sessions", "2026", "07", "22")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "archived_sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	filesystem := NewCanonical()
	filesystem.SetNativeRoot(root)
	first, errno := filesystem.Getattr("/sessions/2026/07/22")
	if errno != 0 {
		t.Fatalf("first Getattr errno=%v", errno)
	}
	if err := os.WriteFile(filepath.Join(directory, "created.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeNotification, errno := filesystem.Getattr("/sessions/2026/07/22")
	if errno != 0 {
		t.Fatalf("pre-notification Getattr errno=%v", errno)
	}
	if beforeNotification.ObjectID != first.ObjectID {
		t.Fatalf("native metadata alone changed object ID from %q to %q", first.ObjectID, beforeNotification.ObjectID)
	}
	filesystem.bumpDirectoryGeneration("/sessions/2026/07/22")
	afterNotification, errno := filesystem.Getattr("/sessions/2026/07/22")
	if errno != 0 {
		t.Fatalf("post-notification Getattr errno=%v", errno)
	}
	if afterNotification.ObjectID != first.ObjectID {
		t.Fatalf("explicit directory content generation replaced object ID %q with %q", first.ObjectID, afterNotification.ObjectID)
	}
	if afterNotification.DirectoryGeneration <= first.DirectoryGeneration {
		t.Fatalf("directory generation did not advance: before=%d after=%d", first.DirectoryGeneration, afterNotification.DirectoryGeneration)
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

func mountSessionWithNativeSnapshot(t *testing.T, sessionID string, source []byte, nativePath string) *vfs.Session {
	t.Helper()
	root := t.TempDir()
	digest := sha256.Sum256(source)
	hexDigest := hex.EncodeToString(digest[:])
	manifest := fold.Manifest{Version: fold.ManifestVersion, Kind: fold.ManifestKind, Session: fold.ManifestSession{ID: sessionID, RolloutPath: nativePath}, Source: fold.ManifestSource{Bytes: int64(len(source)), SHA256: hexDigest}, Parts: []fold.Part{{Kind: fold.PartResidual, Object: fold.ObjectRef{SHA256: hexDigest, RawBytes: int64(len(source))}}}}
	session, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{Root: root, ManifestPath: filepath.Join(root, "manifest.json"), Manifest: manifest, Reader: mountReader{hexDigest: source}, NativeSnapshot: vfs.NativeFile{Path: nativePath, Bytes: int64(len(source)), SHA256: hexDigest}})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

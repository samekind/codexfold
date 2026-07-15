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
	"strings"
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
	createdBytes := []byte("created-session\n")
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

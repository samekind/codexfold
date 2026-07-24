package vfs

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
	"testing"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/storage"
)

func TestSessionAppendPersistsWithoutHydratingBase(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)

	oldReader, err := session.OpenReader()
	if err != nil {
		t.Fatalf("OpenReader before append: %v", err)
	}
	defer oldReader.Close()

	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	if _, err := writer.Append(context.Background(), []byte("-durable-tail")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close writer: %v", err)
	}

	if got := readHandle(t, oldReader); !bytes.Equal(got, source) {
		t.Fatalf("old generation changed after append: %q", got)
	}
	newReader, err := session.OpenReader()
	if err != nil {
		t.Fatalf("OpenReader after append: %v", err)
	}
	want := append(append([]byte(nil), source...), []byte("-durable-tail")...)
	if got := readHandle(t, newReader); !bytes.Equal(got, want) {
		t.Fatalf("new generation bytes differ: got=%q want=%q", got, want)
	}
	_ = newReader.Close()

	state := session.State()
	if state.BackingPath != "" {
		t.Fatalf("append hydrated a backing file: %#v", state)
	}
	if info, err := os.Stat(state.DeltaPath); err != nil || info.Size() != int64(len("-durable-tail")) {
		t.Fatalf("delta state differs: info=%v err=%v", info, err)
	}

	reopened := openFixtureSession(t, root, manifest, reader, nil)
	reopenedReader, err := reopened.OpenReader()
	if err != nil {
		t.Fatalf("reopened OpenReader: %v", err)
	}
	defer reopenedReader.Close()
	if got := readHandle(t, reopenedReader); !bytes.Equal(got, want) {
		t.Fatalf("reopened bytes differ: got=%q want=%q", got, want)
	}
}

func TestSessionAllowsOnlyOneWriterLease(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	first, err := session.OpenWriter()
	if err != nil {
		t.Fatalf("first OpenWriter: %v", err)
	}
	if _, err := session.OpenWriter(); err == nil {
		t.Fatal("second OpenWriter should fail while the lease is held")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first writer: %v", err)
	}
	second, err := session.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter after release: %v", err)
	}
	_ = second.Close()
}

func TestSessionReaderHoldsGenerationLeaseUntilClose(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	handle, err := session.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	leaseDirectory := filepath.Join(root, "fs", "sessions", "session", "leases", "generation-00000000000000000001")
	active, err := storage.DirectoryHasActiveLease(leaseDirectory, false)
	if err != nil || !active {
		t.Fatalf("reader generation lease: active=%t err=%v", active, err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	active, err = storage.DirectoryHasActiveLease(leaseDirectory, true)
	if err != nil || active {
		t.Fatalf("closed reader generation lease: active=%t err=%v", active, err)
	}
}

func TestSessionReaderDoesNotRecreateRetiredStateDirectory(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	stateDirectory := filepath.Dir(session.State().DeltaPath)
	retired := stateDirectory + ".retired"
	if err := os.Rename(stateDirectory, retired); err != nil {
		t.Fatal(err)
	}
	if _, err := session.OpenReader(); err == nil {
		t.Fatal("reader unexpectedly opened after the state directory was retired")
	}
	if _, err := os.Lstat(stateDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired state directory was recreated: %v", err)
	}
}

func TestSessionRandomWriteTransitionsToVerifiedBacking(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	if _, err := writer.Append(context.Background(), []byte("-tail")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := writer.WriteAt(context.Background(), []byte("PATCH"), 3); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := writer.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	_ = writer.Close()

	want := append(append([]byte(nil), source...), []byte("-tail")...)
	copy(want[3:], []byte("PATCH"))
	current, err := session.MaterializeCurrent(context.Background(), filepath.Join(root, "current.jsonl"), false)
	if err != nil {
		t.Fatalf("MaterializeCurrent: %v", err)
	}
	if current.SHA256 != digestBytes(want) || current.Bytes != int64(len(want)) {
		t.Fatalf("materialized metadata differs: %#v", current)
	}
	got, err := os.ReadFile(current.Path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("materialized bytes differ: bytes=%q err=%v", got, err)
	}
	if session.State().BackingPath == "" {
		t.Fatal("random write did not activate a backing file")
	}
}

func TestSessionBudgetRejectsCopyOnWriteBeforeCreatingBacking(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	checker := &vfsRejectingChecker{}
	session, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: filepath.Join(root, "manifest.json"), Manifest: manifest, Reader: reader,
		NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
		Budget:         checker,
	})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	if _, err := writer.WriteAt(context.Background(), []byte("X"), 0); !errors.Is(err, storage.ErrBudgetExceeded) {
		t.Fatalf("WriteAt error = %v, want storage budget rejection", err)
	}
	_ = writer.Close()
	if checker.Calls != 1 || checker.Projection.Operation != "copy-on-write" || checker.Projection.TemporaryBytes != int64(len(source)) {
		t.Fatalf("unexpected COW budget projection: %#v", checker)
	}
	if state := session.State(); state.BackingPath != "" || state.Generation != 1 {
		t.Fatalf("budget rejection changed session state: %#v", state)
	}
	entries, err := os.ReadDir(filepath.Join(root, "fs", "sessions", "session"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "backing") {
			t.Fatalf("backing artifact exists after preflight rejection: %s", entry.Name())
		}
	}
}

func TestSessionBudgetRejectsMaterializeBeforeCreatingTarget(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	checker := &vfsRejectingChecker{}
	session, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: filepath.Join(root, "manifest.json"), Manifest: manifest, Reader: reader,
		NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
		Budget:         checker,
	})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	target := filepath.Join(root, "new", "current.jsonl")
	if _, err := session.MaterializeCurrent(context.Background(), target, false); !errors.Is(err, storage.ErrBudgetExceeded) {
		t.Fatalf("MaterializeCurrent error = %v, want storage budget rejection", err)
	}
	if checker.Calls != 1 || checker.Projection.Operation != "materialize-current" {
		t.Fatalf("unexpected materialize budget projection: %#v", checker)
	}
	if _, err := os.Stat(filepath.Dir(target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("materialize target directory exists after preflight rejection: %v", err)
	}
}

func TestRetireNativeSnapshotKeepsManagedSessionReadableAndRestartable(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	hiddenSnapshot := filepath.Join(root, "fs", "snapshots", "session", "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(hiddenSnapshot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(manifest.Session.RolloutPath, hiddenSnapshot); err != nil {
		t.Fatal(err)
	}
	session, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: filepath.Join(root, "manifest.json"), Manifest: manifest, Reader: reader,
		NativeSnapshot: NativeFile{Path: hiddenSnapshot, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := session.State().NativeSnapshot
	proofFile := filepath.Join(root, "visible-proof.jsonl")
	visible, err := session.MaterializeCurrent(context.Background(), proofFile, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.RetireNativeSnapshot(snapshot, visible); err == nil {
		t.Fatal("native snapshot retirement succeeded without a writer lease")
	}
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	proof, err := session.RetireNativeSnapshot(snapshot, visible)
	if err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if proof.Snapshot != snapshot || proof.Visible.SHA256 != digestBytes(source) || session.State().NativeSnapshot.Path != "" {
		t.Fatalf("native retirement proof/state = %#v / %#v", proof, session.State())
	}
	proofPath := filepath.Join(root, "fs", "sessions", "session", NativeRetirementFilename)
	loaded, err := LoadNativeRetirementProof(proofPath)
	if err != nil || loaded.Snapshot != snapshot || loaded.Visible.SHA256 != digestBytes(source) {
		t.Fatalf("load native retirement proof: proof=%#v err=%v", loaded, err)
	}
	if _, err := os.Stat(snapshot.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired snapshot still exists: %v", err)
	}
	handle, err := session.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	if got := readHandle(t, handle); !bytes.Equal(got, source) {
		t.Fatalf("managed bytes after native retirement = %q", got)
	}
	_ = handle.Close()
	restarted, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: filepath.Join(root, "manifest.json"), Manifest: manifest, Reader: reader,
	})
	if err != nil {
		t.Fatalf("restart without native snapshot: %v", err)
	}
	restartedHandle, err := restarted.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	if got := readHandle(t, restartedHandle); !bytes.Equal(got, source) {
		t.Fatalf("restarted managed bytes = %q", got)
	}
	_ = restartedHandle.Close()
}

func TestExternalNativeRetirementCannotResurrectSnapshotDuringCopyOnWrite(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	hiddenSnapshot := filepath.Join(root, "fs", "snapshots", "session", "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(hiddenSnapshot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(manifest.Session.RolloutPath, hiddenSnapshot); err != nil {
		t.Fatal(err)
	}
	native := NativeFile{Path: hiddenSnapshot, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256}
	serving, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: filepath.Join(root, "manifest.json"), Manifest: manifest, Reader: reader, NativeSnapshot: native,
	})
	if err != nil {
		t.Fatal(err)
	}
	maintenance, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: filepath.Join(root, "manifest.json"), Manifest: manifest, Reader: reader, NativeSnapshot: native,
	})
	if err != nil {
		t.Fatal(err)
	}
	visible, err := maintenance.MaterializeCurrent(context.Background(), filepath.Join(root, "retirement-proof.jsonl"), false)
	if err != nil {
		t.Fatal(err)
	}
	retirementWriter, err := maintenance.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.RetireNativeSnapshot(native, visible); err != nil {
		_ = retirementWriter.Close()
		t.Fatal(err)
	}
	if err := retirementWriter.Close(); err != nil {
		t.Fatal(err)
	}
	retired := maintenance.State()
	if retired.Generation != 2 || retired.NativeSnapshot.Path != "" {
		t.Fatalf("retired state = %#v", retired)
	}
	if serving.State().NativeSnapshot != native {
		t.Fatalf("fixture no longer represents a stale serving session: %#v", serving.State())
	}

	writer, err := serving.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	if serving.State().NativeSnapshot.Path != "" || serving.State().Generation != retired.Generation {
		_ = writer.Close()
		t.Fatalf("writer did not absorb external retirement: serving=%#v retired=%#v", serving.State(), retired)
	}
	if _, err := writer.WriteAt(context.Background(), []byte("X"), 0); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Sync(); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	persisted, err := LoadSessionState(filepath.Join(root, "fs", "sessions", "session", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.NativeSnapshot.Path != "" || persisted.Generation != retired.Generation+1 {
		t.Fatalf("copy-on-write resurrected retired snapshot: %#v", persisted)
	}
	want := append([]byte(nil), source...)
	want[0] = 'X'
	current, err := serving.MaterializeCurrent(context.Background(), filepath.Join(root, "after-cow.jsonl"), false)
	if err != nil || current.Bytes != int64(len(want)) || current.SHA256 != digestBytes(want) {
		t.Fatalf("copy-on-write bytes = %#v err=%v", current, err)
	}
}

func TestExternalNativeRetirementCannotResurrectSnapshotDuringCompact(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	hiddenSnapshot := filepath.Join(root, "fs", "snapshots", "session", "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(hiddenSnapshot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(manifest.Session.RolloutPath, hiddenSnapshot); err != nil {
		t.Fatal(err)
	}
	native := NativeFile{Path: hiddenSnapshot, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256}
	serving, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: filepath.Join(root, "manifest.json"), Manifest: manifest, Reader: reader, NativeSnapshot: native,
	})
	if err != nil {
		t.Fatal(err)
	}
	maintenance, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: filepath.Join(root, "manifest.json"), Manifest: manifest, Reader: reader, NativeSnapshot: native,
	})
	if err != nil {
		t.Fatal(err)
	}
	visible, err := maintenance.MaterializeCurrent(context.Background(), filepath.Join(root, "retirement-proof.jsonl"), false)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := maintenance.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.RetireNativeSnapshot(native, visible); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	retired := maintenance.State()

	result, err := serving.Compact(context.Background(), CompactOptions{Prepare: func(_ context.Context, current NativeFile, generation uint64) (PreparedGeneration, error) {
		if generation != retired.Generation+1 || current.Bytes != manifest.Source.Bytes || current.SHA256 != manifest.Source.SHA256 {
			t.Fatalf("compact preparation received stale state: generation=%d current=%#v retired=%#v", generation, current, retired)
		}
		prepared := manifest
		prepared.Source = fold.ManifestSource{Bytes: current.Bytes, SHA256: current.SHA256}
		view, err := NewView(prepared, reader)
		return PreparedGeneration{ManifestPath: filepath.Join(root, "manifest-compact.json"), Manifest: prepared, View: view}, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Generation != retired.Generation+1 {
		t.Fatalf("compact generation = %d, want %d", result.Generation, retired.Generation+1)
	}
	persisted, err := LoadSessionState(filepath.Join(root, "fs", "sessions", "session", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.NativeSnapshot.Path != "" || persisted.Generation != result.Generation {
		t.Fatalf("compact resurrected retired snapshot: %#v", persisted)
	}
}

func TestNativeSnapshotAlreadyRetiredRequiresExactDurableProof(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	hiddenSnapshot := filepath.Join(root, "fs", "snapshots", "session", "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(hiddenSnapshot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(manifest.Session.RolloutPath, hiddenSnapshot); err != nil {
		t.Fatal(err)
	}
	native := NativeFile{Path: hiddenSnapshot, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256}
	session, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: filepath.Join(root, "manifest.json"), Manifest: manifest, Reader: reader, NativeSnapshot: native,
	})
	if err != nil {
		t.Fatal(err)
	}
	visible, err := session.MaterializeCurrent(context.Background(), filepath.Join(root, "retirement-proof.jsonl"), false)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.RetireNativeSnapshot(native, visible); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	stale := session.State()
	stale.Generation += 3
	stale.NativeSnapshot = native
	retired, err := NativeSnapshotAlreadyRetired(root, stale)
	if err != nil || !retired {
		t.Fatalf("verified retired snapshot = %t, %v", retired, err)
	}
	stale.NativeSnapshot.SHA256 = strings.Repeat("0", 64)
	if retired, err := NativeSnapshotAlreadyRetired(root, stale); err == nil || retired {
		t.Fatalf("mismatched retirement proof accepted: retired=%t err=%v", retired, err)
	}
}

func TestSessionTruncateTransitionsToBacking(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	newSize := int64(len(source) - 4)
	if err := writer.Truncate(context.Background(), newSize); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	_ = writer.Close()
	readerHandle, err := session.OpenReader()
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer readerHandle.Close()
	if got := readHandle(t, readerHandle); !bytes.Equal(got, source[:newSize]) {
		t.Fatalf("truncated bytes differ: got=%q want=%q", got, source[:newSize])
	}
}

func TestSessionEqualLengthTruncateDoesNotCreateBacking(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	if err := writer.Truncate(context.Background(), int64(len(source))); err != nil {
		t.Fatalf("equal-length Truncate: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close writer: %v", err)
	}
	if state := session.State(); state.BackingPath != "" {
		t.Fatalf("equal-length truncate created backing: %#v", state)
	}
	if info, err := os.Stat(session.State().DeltaPath); err != nil || info.Size() != 0 {
		t.Fatalf("equal-length truncate changed delta: info=%#v err=%v", info, err)
	}
}

func TestSessionInterruptedCopyOnWriteKeepsPreviousGeneration(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	stop := errors.New("stop before COW publish")
	session := openFixtureSession(t, root, manifest, reader, func(phase string) error {
		if phase == "before-publish" {
			return stop
		}
		return nil
	})
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	if _, err := writer.WriteAt(context.Background(), []byte("X"), 0); !errors.Is(err, stop) {
		t.Fatalf("WriteAt error = %v, want %v", err, stop)
	}
	_ = writer.Close()
	if session.State().BackingPath != "" {
		t.Fatalf("interrupted COW published backing: %#v", session.State())
	}
	handle, err := session.OpenReader()
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer handle.Close()
	if got := readHandle(t, handle); !bytes.Equal(got, source) {
		t.Fatalf("previous generation changed after interrupted COW: %q", got)
	}
}

func sessionFixture(t *testing.T, root string) (fold.Manifest, memoryReader, []byte) {
	t.Helper()
	parts := [][]byte{[]byte("first-line\n"), bytes.Repeat([]byte("middle"), 11), []byte("\nlast-line\n")}
	reader := memoryReader{}
	manifest := fold.Manifest{Version: fold.ManifestVersion, Kind: fold.ManifestKind, Session: fold.ManifestSession{ID: "session", RolloutPath: filepath.Join(root, "native.jsonl")}}
	var source []byte
	for _, partBytes := range parts {
		digest := digestBytes(partBytes)
		reader[digest] = partBytes
		manifest.Parts = append(manifest.Parts, fold.Part{Kind: fold.PartResidual, Object: fold.ObjectRef{SHA256: digest, RawBytes: int64(len(partBytes))}})
		source = append(source, partBytes...)
	}
	manifest.Source = fold.ManifestSource{Bytes: int64(len(source)), SHA256: digestBytes(source)}
	if err := os.WriteFile(manifest.Session.RolloutPath, source, 0o600); err != nil {
		t.Fatalf("write native snapshot: %v", err)
	}
	return manifest, reader, source
}

func openFixtureSession(t *testing.T, root string, manifest fold.Manifest, reader memoryReader, hook func(string) error) *Session {
	t.Helper()
	session, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: filepath.Join(root, "manifest.json"), Manifest: manifest, Reader: reader,
		NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
		BeforeCOWPhase: hook,
	})
	if err != nil {
		t.Fatalf("OpenSession returned error: %v", err)
	}
	return session
}

func readHandle(t *testing.T, handle *ReadHandle) []byte {
	t.Helper()
	buffer := make([]byte, handle.Size())
	n, err := handle.ReadAt(context.Background(), buffer, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt returned error: %v", err)
	}
	return buffer[:n]
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

type vfsRejectingChecker struct {
	Calls      int
	Projection storage.Projection
}

func (c *vfsRejectingChecker) Check(_ context.Context, projection storage.Projection) (storage.Assessment, error) {
	c.Calls++
	c.Projection = projection
	return storage.Assessment{}, storage.ErrBudgetExceeded
}

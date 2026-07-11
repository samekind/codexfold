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
	"testing"

	"github.com/jstar0/codexfold/internal/fold"
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

package vfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/storage"
)

func TestRecoverFinishesPublishedCopyOnWriteGeneration(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	stop := errors.New("stop after backing publish")
	session := openFixtureSession(t, root, manifest, reader, func(phase string) error {
		if phase == "after-file-publish" {
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
		t.Fatal("interrupted state should not publish backing before recovery")
	}

	reopened := openFixtureSession(t, root, manifest, reader, nil)
	if reopened.State().BackingPath == "" || reopened.State().Generation != 2 {
		t.Fatalf("recovery did not finish COW state: %#v", reopened.State())
	}
	handle, err := reopened.OpenReader()
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer handle.Close()
	if got := readHandle(t, handle); !bytes.Equal(got, source) {
		t.Fatalf("recovered backing differs: got=%q want=%q", got, source)
	}
}

func TestCreateCurrentNativeBackingIncludesVirtualTail(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	if _, err := writer.Append(context.Background(), []byte("-new-tail")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	_ = writer.Close()

	target := filepath.Join(root, "fallback", "session.jsonl")
	backing, err := session.CreateCurrentNativeBacking(context.Background(), target)
	if err != nil {
		t.Fatalf("CreateCurrentNativeBacking: %v", err)
	}
	want := append(append([]byte(nil), source...), []byte("-new-tail")...)
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("fallback differs: got=%q err=%v", got, err)
	}
	if backing.SHA256 != digestBytes(want) || backing.SHA256 == manifest.Source.SHA256 {
		t.Fatalf("fallback digest does not represent current bytes: %#v", backing)
	}
}

func TestCompactSwitchesGenerationAndPreservesPinnedReader(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	writer, _ := session.OpenWriter()
	_, _ = writer.Append(context.Background(), []byte("-tail"))
	_ = writer.Close()
	oldReader, err := session.OpenReader()
	if err != nil {
		t.Fatalf("OpenReader before compact: %v", err)
	}
	defer oldReader.Close()
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(session.State().DeltaPath, oldTime, oldTime); err != nil {
		t.Fatalf("age delta: %v", err)
	}

	want := append(append([]byte(nil), source...), []byte("-tail")...)
	result, err := session.Compact(context.Background(), CompactOptions{
		IdleFor: 10 * time.Minute,
		Prepare: func(_ context.Context, current NativeFile, next uint64) (PreparedGeneration, error) {
			data, err := os.ReadFile(current.Path)
			if err != nil {
				return PreparedGeneration{}, err
			}
			digest := digestBytes(data)
			preparedManifest := fold.Manifest{
				Version: fold.ManifestVersion, Kind: fold.ManifestKind,
				Session: fold.ManifestSession{ID: "session", RolloutPath: current.Path},
				Source:  fold.ManifestSource{Bytes: int64(len(data)), SHA256: digest},
				Parts:   []fold.Part{{Kind: fold.PartResidual, Object: fold.ObjectRef{SHA256: digest, RawBytes: int64(len(data))}}},
			}
			preparedReader := memoryReader{digest: data}
			view, err := NewView(preparedManifest, preparedReader)
			return PreparedGeneration{ManifestPath: filepath.Join(root, "manifest-generation-2.json"), Manifest: preparedManifest, View: view}, err
		},
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if result.Generation != 2 || session.State().BackingPath != "" {
		t.Fatalf("unexpected compact result/state: result=%#v state=%#v", result, session.State())
	}
	newReader, err := session.OpenReader()
	if err != nil {
		t.Fatalf("OpenReader after compact: %v", err)
	}
	defer newReader.Close()
	if got := readHandle(t, newReader); !bytes.Equal(got, want) {
		t.Fatalf("compacted generation differs: got=%q want=%q", got, want)
	}
	if got := readHandle(t, oldReader); !bytes.Equal(got, want) {
		t.Fatalf("pinned old reader changed: got=%q want=%q", got, want)
	}
}

func TestStorageGCKeepsOldSessionGenerationUntilReaderLeaseCloses(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Append(context.Background(), []byte("-tail")); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	oldDelta := session.State().DeltaPath
	oldReader, err := session.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), source...), []byte("-tail")...)
	_, err = session.Compact(context.Background(), CompactOptions{Prepare: func(_ context.Context, current NativeFile, _ uint64) (PreparedGeneration, error) {
		data, err := os.ReadFile(current.Path)
		if err != nil {
			return PreparedGeneration{}, err
		}
		digest := digestBytes(data)
		prepared := fold.Manifest{
			Version: fold.ManifestVersion, Kind: fold.ManifestKind,
			Session: fold.ManifestSession{ID: "session", RolloutPath: current.Path},
			Source:  fold.ManifestSource{Bytes: int64(len(data)), SHA256: digest},
			Parts:   []fold.Part{{Kind: fold.PartResidual, Object: fold.ObjectRef{SHA256: digest, RawBytes: int64(len(data))}}},
		}
		view, err := NewView(prepared, memoryReader{digest: data})
		return PreparedGeneration{ManifestPath: filepath.Join(root, "manifest-generation-2.json"), Manifest: prepared, View: view}, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := readHandle(t, oldReader); !bytes.Equal(got, want) {
		t.Fatalf("old reader changed: %q", got)
	}
	blocked, err := storage.Collect(context.Background(), storage.GCOptions{StoreDir: root, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if blocked.RemovedCount != 0 {
		t.Fatalf("active reader generation was collected: %#v", blocked)
	}
	if _, err := os.Stat(oldDelta); err != nil {
		t.Fatalf("old delta missing while reader lease active: %v", err)
	}
	if err := oldReader.Close(); err != nil {
		t.Fatal(err)
	}
	collected, err := storage.Collect(context.Background(), storage.GCOptions{StoreDir: root, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if collected.RemovedCount != 1 {
		t.Fatalf("closed reader generation was not collected: %#v", collected)
	}
	if _, err := os.Stat(oldDelta); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old delta remains after reader close: %v", err)
	}
}

func TestCompactRejectsDeltaChangedDuringPreparation(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	writer, _ := session.OpenWriter()
	_, _ = writer.Append(context.Background(), []byte("initial"))
	_ = writer.Close()
	oldTime := time.Now().Add(-time.Hour)
	_ = os.Chtimes(session.State().DeltaPath, oldTime, oldTime)

	_, err := session.Compact(context.Background(), CompactOptions{
		IdleFor: 10 * time.Minute,
		Prepare: func(_ context.Context, current NativeFile, _ uint64) (PreparedGeneration, error) {
			file, openErr := os.OpenFile(session.State().DeltaPath, os.O_APPEND|os.O_WRONLY, 0)
			if openErr != nil {
				return PreparedGeneration{}, openErr
			}
			_, _ = file.WriteString("changed")
			_ = file.Close()
			data, _ := os.ReadFile(current.Path)
			digest := digestBytes(data)
			preparedManifest := fold.Manifest{Version: fold.ManifestVersion, Kind: fold.ManifestKind, Session: fold.ManifestSession{ID: "session"}, Source: fold.ManifestSource{Bytes: int64(len(data)), SHA256: digest}, Parts: []fold.Part{{Kind: fold.PartResidual, Object: fold.ObjectRef{SHA256: digest, RawBytes: int64(len(data))}}}}
			view, _ := NewView(preparedManifest, memoryReader{digest: data})
			return PreparedGeneration{ManifestPath: filepath.Join(root, "next.json"), Manifest: preparedManifest, View: view}, nil
		},
	})
	if err == nil {
		t.Fatal("Compact should reject a delta changed during preparation")
	}
	if session.State().Generation != 1 {
		t.Fatalf("failed compact changed generation: %#v", session.State())
	}
}

func TestCompactBudgetRejectsBeforeScratchOrPreparation(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	checker := &vfsRejectingChecker{}
	session, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: filepath.Join(root, "manifest.json"), Manifest: manifest, Reader: reader,
		NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
		Budget:         checker,
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared := false
	if _, err := session.Compact(context.Background(), CompactOptions{Prepare: func(context.Context, NativeFile, uint64) (PreparedGeneration, error) {
		prepared = true
		return PreparedGeneration{}, nil
	}}); !errors.Is(err, storage.ErrBudgetExceeded) {
		t.Fatalf("Compact error = %v, want storage budget rejection", err)
	}
	if prepared {
		t.Fatal("compact preparation ran after budget rejection")
	}
	entries, err := os.ReadDir(filepath.Join(root, "fs", "sessions", "session"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".compact-") {
			t.Fatalf("compact scratch exists after preflight rejection: %s", entry.Name())
		}
	}
}

func TestCompactRejectsWriterLeaseHeldByAnotherSession(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	serving := openFixtureSession(t, root, manifest, reader, nil)
	writer, err := serving.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer writer.Close()

	maintenance := openFixtureSession(t, root, manifest, reader, nil)
	_, err = maintenance.Compact(context.Background(), CompactOptions{
		Prepare: func(context.Context, NativeFile, uint64) (PreparedGeneration, error) {
			t.Fatal("compact preparation ran while another process held the writer lease")
			return PreparedGeneration{}, nil
		},
	})
	if !errors.Is(err, ErrWriterBusy) {
		t.Fatalf("Compact error = %v, want %v", err, ErrWriterBusy)
	}
}

func TestCompactHoldsAndReleasesInProcessWriterState(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	stop := errors.New("stop during preparation")
	_, err := session.Compact(context.Background(), CompactOptions{
		Prepare: func(context.Context, NativeFile, uint64) (PreparedGeneration, error) {
			session.mu.Lock()
			held := session.writerOpen
			session.mu.Unlock()
			if !held {
				t.Fatal("compact did not publish its in-process writer state")
			}
			if writer, writerErr := session.OpenWriter(); !errors.Is(writerErr, ErrWriterBusy) {
				if writer != nil {
					_ = writer.Close()
				}
				t.Fatalf("OpenWriter during compact = %v, want %v", writerErr, ErrWriterBusy)
			}
			return PreparedGeneration{}, stop
		},
	})
	if !errors.Is(err, stop) {
		t.Fatalf("Compact error = %v, want %v", err, stop)
	}
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter after compact failure: %v", err)
	}
	_ = writer.Close()
}

func TestRecoverInterruptedCompactRemovesJournalOwnedScratch(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	state := session.State()
	next := state
	next.Generation++
	next.DeltaPath = filepath.Join(session.directory, "delta-00000000000000000002.jsonl")
	scratch := filepath.Join(session.directory, ".compact-00000000000000000001.jsonl")
	stateTemporary := filepath.Join(session.directory, ".state-compact-00000000000000000002.tmp")
	for path, data := range map[string][]byte{
		next.DeltaPath: nil,
		scratch:        source,
		stateTemporary: []byte("partial state"),
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write interrupted compact artifact %s: %v", path, err)
		}
	}
	if err := appendJournal(session.directory, JournalRecord{
		OperationID: "compact-00000000000000000001", SessionID: state.SessionID,
		Kind: "compact", Phase: "state-publishing", Candidate: next,
		TempPath: stateTemporary, FinalPath: next.DeltaPath,
		Native: NativeFile{Path: scratch, Bytes: int64(len(source)), SHA256: digestBytes(source)},
	}); err != nil {
		t.Fatalf("append interrupted compact journal: %v", err)
	}

	reopened := openFixtureSession(t, root, manifest, reader, nil)
	if reopened.State().Generation != state.Generation || reopened.State().DeltaPath != state.DeltaPath {
		t.Fatalf("recovery changed committed state: %#v", reopened.State())
	}
	for _, path := range []string{next.DeltaPath, scratch, stateTemporary} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("recovery left interrupted compact artifact %s: %v", path, err)
		}
	}
	records, err := readJournal(session.directory)
	if err != nil {
		t.Fatalf("read recovered journal: %v", err)
	}
	latest := records[len(records)-1]
	if latest.Phase != "rolled-back" || latest.TempPath != stateTemporary || latest.Native.Path != scratch {
		t.Fatalf("recovery did not preserve cleanup ownership: %#v", latest)
	}
}

func TestOpenSessionCleansUnlockedStaleWriterLease(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	leasePath := filepath.Join(session.directory, "writer.lease")
	if err := os.WriteFile(leasePath, []byte("stale\n"), 0o600); err != nil {
		t.Fatalf("write stale lease: %v", err)
	}
	reopened := openFixtureSession(t, root, manifest, reader, nil)
	writer, err := reopened.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter after stale lease cleanup: %v", err)
	}
	_ = writer.Close()
}

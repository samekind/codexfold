package vfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestRecoverSessionJournalWithWriterLeaseRollsBackDataSyncedCopyOnWrite(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	state := session.State()
	temporary := filepath.Join(session.directory, ".backing-crashed.tmp")
	if err := os.WriteFile(temporary, source, 0o600); err != nil {
		t.Fatal(err)
	}
	stateTemporary := filepath.Join(session.directory, ".state-primary-crashed.tmp")
	if err := os.WriteFile(stateTemporary, []byte("interrupted primary publication"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := hashNativePath(temporary)
	if err != nil {
		t.Fatal(err)
	}
	if err := appendJournal(session.directory, JournalRecord{
		OperationID: fmt.Sprintf("cow-%020d", state.Generation), SessionID: state.SessionID,
		Kind: "copy-on-write", Phase: "data-synced", TempPath: temporary, Native: identity,
	}); err != nil {
		t.Fatal(err)
	}
	guard, acquired, err := TryAcquireWriterLeaseGuardAtPath(filepath.Join(session.directory, "writer.lease"))
	if err != nil || !acquired {
		t.Fatalf("acquire writer guard: acquired=%t err=%v", acquired, err)
	}
	defer guard.Close()
	recovered, err := RecoverSessionJournalWithWriterLease(context.Background(), session.statePath)
	if err != nil {
		t.Fatalf("RecoverSessionJournalWithWriterLease: %v", err)
	}
	if recovered != state {
		t.Fatalf("recovered state = %#v, want %#v", recovered, state)
	}
	if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal-owned temporary remains: %v", err)
	}
	if _, err := os.Stat(stateTemporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("proven state metadata temporary remains: %v", err)
	}
	records, err := readJournal(session.directory)
	if err != nil {
		t.Fatal(err)
	}
	if latest := records[len(records)-1]; latest.Phase != "rolled-back" || latest.OperationID != fmt.Sprintf("cow-%020d", state.Generation) {
		t.Fatalf("latest journal record = %#v", latest)
	}
	if verified, err := LoadSessionStateWithWriterLease(session.statePath); err != nil || verified != state {
		t.Fatalf("strict state after replay = %#v, %v", verified, err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	advanced := openFixtureSession(t, root, manifest, reader, nil)
	writer, err := advanced.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteAt(context.Background(), []byte("X"), 0); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened := openFixtureSession(t, root, manifest, reader, nil); reopened.State().Generation != state.Generation+1 {
		t.Fatalf("reopened state after later COW = %#v", reopened.State())
	}
}

func TestRecoverSessionJournalWithWriterLeasePreservesUnsafeCOWTemporary(t *testing.T) {
	for _, test := range []struct {
		name      string
		temporary func(*testing.T, string) (string, NativeFile)
	}{
		{
			name: "outside session",
			temporary: func(t *testing.T, _ string) (string, NativeFile) {
				path := filepath.Join(t.TempDir(), ".backing-outside.tmp")
				if err := os.WriteFile(path, []byte("outside evidence"), 0o600); err != nil {
					t.Fatal(err)
				}
				identity, err := hashNativePath(path)
				if err != nil {
					t.Fatal(err)
				}
				return path, identity
			},
		},
		{
			name: "symlink",
			temporary: func(t *testing.T, directory string) (string, NativeFile) {
				target := filepath.Join(t.TempDir(), "outside.txt")
				if err := os.WriteFile(target, []byte("symlink target evidence"), 0o600); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(directory, ".backing-symlink.tmp")
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
				identity, err := hashNativePath(path)
				if err != nil {
					t.Fatal(err)
				}
				return path, identity
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			manifest, reader, _ := sessionFixture(t, root)
			session := openFixtureSession(t, root, manifest, reader, nil)
			state := session.State()
			temporary, identity := test.temporary(t, session.directory)
			if err := appendJournal(session.directory, JournalRecord{
				OperationID: fmt.Sprintf("cow-%020d", state.Generation), SessionID: state.SessionID,
				Kind: "copy-on-write", Phase: "data-synced", TempPath: temporary, Native: identity,
			}); err != nil {
				t.Fatal(err)
			}
			guard, acquired, err := TryAcquireWriterLeaseGuardAtPath(filepath.Join(session.directory, "writer.lease"))
			if err != nil || !acquired {
				t.Fatalf("acquire writer guard: acquired=%t err=%v", acquired, err)
			}
			defer guard.Close()
			if _, err := RecoverSessionJournalWithWriterLease(context.Background(), session.statePath); err == nil {
				t.Fatal("unsafe journal-owned temporary was accepted")
			}
			if _, err := os.Lstat(temporary); err != nil {
				t.Fatalf("unsafe temporary evidence changed: %v", err)
			}
			records, err := readJournal(session.directory)
			if err != nil {
				t.Fatal(err)
			}
			if latest := records[len(records)-1]; latest.Phase != "data-synced" {
				t.Fatalf("unsafe journal was advanced: %#v", latest)
			}
		})
	}
}

func TestRecoverSessionJournalWithWriterLeaseFinishesPublishedCopyOnWrite(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	state := session.State()
	backing := filepath.Join(session.directory, fmt.Sprintf("backing-%020d.jsonl", state.Generation+1))
	if err := os.WriteFile(backing, source, 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := hashNativePath(backing)
	if err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(session.directory, ".backing-crashed.tmp")
	identity.Path = temporary
	candidate := state
	candidate.Generation++
	candidate.BackingPath = backing
	if err := appendJournal(session.directory, JournalRecord{
		OperationID: fmt.Sprintf("cow-%020d", state.Generation), SessionID: state.SessionID,
		Kind: "copy-on-write", Phase: "after-file-publish", Candidate: candidate,
		FinalPath: backing, Native: identity,
	}); err != nil {
		t.Fatal(err)
	}
	guard, acquired, err := TryAcquireWriterLeaseGuardAtPath(filepath.Join(session.directory, "writer.lease"))
	if err != nil || !acquired {
		t.Fatalf("acquire writer guard: acquired=%t err=%v", acquired, err)
	}
	defer guard.Close()
	recovered, err := RecoverSessionJournalWithWriterLease(context.Background(), session.statePath)
	if err != nil {
		t.Fatalf("RecoverSessionJournalWithWriterLease: %v", err)
	}
	if recovered != candidate {
		t.Fatalf("recovered state = %#v, want %#v", recovered, candidate)
	}
	records, err := readJournal(session.directory)
	if err != nil {
		t.Fatal(err)
	}
	if latest := records[len(records)-1]; latest.Phase != "complete" || latest.OperationID != fmt.Sprintf("cow-%020d", state.Generation) {
		t.Fatalf("latest journal record = %#v", latest)
	}
	if verified, err := LoadSessionStateWithWriterLease(session.statePath); err != nil || verified != candidate {
		t.Fatalf("strict state after replay = %#v, %v", verified, err)
	}
}

func TestRecoverSessionJournalWithWriterLeaseAdoptsPinnedCOWCheckpointBeforeRepublish(t *testing.T) {
	for _, mode := range []string{"checkpoint-temporary", "checkpoint-final", "checkpoint-final-with-identical-orphan"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			manifest, reader, source := sessionFixture(t, root)
			session := openFixtureSession(t, root, manifest, reader, nil)
			state := session.State()
			chain, err := loadStateCheckpointChain(session.directory, state.SessionID, true)
			if err != nil {
				t.Fatal(err)
			}
			request, err := json.Marshal(map[string]string{"checkpoint_sha256": chain.catalog.CheckpointSHA256})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(session.directory, "retire.request.json"), append(request, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			backing := filepath.Join(session.directory, fmt.Sprintf("backing-%020d.jsonl", state.Generation+1))
			if err := os.WriteFile(backing, source, 0o600); err != nil {
				t.Fatal(err)
			}
			identity, err := hashNativePath(backing)
			if err != nil {
				t.Fatal(err)
			}
			identity.Path = filepath.Join(session.directory, ".backing-checkpointcrash.tmp")
			candidate := state
			candidate.Generation++
			candidate.BackingPath = backing
			stateData, err := encodeSessionState(candidate)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err := buildStateCheckpoint(candidate, digestStateBytes(stateData), chain.catalog.CheckpointSHA256, chain.catalog.Sequence+1, session.directory, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkpointData, err := encodeStateCheckpoint(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			checkpointSHA256 := digestStateBytes(checkpointData)
			checkpointName := stateCheckpointFilename(candidate.Generation, checkpoint.Sequence, checkpointSHA256)
			generations := filepath.Join(session.directory, stateGenerationsDirectoryName)
			checkpointTemporary, err := os.CreateTemp(generations, ".state-checkpoint-*.tmp")
			if err != nil {
				t.Fatal(err)
			}
			checkpointTemporaryPath := checkpointTemporary.Name()
			if _, err := checkpointTemporary.Write(checkpointData); err != nil {
				_ = checkpointTemporary.Close()
				t.Fatal(err)
			}
			if err := checkpointTemporary.Sync(); err != nil {
				_ = checkpointTemporary.Close()
				t.Fatal(err)
			}
			if err := checkpointTemporary.Close(); err != nil {
				t.Fatal(err)
			}
			if mode != "checkpoint-temporary" {
				checkpointPath := filepath.Join(generations, checkpointName)
				if err := os.Link(checkpointTemporaryPath, checkpointPath); err != nil {
					t.Fatal(err)
				}
				if err := syncStateDirectory(generations); err != nil {
					t.Fatal(err)
				}
				if mode == "checkpoint-final-with-identical-orphan" {
					orphanDirectory := filepath.Join(session.directory, "state-orphans")
					if err := os.Mkdir(orphanDirectory, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(orphanDirectory, checkpointName), checkpointData, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := appendJournal(session.directory, JournalRecord{
				OperationID: fmt.Sprintf("cow-%020d", state.Generation), SessionID: state.SessionID,
				Kind: "copy-on-write", Phase: "after-file-publish", Candidate: candidate,
				FinalPath: backing, Native: identity,
			}); err != nil {
				t.Fatal(err)
			}
			guard, acquired, err := TryAcquireWriterLeaseGuardAtPath(filepath.Join(session.directory, "writer.lease"))
			if err != nil || !acquired {
				t.Fatalf("acquire writer guard: acquired=%t err=%v", acquired, err)
			}
			defer guard.Close()
			recovered, err := RecoverSessionJournalWithWriterLease(context.Background(), session.statePath)
			if err != nil {
				t.Fatal(err)
			}
			if recovered != candidate {
				t.Fatalf("recovered COW state=%#v want=%#v", recovered, candidate)
			}
			replayed, err := RecoverSessionJournalWithWriterLease(context.Background(), session.statePath)
			if err != nil || replayed != candidate {
				t.Fatalf("idempotent COW replay=%#v want=%#v err=%v", replayed, candidate, err)
			}
			if _, err := os.Lstat(checkpointTemporaryPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("checkpoint temporary remains: %v", err)
			}
			records, err := readJournal(session.directory)
			if err != nil {
				t.Fatal(err)
			}
			if latest := records[len(records)-1]; latest.Phase != "complete" {
				t.Fatalf("latest COW journal record=%#v", latest)
			}
		})
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
			manifestPath := filepath.Join(root, "manifests", "generations", preparedManifest.Session.ID, "00000000000000000002.json")
			persistManifestFixture(t, manifestPath, preparedManifest)
			return PreparedGeneration{ManifestPath: manifestPath, Manifest: preparedManifest, View: view}, err
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
		manifestPath := filepath.Join(root, "manifests", "generations", prepared.Session.ID, "00000000000000000002.json")
		persistManifestFixture(t, manifestPath, prepared)
		return PreparedGeneration{ManifestPath: manifestPath, Manifest: prepared, View: view}, err
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
	if collected.RemovedCount != 0 || collected.RetainedUnprovedCount != 1 {
		t.Fatalf("closed reader generation was not retained without exact deletion proof: %#v", collected)
	}
	if _, err := os.Stat(oldDelta); err != nil {
		t.Fatalf("unproved old delta was removed after reader close: %v", err)
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
			return PreparedGeneration{ManifestPath: filepath.Join(root, "manifests", "generations", preparedManifest.Session.ID, "00000000000000000002.json"), Manifest: preparedManifest, View: view}, nil
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
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
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

func TestRecoverSessionJournalWithWriterLeaseAdoptsPinnedCompactCheckpointBeforeRollback(t *testing.T) {
	for _, linkFinal := range []bool{false, true} {
		name := "checkpoint-temporary"
		if linkFinal {
			name = "checkpoint-final"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			manifest, reader, source := sessionFixture(t, root)
			session := openFixtureSession(t, root, manifest, reader, nil)
			state := session.State()
			directory := session.directory
			tail := []byte("-retained-compact-predecessor")
			active, err := os.OpenFile(state.DeltaPath, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := active.Write(tail); err != nil {
				_ = active.Close()
				t.Fatal(err)
			}
			if err := active.Sync(); err != nil {
				_ = active.Close()
				t.Fatal(err)
			}
			if err := active.Close(); err != nil {
				t.Fatal(err)
			}
			if err := refreshSessionStateCheckpoint(session.statePath, state); err != nil {
				t.Fatal(err)
			}
			chain, err := loadStateCheckpointChain(directory, state.SessionID, true)
			if err != nil {
				t.Fatal(err)
			}
			request, err := json.Marshal(map[string]string{"checkpoint_sha256": chain.catalog.CheckpointSHA256})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "retire.request.json"), append(request, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}

			currentBytes := append(append([]byte(nil), source...), tail...)
			currentSHA256 := digestBytes(currentBytes)
			compactedManifest := fold.Manifest{
				Version: fold.ManifestVersion, Kind: fold.ManifestKind,
				Session: fold.ManifestSession{ID: state.SessionID, RolloutPath: manifest.Session.RolloutPath},
				Source:  fold.ManifestSource{Bytes: int64(len(currentBytes)), SHA256: currentSHA256},
				Parts:   []fold.Part{{Kind: fold.PartResidual, Object: fold.ObjectRef{SHA256: currentSHA256, RawBytes: int64(len(currentBytes))}}},
			}
			compactedManifestPath := filepath.Join(root, "manifests", "generations", state.SessionID, "00000000000000000002.json")
			persistManifestFixture(t, compactedManifestPath, compactedManifest)
			manifestIdentity, _, err := captureManifestIdentity(compactedManifestPath, state.SessionID, int64(len(currentBytes)), currentSHA256)
			if err != nil {
				t.Fatal(err)
			}
			next := state
			next.Generation++
			next.ManifestPath = compactedManifestPath
			next.ManifestSHA256 = manifestIdentity.SHA256
			next.BaseBytes = int64(len(currentBytes))
			next.BaseSHA256 = currentSHA256
			next.DeltaPath = filepath.Join(directory, fmt.Sprintf("delta-%020d.jsonl", next.Generation))
			if err := os.WriteFile(next.DeltaPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			scratch := filepath.Join(directory, fmt.Sprintf(".compact-%020d.jsonl", state.Generation))
			if err := os.WriteFile(scratch, currentBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			native, err := hashNativePath(scratch)
			if err != nil {
				t.Fatal(err)
			}
			stateTemporary := filepath.Join(directory, fmt.Sprintf(".state-compact-%020d.tmp", next.Generation))
			if err := os.WriteFile(stateTemporary, []byte("interrupted primary state"), 0o600); err != nil {
				t.Fatal(err)
			}

			stateData, err := encodeSessionState(next)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err := buildStateCheckpoint(next, digestStateBytes(stateData), chain.catalog.CheckpointSHA256, chain.catalog.Sequence+1, directory, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkpointData, err := encodeStateCheckpoint(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			checkpointSHA256 := digestStateBytes(checkpointData)
			checkpointName := stateCheckpointFilename(next.Generation, checkpoint.Sequence, checkpointSHA256)
			generations := filepath.Join(directory, stateGenerationsDirectoryName)
			checkpointTemporary, err := os.CreateTemp(generations, ".state-checkpoint-*.tmp")
			if err != nil {
				t.Fatal(err)
			}
			checkpointTemporaryPath := checkpointTemporary.Name()
			if _, err := checkpointTemporary.Write(checkpointData); err != nil {
				_ = checkpointTemporary.Close()
				t.Fatal(err)
			}
			if err := checkpointTemporary.Sync(); err != nil {
				_ = checkpointTemporary.Close()
				t.Fatal(err)
			}
			if err := checkpointTemporary.Close(); err != nil {
				t.Fatal(err)
			}
			if linkFinal {
				if err := os.Link(checkpointTemporaryPath, filepath.Join(generations, checkpointName)); err != nil {
					t.Fatal(err)
				}
				if err := syncStateDirectory(generations); err != nil {
					t.Fatal(err)
				}
			}
			if err := appendJournal(directory, JournalRecord{
				OperationID: fmt.Sprintf("compact-%020d", state.Generation), SessionID: state.SessionID,
				Kind: "compact", Phase: "state-publishing", Candidate: next,
				TempPath: stateTemporary, FinalPath: next.DeltaPath, Native: native,
			}); err != nil {
				t.Fatal(err)
			}

			guard, acquired, err := TryAcquireWriterLeaseGuardAtPath(filepath.Join(directory, "writer.lease"))
			if err != nil || !acquired {
				t.Fatalf("acquire writer guard: acquired=%t err=%v", acquired, err)
			}
			defer guard.Close()
			recovered, err := RecoverSessionJournalWithWriterLease(context.Background(), session.statePath)
			if err != nil {
				t.Fatal(err)
			}
			if recovered != next {
				t.Fatalf("recovered compact state=%#v want=%#v", recovered, next)
			}
			for _, removed := range []string{scratch, stateTemporary, checkpointTemporaryPath} {
				if _, err := os.Lstat(removed); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("compact recovery temporary remains %s: %v", removed, err)
				}
			}
			if _, err := os.Stat(next.DeltaPath); err != nil {
				t.Fatalf("adopted compact delta was removed: %v", err)
			}
			records, err := readJournal(directory)
			if err != nil {
				t.Fatal(err)
			}
			if latest := records[len(records)-1]; latest.Phase != "complete" {
				t.Fatalf("latest compact journal record=%#v", latest)
			}
		})
	}
}

func TestRecoverSessionJournalWithWriterLeaseDoesNotRollbackCurrentCompactDelta(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	current := session.State()
	current.Generation++
	current.DeltaPath = filepath.Join(session.directory, fmt.Sprintf("delta-%020d.jsonl", current.Generation))
	if err := os.WriteFile(current.DeltaPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishSessionState(session.statePath, current); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(session.directory, fmt.Sprintf(".compact-%020d.jsonl", current.Generation-1))
	if err := os.WriteFile(scratch, source, 0o600); err != nil {
		t.Fatal(err)
	}
	native, err := hashNativePath(scratch)
	if err != nil {
		t.Fatal(err)
	}
	stateTemporary := filepath.Join(session.directory, fmt.Sprintf(".state-compact-%020d.tmp", current.Generation))
	if err := os.WriteFile(stateTemporary, []byte("crafted state temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendJournal(session.directory, JournalRecord{
		OperationID: fmt.Sprintf("compact-%020d", current.Generation-1), SessionID: current.SessionID,
		Kind: "compact", Phase: "prepared", Candidate: current,
		TempPath: stateTemporary, FinalPath: current.DeltaPath, Native: native,
	}); err != nil {
		t.Fatal(err)
	}
	guard, acquired, err := TryAcquireWriterLeaseGuardAtPath(filepath.Join(session.directory, "writer.lease"))
	if err != nil || !acquired {
		t.Fatalf("acquire writer guard: acquired=%t err=%v", acquired, err)
	}
	defer guard.Close()
	if _, err := RecoverSessionJournalWithWriterLease(context.Background(), session.statePath); err == nil {
		t.Fatal("crafted compact rollback was accepted")
	}
	if _, err := os.Stat(current.DeltaPath); err != nil {
		t.Fatalf("current compact delta was removed: %v", err)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("crafted compact scratch evidence was removed: %v", err)
	}
	records, err := readJournal(session.directory)
	if err != nil {
		t.Fatal(err)
	}
	if latest := records[len(records)-1]; latest.Phase != "prepared" {
		t.Fatalf("crafted compact journal was advanced: %#v", latest)
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

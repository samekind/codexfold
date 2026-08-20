package vfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestOpenSessionCannotFollowManifestDirectorySymlinkOutsideStore(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	manifestPath := fold.ManifestPath(root, manifest.Session.ID)
	external := t.TempDir()
	externalManifest := filepath.Join(external, filepath.Base(manifestPath))
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(externalManifest, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "manifests")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "manifests")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: manifestPath, Manifest: manifest, Reader: reader,
		NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
	}); err == nil {
		t.Fatal("managed session followed a manifest directory symlink outside the store")
	}
}

func TestManagedSessionCannotFollowSessionDirectorySymlinkOutsideStore(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	_ = openFixtureSession(t, root, manifest, reader, nil)
	sessionsPath := filepath.Join(root, "fs", "sessions")
	externalSessions := filepath.Join(t.TempDir(), "sessions")
	if err := os.Rename(sessionsPath, externalSessions); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalSessions, sessionsPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DiscoverSessionStatesDetailed(root); err == nil {
		t.Fatal("managed state discovery followed a session root symlink outside the store")
	}
	if _, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
		NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
	}); err == nil {
		t.Fatal("managed session opened delta data through a session root symlink outside the store")
	}
}

func TestOpenSessionRecoversAbandonedInitialPublicationStages(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, root string, manifest fold.Manifest)
	}{
		{
			name: "staging directory created",
			prepare: func(t *testing.T, root string, manifest fold.Manifest) {
				createInitialStagingFixture(t, root, manifest, false, false)
			},
		},
		{
			name: "staged delta synced",
			prepare: func(t *testing.T, root string, manifest fold.Manifest) {
				createInitialStagingFixture(t, root, manifest, true, false)
			},
		},
		{
			name: "staged state synced before publish",
			prepare: func(t *testing.T, root string, manifest fold.Manifest) {
				createInitialStagingFixture(t, root, manifest, true, true)
			},
		},
		{
			name: "legacy directory created",
			prepare: func(t *testing.T, root string, manifest fold.Manifest) {
				directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "legacy delta and temporary state created",
			prepare: func(t *testing.T, root string, manifest fold.Manifest) {
				directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, "delta.jsonl"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, "writer.lease"), []byte("123\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, ".state-killed.tmp"), []byte("{\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			manifest, reader, source := sessionFixture(t, root)
			if err := os.MkdirAll(filepath.Join(root, "fs", "sessions"), 0o700); err != nil {
				t.Fatal(err)
			}
			test.prepare(t, root, manifest)

			session := openFixtureSession(t, root, manifest, reader, nil)
			handle, err := session.OpenReader()
			if err != nil {
				t.Fatalf("OpenReader after recovery: %v", err)
			}
			if got := readHandle(t, handle); !bytes.Equal(got, source) {
				t.Fatalf("recovered bytes = %q, want %q", got, source)
			}
			_ = handle.Close()

			statePath := filepath.Join(root, "fs", "sessions", manifest.Session.ID, "state.json")
			if _, err := LoadSessionState(statePath); err != nil {
				t.Fatalf("published state is invalid: %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(root, "fs", "sessions"))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if isInitialSessionStagingName(entry.Name()) {
					t.Fatalf("abandoned staging remains after recovery: %s", entry.Name())
				}
			}
		})
	}
}

func TestOpenSessionDoesNotDiscardUnrecognizedMissingStateDirectory(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	deltaPath := filepath.Join(directory, "delta.jsonl")
	contents := []byte("possibly committed session bytes")
	if err := os.WriteFile(deltaPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
		NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
	})
	if err == nil {
		t.Fatal("OpenSession should reject an unrecognized missing-state directory")
	}
	got, readErr := os.ReadFile(deltaPath)
	if readErr != nil || !bytes.Equal(got, contents) {
		t.Fatalf("unrecognized data changed: got=%q err=%v", got, readErr)
	}
}

func TestOpenSessionRejectsEscapingSessionIDsBeforeCreatingFilesystemState(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	for _, sessionID := range []string{"../escaped", "nested/session", `nested\session`, ".."} {
		manifest.Session.ID = sessionID
		_, err := OpenSession(context.Background(), SessionOptions{
			Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
			NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
		})
		if err == nil {
			t.Fatalf("OpenSession accepted unsafe session ID %q", sessionID)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "fs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe session ID created managed filesystem state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "escaped")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe session ID escaped the session root: %v", err)
	}
}

func TestConcurrentInitialOpenPublishesOneCompleteSession(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	const opens = 8
	start := make(chan struct{})
	results := make(chan error, opens)
	var wait sync.WaitGroup
	for range opens {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			session, err := OpenSession(context.Background(), SessionOptions{
				Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
				NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
			})
			if err == nil && session.State().Generation != 1 {
				err = fmt.Errorf("generation = %d, want 1", session.State().Generation)
			}
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent OpenSession: %v", err)
		}
	}

	directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
	if _, err := LoadSessionState(filepath.Join(directory, "state.json")); err != nil {
		t.Fatalf("published state is invalid: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(directory))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if isInitialSessionStagingName(entry.Name()) {
			t.Fatalf("concurrent publication left staging directory %s", entry.Name())
		}
	}
}

func TestOpenSessionWithWriterPublishesCompleteLockedSession(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	session, writer, err := OpenSessionWithWriter(context.Background(), SessionOptions{
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
		NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
	})
	if err != nil {
		t.Fatalf("OpenSessionWithWriter: %v", err)
	}
	if writer == nil {
		t.Fatal("OpenSessionWithWriter returned no reserved writer")
	}
	if _, err := session.OpenWriter(); !errors.Is(err, ErrWriterBusy) {
		t.Fatalf("second writer error = %v, want ErrWriterBusy", err)
	}
	if _, err := writer.Append(context.Background(), []byte("-tail")); err != nil {
		t.Fatalf("reserved writer append: %v", err)
	}
	if err := writer.Sync(); err != nil {
		t.Fatalf("reserved writer sync: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("reserved writer close: %v", err)
	}

	reopened := openFixtureSession(t, root, manifest, reader, nil)
	handle, err := reopened.OpenReader()
	if err != nil {
		t.Fatalf("OpenReader after reserved writer: %v", err)
	}
	defer handle.Close()
	want := append(append([]byte(nil), source...), []byte("-tail")...)
	if got := readHandle(t, handle); !bytes.Equal(got, want) {
		t.Fatalf("bytes after reserved writer = %q, want %q", got, want)
	}
}

func TestInitialSessionLeaseWaitHonorsContextCancellation(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	parent := filepath.Join(root, "fs", "sessions")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireWriterLease(filepath.Join(parent, initialSessionLockName(manifest.Session.ID)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = unlockWriterFile(lease)
		_ = lease.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = OpenSession(ctx, SessionOptions{
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
		NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("OpenSession error = %v, want context deadline", err)
	}
	if _, err := os.Stat(filepath.Join(parent, manifest.Session.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled initialization exposed a session directory: %v", err)
	}
}

func TestOpenSessionPreservesUnrecognizedStagingData(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	parent := filepath.Join(root, "fs", "sessions")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	staging, err := os.MkdirTemp(parent, initialSessionStagingNamePrefix(manifest.Session.ID)+"*")
	if err != nil {
		t.Fatal(err)
	}
	unknownPath := filepath.Join(staging, "unknown.data")
	contents := []byte("must not be deleted")
	if err := os.WriteFile(unknownPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	session, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
		NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
	})
	if err != nil {
		t.Fatalf("OpenSession with unrelated staging: %v", err)
	}
	if session.State().SessionID != manifest.Session.ID {
		t.Fatalf("published state = %#v", session.State())
	}
	got, readErr := os.ReadFile(unknownPath)
	if readErr != nil || !bytes.Equal(got, contents) {
		t.Fatalf("unrecognized staging data changed: got=%q err=%v", got, readErr)
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
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
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
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
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
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
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
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
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

func TestRetireNativeSnapshotKeepsPreparedProofWhenStatePublicationFails(t *testing.T) {
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
		Root: root, ManifestPath: filepath.Join(root, "manifests", "session.json"), Manifest: manifest, Reader: reader, NativeSnapshot: native,
	})
	if err != nil {
		t.Fatal(err)
	}
	visible, err := session.MaterializeCurrent(context.Background(), filepath.Join(root, "visible-proof.jsonl"), false)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	generations := filepath.Join(root, "fs", "sessions", "session", stateGenerationsDirectoryName)
	if err := os.RemoveAll(generations); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(generations, []byte("block checkpoint publication"), 0o600); err != nil {
		t.Fatal(err)
	}

	proof, err := session.RetireNativeSnapshot(native, visible)
	if err == nil {
		t.Fatal("broken checkpoint path allowed native retirement state publication")
	}
	if proof.Snapshot != native || proof.StateGeneration != 1 {
		t.Fatalf("prepared retirement proof = %#v", proof)
	}
	proofPath := filepath.Join(root, "fs", "sessions", "session", NativeRetirementFilename)
	loaded, loadErr := LoadNativeRetirementProof(proofPath)
	if loadErr != nil || loaded != proof {
		t.Fatalf("prepared proof was not retained: proof=%#v err=%v", loaded, loadErr)
	}
	persisted, _, readErr := readSessionState(filepath.Join(root, "fs", "sessions", "session", "state.json"))
	if readErr != nil || persisted.Generation != proof.StateGeneration || persisted.NativeSnapshot != native {
		t.Fatalf("failed publication changed primary state: state=%#v err=%v", persisted, readErr)
	}
	if err := os.Remove(hiddenSnapshot); err != nil {
		t.Fatal(err)
	}
	if retired, err := NativeSnapshotAlreadyRetired(root, persisted); err == nil || retired {
		t.Fatalf("equal-generation prepared proof authorized a missing snapshot: retired=%t err=%v", retired, err)
	}
}

func TestRetireNativeSnapshotBindsAndRemovesNonemptySidecar(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	hiddenSnapshot := filepath.Join(root, "fs", "snapshots", "session", "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(hiddenSnapshot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(manifest.Session.RolloutPath, hiddenSnapshot); err != nil {
		t.Fatal(err)
	}
	sidecarPath := filepath.Join(filepath.Dir(hiddenSnapshot), "._native.jsonl")
	sidecarBytes := []byte("nonempty appledouble metadata")
	if err := os.WriteFile(sidecarPath, sidecarBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	native := NativeFile{Path: hiddenSnapshot, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256}
	session, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: filepath.Join(root, "manifests", "session.json"), Manifest: manifest, Reader: reader, NativeSnapshot: native,
	})
	if err != nil {
		t.Fatal(err)
	}
	visible, err := session.MaterializeCurrent(context.Background(), filepath.Join(root, "visible-sidecar-proof.jsonl"), false)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	proof, err := session.RetireNativeSnapshot(native, visible)
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if proof.Version != nativeRetirementVersion || proof.Sidecar == nil || proof.Sidecar.Path != sidecarPath || proof.Sidecar.Bytes != int64(len(sidecarBytes)) || proof.Sidecar.SHA256 != digestBytes(sidecarBytes) {
		t.Fatalf("native sidecar proof = %#v", proof.Sidecar)
	}
	for _, path := range []string{hiddenSnapshot, sidecarPath} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("proved native retirement artifact remains at %s: %v", path, err)
		}
	}
}

func TestCompleteNativeSnapshotRetirementPreservesUnprovedNonemptySidecar(t *testing.T) {
	root := t.TempDir()
	snapshotPath := filepath.Join(root, "fs", "snapshots", "session", "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o700); err != nil {
		t.Fatal(err)
	}
	snapshotBytes := []byte("snapshot")
	sidecarBytes := []byte("unproved sidecar")
	if err := os.WriteFile(snapshotPath, snapshotBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	sidecarPath := filepath.Join(filepath.Dir(snapshotPath), "._native.jsonl")
	if err := os.WriteFile(sidecarPath, sidecarBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	proof := NativeRetirementProof{
		Version: legacyNativeRetirementVersion, SessionID: "session", StateGeneration: 1,
		RetiredAt: time.Now().UTC().Format(time.RFC3339Nano),
		Snapshot:  NativeFile{Path: snapshotPath, Bytes: int64(len(snapshotBytes)), SHA256: digestBytes(snapshotBytes)},
		Visible:   NativeFile{Path: filepath.Join(root, "visible.jsonl"), Bytes: int64(len(snapshotBytes)), SHA256: digestBytes(snapshotBytes)},
	}
	if err := CompleteNativeSnapshotRetirement(root, proof); err == nil {
		t.Fatal("legacy proof removed a nonempty sidecar without exact identity")
	}
	for path, want := range map[string][]byte{snapshotPath: snapshotBytes, sidecarPath: sidecarBytes} {
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("unproved retirement artifact changed at %s: got=%q err=%v", path, got, err)
		}
	}
}

func TestCompleteNativeSnapshotRetirementReplaysCrashAfterAtomicStaging(t *testing.T) {
	root, snapshotPath, _, proof := nativeRetirementCompletionFixture(t, false)
	proofSHA256, err := nativeRetirementProofDigest(proof)
	if err != nil {
		t.Fatal(err)
	}
	stagingPath := filepath.Join(root, "fs", "native-retirement-staging", proof.SessionID, proofSHA256)
	markerPath := filepath.Join(root, "fs", "native-retirements", proof.SessionID, proofSHA256+".json")
	crash := errors.New("simulated crash after native snapshot staging")
	previousHook := nativeRetirementHook
	defer func() { nativeRetirementHook = previousHook }()
	nativeRetirementHook = func(phase string) error {
		if phase == nativeRetirementPhaseStaged {
			return crash
		}
		return nil
	}
	if err := CompleteNativeSnapshotRetirement(root, proof); !errors.Is(err, crash) {
		t.Fatalf("staging crash error = %v, want %v", err, crash)
	}
	if _, err := os.Lstat(snapshotPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical snapshot remained after atomic staging: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(stagingPath, "native.jsonl")); err != nil {
		t.Fatalf("exact snapshot did not remain in deterministic staging: %v", err)
	}
	if _, err := os.Lstat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deletion marker was published before the staging crash: %v", err)
	}

	nativeRetirementHook = nil
	if err := CompleteNativeSnapshotRetirement(root, proof); err != nil {
		t.Fatalf("replay staged native retirement: %v", err)
	}
	if _, err := os.Lstat(stagingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replayed native retirement staging remains: %v", err)
	}
	if _, err := os.Lstat(markerPath); err != nil {
		t.Fatalf("durable native retirement deletion marker missing: %v", err)
	}
}

func TestCompleteNativeSnapshotRetirementPreservesCanonicalReplacementAfterStaging(t *testing.T) {
	root, snapshotPath, _, proof := nativeRetirementCompletionFixture(t, false)
	replacement := []byte("replacement snapshot must survive")
	crash := errors.New("simulated crash after replacement appeared")
	previousHook := nativeRetirementHook
	defer func() { nativeRetirementHook = previousHook }()
	nativeRetirementHook = func(phase string) error {
		if phase != nativeRetirementPhaseStaged {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(snapshotPath, replacement, 0o600); err != nil {
			return err
		}
		return crash
	}
	if err := CompleteNativeSnapshotRetirement(root, proof); !errors.Is(err, crash) {
		t.Fatalf("replacement crash error = %v, want %v", err, crash)
	}

	nativeRetirementHook = nil
	if err := CompleteNativeSnapshotRetirement(root, proof); err != nil {
		t.Fatalf("replay retirement with canonical replacement: %v", err)
	}
	if got, err := os.ReadFile(snapshotPath); err != nil || !bytes.Equal(got, replacement) {
		t.Fatalf("canonical replacement changed: got=%q err=%v", got, err)
	}
	proofSHA256, err := nativeRetirementProofDigest(proof)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "fs", "native-retirement-staging", proof.SessionID, proofSHA256)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("proved staging remains after replacement-preserving replay: %v", err)
	}
}

func TestCompleteNativeSnapshotRetirementPreservesUnknownSnapshotDirectoryContent(t *testing.T) {
	root, snapshotPath, _, proof := nativeRetirementCompletionFixture(t, false)
	unknownPath := filepath.Join(filepath.Dir(snapshotPath), "foreign.data")
	unknown := []byte("unknown content must survive")
	if err := os.WriteFile(unknownPath, unknown, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CompleteNativeSnapshotRetirement(root, proof); err == nil {
		t.Fatal("unknown snapshot directory content was accepted for retirement")
	}
	if got, err := os.ReadFile(unknownPath); err != nil || !bytes.Equal(got, unknown) {
		t.Fatalf("unknown snapshot directory content changed: got=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(snapshotPath); err != nil || digestBytes(got) != proof.Snapshot.SHA256 {
		t.Fatalf("proved snapshot changed after unknown-content rejection: got=%q err=%v", got, err)
	}
}

func TestCompleteNativeSnapshotRetirementRefusesSymlinkedStagingAncestor(t *testing.T) {
	root, snapshotPath, _, proof := nativeRetirementCompletionFixture(t, false)
	outside := t.TempDir()
	stagingRoot := filepath.Join(root, "fs", "native-retirement-staging")
	if err := os.Symlink(outside, stagingRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := CompleteNativeSnapshotRetirement(root, proof); err == nil {
		t.Fatal("symlinked native retirement staging ancestor was accepted")
	}
	if got, err := os.ReadFile(snapshotPath); err != nil || digestBytes(got) != proof.Snapshot.SHA256 {
		t.Fatalf("snapshot changed through symlinked staging rejection: got=%q err=%v", got, err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside staging target changed: entries=%v err=%v", entries, err)
	}
}

func TestCompleteNativeSnapshotRetirementReplaysPartialDeletionAfterDurableMarker(t *testing.T) {
	root, _, sidecarPath, proof := nativeRetirementCompletionFixture(t, true)
	crash := errors.New("simulated crash after staged snapshot removal")
	previousHook := nativeRetirementHook
	defer func() { nativeRetirementHook = previousHook }()
	nativeRetirementHook = func(phase string) error {
		if phase == nativeRetirementPhaseSnapshotRemoved {
			return crash
		}
		return nil
	}
	if err := CompleteNativeSnapshotRetirement(root, proof); !errors.Is(err, crash) {
		t.Fatalf("partial deletion crash error = %v, want %v", err, crash)
	}
	proofSHA256, err := nativeRetirementProofDigest(proof)
	if err != nil {
		t.Fatal(err)
	}
	stagingPath := filepath.Join(root, "fs", "native-retirement-staging", proof.SessionID, proofSHA256)
	markerPath := filepath.Join(root, "fs", "native-retirements", proof.SessionID, proofSHA256+".json")
	if _, err := os.Lstat(filepath.Join(stagingPath, "native.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged snapshot was not removed before simulated crash: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(stagingPath, filepath.Base(sidecarPath))); err != nil {
		t.Fatalf("proved sidecar did not remain for replay: %v", err)
	}
	if _, err := os.Lstat(markerPath); err != nil {
		t.Fatalf("deletable marker was not durable before physical deletion: %v", err)
	}

	nativeRetirementHook = nil
	if err := CompleteNativeSnapshotRetirement(root, proof); err != nil {
		t.Fatalf("replay partial staged deletion: %v", err)
	}
	if _, err := os.Lstat(stagingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial staged deletion did not complete: %v", err)
	}
}

func TestCompleteNativeSnapshotRetirementRehashesSameInodeSizeAndMtimeBeforeStaging(t *testing.T) {
	root, snapshotPath, _, proof := nativeRetirementCompletionFixture(t, false)
	before, err := os.Lstat(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	mutated := bytes.Repeat([]byte("x"), int(before.Size()))
	previousHook := nativeRetirementHook
	defer func() { nativeRetirementHook = previousHook }()
	nativeRetirementHook = func(phase string) error {
		if phase != nativeRetirementPhaseVerified {
			return nil
		}
		file, err := os.OpenFile(snapshotPath, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, writeErr := file.WriteAt(mutated, 0)
		closeErr := file.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return err
		}
		return os.Chtimes(snapshotPath, before.ModTime(), before.ModTime())
	}
	if err := CompleteNativeSnapshotRetirement(root, proof); err == nil {
		t.Fatal("same-inode, same-size, restored-mtime mutation was accepted for retirement")
	}
	after, err := os.Lstat(snapshotPath)
	if err != nil {
		t.Fatalf("mutated snapshot was not preserved: %v", err)
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("test did not preserve inode/size/mtime: before=%v after=%v", before, after)
	}
	if got, err := os.ReadFile(snapshotPath); err != nil || !bytes.Equal(got, mutated) {
		t.Fatalf("mutated snapshot bytes changed: got=%q err=%v", got, err)
	}
}

func TestLoadNativeRetirementProofRejectsUnknownFields(t *testing.T) {
	root, _, _, proof := nativeRetirementCompletionFixture(t, false)
	data, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document["future_authority"] = true
	data, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "native-retirement-with-unknown-field.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadNativeRetirementProof(path); err == nil {
		t.Fatal("native retirement proof with unknown authority field was accepted")
	}
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
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader, NativeSnapshot: native,
	})
	if err != nil {
		t.Fatal(err)
	}
	maintenance, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader, NativeSnapshot: native,
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
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader, NativeSnapshot: native,
	})
	if err != nil {
		t.Fatal(err)
	}
	maintenance, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader, NativeSnapshot: native,
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
		manifestPath := filepath.Join(root, "manifests", "generations", manifest.Session.ID, fmt.Sprintf("%020d.json", generation))
		persistManifestFixture(t, manifestPath, prepared)
		return PreparedGeneration{ManifestPath: manifestPath, Manifest: prepared, View: view}, err
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
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader, NativeSnapshot: native,
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

func TestCompleteNativeSnapshotRetirementRefusesSymlinkedSnapshotAncestor(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	content := []byte("outside snapshot must survive")
	outsideSnapshot := filepath.Join(outside, "native.jsonl")
	if err := os.WriteFile(outsideSnapshot, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "fs", "snapshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "fs", "snapshots", "session")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	proof := NativeRetirementProof{
		Version: nativeRetirementVersion, SessionID: "session", StateGeneration: 1,
		RetiredAt: time.Now().UTC().Format(time.RFC3339Nano),
		Snapshot: NativeFile{
			Path:  filepath.Join(root, "fs", "snapshots", "session", "native.jsonl"),
			Bytes: int64(len(content)), SHA256: digestBytes(content),
		},
		Visible: NativeFile{Path: filepath.Join(root, "visible.jsonl"), Bytes: int64(len(content)), SHA256: digestBytes(content)},
	}
	if err := CompleteNativeSnapshotRetirement(root, proof); err == nil {
		t.Fatal("symlinked native snapshot ancestor allowed physical deletion")
	}
	if got, err := os.ReadFile(outsideSnapshot); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("outside snapshot changed: got=%q err=%v", got, err)
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
	persistManifestFixture(t, fold.ManifestPath(root, manifest.Session.ID), manifest)
	if err := os.WriteFile(manifest.Session.RolloutPath, source, 0o600); err != nil {
		t.Fatalf("write native snapshot: %v", err)
	}
	return manifest, reader, source
}

func persistManifestFixture(t *testing.T, path string, manifest fold.Manifest) {
	t.Helper()
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func createInitialStagingFixture(t *testing.T, root string, manifest fold.Manifest, withDelta bool, withState bool) {
	t.Helper()
	parent := filepath.Join(root, "fs", "sessions")
	staging, err := os.MkdirTemp(parent, initialSessionStagingNamePrefix(manifest.Session.ID)+"*")
	if err != nil {
		t.Fatal(err)
	}
	if withDelta {
		if err := os.WriteFile(filepath.Join(staging, "delta.jsonl"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if withState {
		state := SessionState{
			Version: sessionStateVersion, SessionID: manifest.Session.ID, Generation: 1,
			ManifestPath: fold.ManifestPath(root, manifest.Session.ID),
			ManifestSHA256: func() string {
				identity, err := captureRegularFileIdentity(fold.ManifestPath(root, manifest.Session.ID))
				if err != nil {
					t.Fatal(err)
				}
				return identity.SHA256
			}(),
			BaseBytes:  manifest.Source.Bytes,
			BaseSHA256: manifest.Source.SHA256,
			DeltaPath:  filepath.Join(parent, manifest.Session.ID, "delta.jsonl"),
			NativeSnapshot: NativeFile{
				Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256,
			},
		}
		if err := writeSessionState(filepath.Join(staging, "state.json"), state); err != nil {
			t.Fatal(err)
		}
	}
}

func openFixtureSession(t *testing.T, root string, manifest fold.Manifest, reader memoryReader, hook func(string) error) *Session {
	t.Helper()
	session, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
		NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
		BeforeCOWPhase: hook,
	})
	if err != nil {
		t.Fatalf("OpenSession returned error: %v", err)
	}
	return session
}

func nativeRetirementCompletionFixture(t *testing.T, withSidecar bool) (string, string, string, NativeRetirementProof) {
	t.Helper()
	root := t.TempDir()
	snapshotPath := filepath.Join(root, "fs", "snapshots", "session", "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o700); err != nil {
		t.Fatal(err)
	}
	snapshotBytes := []byte("exact native snapshot bytes")
	if err := os.WriteFile(snapshotPath, snapshotBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	proof := NativeRetirementProof{
		Version: nativeRetirementVersion, SessionID: "session", StateGeneration: 1,
		RetiredAt: time.Now().UTC().Format(time.RFC3339Nano),
		Snapshot:  NativeFile{Path: snapshotPath, Bytes: int64(len(snapshotBytes)), SHA256: digestBytes(snapshotBytes)},
		Visible: NativeFile{
			Path: filepath.Join(root, "visible.jsonl"), Bytes: int64(len(snapshotBytes)), SHA256: digestBytes(snapshotBytes),
		},
	}
	var sidecarPath string
	if withSidecar {
		sidecarPath = filepath.Join(filepath.Dir(snapshotPath), "._native.jsonl")
		sidecarBytes := []byte("exact native sidecar bytes")
		if err := os.WriteFile(sidecarPath, sidecarBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		proof.Sidecar = &NativeFile{Path: sidecarPath, Bytes: int64(len(sidecarBytes)), SHA256: digestBytes(sidecarBytes)}
	}
	return root, snapshotPath, sidecarPath, proof
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

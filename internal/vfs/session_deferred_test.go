package vfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/samekind/codexfold/internal/fold"
)

func TestDeferredOpenReadsExactStateAndRejectsMutationEntrypoints(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	serving := openFixtureSession(t, root, manifest, reader, nil)
	tail := []byte("-deferred-tail")
	writer, err := serving.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Append(context.Background(), tail); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
	leasePath := filepath.Join(directory, "writer.lease")
	if err := os.WriteFile(leasePath, []byte("stale-writer-diagnostic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotDeferredSessionTree(t, directory)

	deferred, err := OpenSession(context.Background(), deferredOptions(root, manifest, reader))
	if err != nil {
		t.Fatalf("deferred OpenSession: %v", err)
	}
	handle, err := deferred.OpenReader()
	if err != nil {
		t.Fatalf("deferred OpenReader: %v", err)
	}
	want := append(append([]byte(nil), source...), tail...)
	if got := readHandle(t, handle); !bytes.Equal(got, want) {
		t.Fatalf("deferred bytes = %q, want %q", got, want)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := deferred.OpenWriter(); !errors.Is(err, ErrSessionRecoveryDeferred) {
		t.Fatalf("OpenWriter error = %v, want ErrSessionRecoveryDeferred", err)
	}
	if err := deferred.Recover(context.Background()); !errors.Is(err, ErrSessionRecoveryDeferred) {
		t.Fatalf("Recover error = %v, want ErrSessionRecoveryDeferred", err)
	}
	if _, err := deferred.Compact(context.Background(), CompactOptions{
		Prepare: func(context.Context, NativeFile, uint64) (PreparedGeneration, error) {
			t.Fatal("deferred Compact called its preparation callback")
			return PreparedGeneration{}, nil
		},
	}); !errors.Is(err, ErrSessionRecoveryDeferred) {
		t.Fatalf("Compact error = %v, want ErrSessionRecoveryDeferred", err)
	}
	if _, _, err := OpenSessionWithWriter(context.Background(), deferredOptions(root, manifest, reader)); !errors.Is(err, ErrSessionRecoveryDeferred) {
		t.Fatalf("OpenSessionWithWriter error = %v, want ErrSessionRecoveryDeferred", err)
	}

	after := snapshotDeferredSessionTree(t, directory)
	assertDeferredSessionTreeEqual(t, before, after)
}

func TestDeferredOpenMissingStateDoesNotCreateSessionArtifacts(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	options := deferredOptions(root, manifest, reader)

	if _, err := OpenSession(context.Background(), options); !errors.Is(err, ErrSessionRecoveryDeferred) {
		t.Fatalf("OpenSession error = %v, want ErrSessionRecoveryDeferred", err)
	}
	if _, _, err := OpenSessionWithWriter(context.Background(), options); !errors.Is(err, ErrSessionRecoveryDeferred) {
		t.Fatalf("OpenSessionWithWriter error = %v, want ErrSessionRecoveryDeferred", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "fs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deferred open created virtual filesystem artifacts: %v", err)
	}
}

func TestDeferredOpenLeavesRecoverablePrimaryStateDamageUntouched(t *testing.T) {
	tests := []struct {
		name   string
		damage func(t *testing.T, statePath string, expected SessionState)
	}{
		{
			name: "missing",
			damage: func(t *testing.T, statePath string, _ SessionState) {
				if err := os.Remove(statePath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt",
			damage: func(t *testing.T, statePath string, _ SessionState) {
				if err := os.WriteFile(statePath, []byte("not-json\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "checkpoint mismatch",
			damage: func(t *testing.T, statePath string, expected SessionState) {
				diverged := expected
				diverged.Generation++
				data, err := encodeSessionState(diverged)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(statePath, append(data, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			manifest, reader, _ := sessionFixture(t, root)
			initialized := openFixtureSession(t, root, manifest, reader, nil)
			expected := initialized.State()
			directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
			statePath := filepath.Join(directory, "state.json")
			test.damage(t, statePath, expected)
			before := snapshotDeferredSessionTree(t, directory)

			if _, err := OpenSession(context.Background(), deferredOptions(root, manifest, reader)); !errors.Is(err, ErrSessionRecoveryDeferred) {
				t.Fatalf("deferred OpenSession error = %v, want ErrSessionRecoveryDeferred", err)
			}
			after := snapshotDeferredSessionTree(t, directory)
			assertDeferredSessionTreeEqual(t, before, after)

			recovered, err := OpenSession(context.Background(), sessionOptions(root, manifest, reader))
			if err != nil {
				t.Fatalf("ordinary OpenSession did not recover state: %v", err)
			}
			if recovered.State() != expected {
				t.Fatalf("recovered state = %#v, want %#v", recovered.State(), expected)
			}
		})
	}
}

func TestDeferredOpenDoesNotMigrateLegacyState(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	initialized := openFixtureSession(t, root, manifest, reader, nil)
	directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
	if err := os.Remove(filepath.Join(directory, stateCatalogFilename)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(directory, stateGenerationsDirectoryName)); err != nil {
		t.Fatal(err)
	}
	legacy := initialized.State()
	legacy.Version = legacySessionStateVersion
	legacy.ManifestSHA256 = ""
	data, err := encodeSessionState(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "manifest_sha256")
	data, err = json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(directory, "state.json")
	if err := os.WriteFile(statePath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotDeferredSessionTree(t, directory)

	if _, err := OpenSession(context.Background(), deferredOptions(root, manifest, reader)); !errors.Is(err, ErrSessionRecoveryDeferred) {
		t.Fatalf("deferred OpenSession error = %v, want ErrSessionRecoveryDeferred", err)
	}
	after := snapshotDeferredSessionTree(t, directory)
	assertDeferredSessionTreeEqual(t, before, after)

	migrated, err := OpenSession(context.Background(), sessionOptions(root, manifest, reader))
	if err != nil {
		t.Fatalf("ordinary OpenSession did not migrate legacy state: %v", err)
	}
	if migrated.State().Version != sessionStateVersion || !validStateSHA256(migrated.State().ManifestSHA256) {
		t.Fatalf("migrated state = %#v", migrated.State())
	}
}

func TestDeferredOpenDoesNotReplayPendingJournal(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	initialized := openFixtureSession(t, root, manifest, reader, nil)
	committed := initialized.State()
	directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
	candidateBytes := append(append([]byte(nil), source...), []byte("-candidate")...)
	candidatePath := filepath.Join(directory, "backing-00000000000000000002.jsonl")
	if err := os.WriteFile(candidatePath, candidateBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	verified, err := hashNativePath(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	candidate := committed
	candidate.Generation++
	candidate.BackingPath = candidatePath
	verified.Path = filepath.Join(directory, ".backing-deferredrecovery.tmp")
	if err := appendJournal(directory, JournalRecord{
		OperationID: fmt.Sprintf("cow-%020d", committed.Generation), SessionID: committed.SessionID,
		Kind: "copy-on-write", Phase: "after-file-publish", Candidate: candidate,
		FinalPath: candidatePath, Native: verified,
	}); err != nil {
		t.Fatal(err)
	}
	before := snapshotDeferredSessionTree(t, directory)

	deferred, err := OpenSession(context.Background(), deferredOptions(root, manifest, reader))
	if err != nil {
		t.Fatalf("deferred OpenSession: %v", err)
	}
	if deferred.State() != committed {
		t.Fatalf("deferred state = %#v, want committed %#v", deferred.State(), committed)
	}
	handle, err := deferred.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	if got := readHandle(t, handle); !bytes.Equal(got, source) {
		t.Fatalf("deferred read replayed pending journal: got=%q want=%q", got, source)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	after := snapshotDeferredSessionTree(t, directory)
	assertDeferredSessionTreeEqual(t, before, after)

	recovered, err := OpenSession(context.Background(), sessionOptions(root, manifest, reader))
	if err != nil {
		t.Fatalf("ordinary OpenSession did not replay journal: %v", err)
	}
	if recovered.State() != candidate {
		t.Fatalf("recovered state = %#v, want candidate %#v", recovered.State(), candidate)
	}
}

func deferredOptions(root string, manifest fold.Manifest, reader memoryReader) SessionOptions {
	options := sessionOptions(root, manifest, reader)
	options.DeferRecovery = true
	return options
}

type deferredTreeEntry struct {
	Mode fs.FileMode
	Data []byte
	Link string
}

func snapshotDeferredSessionTree(t *testing.T, directory string) map[string]deferredTreeEntry {
	t.Helper()
	snapshot := make(map[string]deferredTreeEntry)
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		if relative == "leases" || strings.HasPrefix(relative, "leases"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		record := deferredTreeEntry{Mode: info.Mode()}
		switch {
		case info.Mode().IsRegular():
			record.Data, err = os.ReadFile(path)
		case info.Mode()&os.ModeSymlink != 0:
			record.Link, err = os.Readlink(path)
		}
		if err != nil {
			return err
		}
		snapshot[relative] = record
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return snapshot
	}
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertDeferredSessionTreeEqual(t *testing.T, before map[string]deferredTreeEntry, after map[string]deferredTreeEntry) {
	t.Helper()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("deferred operation changed session artifacts:\nbefore=%#v\nafter=%#v", before, after)
	}
}

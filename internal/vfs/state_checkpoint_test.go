package vfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samekind/codexfold/internal/fold"
)

func TestSessionStateRecoversFromCheckpoint(t *testing.T) {
	for _, damage := range []struct {
		name string
		do   func(t *testing.T, path string)
	}{
		{name: "missing", do: func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "corrupt", do: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("not-json\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(damage.name, func(t *testing.T) {
			root := t.TempDir()
			manifest, reader, _ := sessionFixture(t, root)
			session := openFixtureSession(t, root, manifest, reader, nil)
			expected := session.State()
			statePath := filepath.Join(root, "fs", "sessions", manifest.Session.ID, "state.json")
			damage.do(t, statePath)

			recovered, err := LoadSessionState(statePath)
			if err != nil {
				t.Fatalf("LoadSessionState: %v", err)
			}
			if recovered != expected {
				t.Fatalf("recovered state = %#v, want %#v", recovered, expected)
			}
		})
	}
}

func TestCheckpointRefreshRecoversDurableAppend(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	session, writer, err := OpenSessionWithWriter(context.Background(), sessionOptions(root, manifest, reader))
	if err != nil {
		t.Fatal(err)
	}
	tail := []byte("-checkpointed-tail")
	if _, err := writer.Append(context.Background(), tail); err != nil {
		t.Fatal(err)
	}
	if err := writer.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	statePath := filepath.Join(root, "fs", "sessions", manifest.Session.ID, "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSession(context.Background(), sessionOptions(root, manifest, reader))
	if err != nil {
		t.Fatalf("OpenSession after state loss: %v", err)
	}
	handle, err := reopened.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	want := append(append([]byte(nil), source...), tail...)
	if got := readHandle(t, handle); !bytes.Equal(got, want) {
		t.Fatalf("recovered bytes = %q, want %q", got, want)
	}
	if reopened.State().Generation != session.State().Generation {
		t.Fatalf("recovered generation = %d, want %d", reopened.State().Generation, session.State().Generation)
	}
}

func TestOpenSessionRefreshesStaleArtifactCheckpointUnderRecoveryLease(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	tail := []byte("-synced-before-checkpoint-failure")
	file, err := os.OpenFile(session.State().DeltaPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(tail); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	refreshed, err := OpenSession(context.Background(), sessionOptions(root, manifest, reader))
	if err != nil {
		t.Fatalf("OpenSession with stale artifact checkpoint: %v", err)
	}
	statePath := filepath.Join(root, "fs", "sessions", manifest.Session.ID, "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	recovered, err := OpenSession(context.Background(), sessionOptions(root, manifest, reader))
	if err != nil {
		t.Fatalf("OpenSession after refreshed state loss: %v", err)
	}
	handle, err := recovered.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	want := append(append([]byte(nil), source...), tail...)
	if got := readHandle(t, handle); !bytes.Equal(got, want) {
		t.Fatalf("recovered bytes = %q, want %q", got, want)
	}
	if refreshed.State() != recovered.State() {
		t.Fatalf("state changed during artifact checkpoint recovery: refreshed=%#v recovered=%#v", refreshed.State(), recovered.State())
	}
}

func TestCheckpointRefreshRecoversDurableBackingMutation(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	_, writer, err := OpenSessionWithWriter(context.Background(), sessionOptions(root, manifest, reader))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteAt(context.Background(), []byte("XYZ"), 2); err != nil {
		t.Fatal(err)
	}
	newSize := int64(len(source) - 3)
	if err := writer.Truncate(context.Background(), newSize); err != nil {
		t.Fatal(err)
	}
	if err := writer.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	statePath := filepath.Join(root, "fs", "sessions", manifest.Session.ID, "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSession(context.Background(), sessionOptions(root, manifest, reader))
	if err != nil {
		t.Fatalf("OpenSession after state loss: %v", err)
	}
	handle, err := reopened.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	want := append([]byte(nil), source[:newSize]...)
	copy(want[2:], []byte("XYZ"))
	if got := readHandle(t, handle); !bytes.Equal(got, want) {
		t.Fatalf("recovered bytes = %q, want %q", got, want)
	}
}

func TestRepeatedSyncDoesNotRehashAppendOnlyData(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	var hashed int64
	stateCheckpointBytesHashed = func(bytes int64) { hashed += bytes }
	t.Cleanup(func() { stateCheckpointBytesHashed = nil })
	first := bytes.Repeat([]byte("x"), 1<<20)
	if _, err := writer.Append(context.Background(), first); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Sync(); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	afterFirstSync := hashed
	if err := writer.Sync(); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if hashed != afterFirstSync {
		t.Fatalf("repeated Sync rehashed %d additional bytes", hashed-afterFirstSync)
	}
	checkpointDirectory := filepath.Join(root, "fs", "sessions", manifest.Session.ID, stateGenerationsDirectoryName)
	for iteration := 0; iteration < 16; iteration++ {
		chunk := bytes.Repeat([]byte{byte('a' + iteration)}, 128<<10)
		if _, err := writer.Append(context.Background(), chunk); err != nil {
			_ = writer.Close()
			t.Fatal(err)
		}
		if err := writer.Sync(); err != nil {
			_ = writer.Close()
			t.Fatal(err)
		}
		if hashed != afterFirstSync {
			t.Fatalf("append-only checkpoint refresh %d rehashed %d bytes", iteration+1, hashed-afterFirstSync)
		}
		entries, err := os.ReadDir(checkpointDirectory)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("append-only checkpoint refresh %d left %d live leaves, want 1", iteration+1, len(entries))
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStateRecoveryRejectsTamperedArtifacts(t *testing.T) {
	for _, test := range []struct {
		name   string
		tamper func(t *testing.T, root string, manifest fold.Manifest, state SessionState)
	}{
		{name: "delta", tamper: func(t *testing.T, _ string, _ fold.Manifest, state SessionState) {
			if err := os.WriteFile(state.DeltaPath, []byte("unknown durable bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "manifest metadata", tamper: func(t *testing.T, root string, manifest fold.Manifest, _ SessionState) {
			manifest.Session.Title = "metadata changed after checkpoint"
			persistManifestFixture(t, fold.ManifestPath(root, manifest.Session.ID), manifest)
		}},
		{name: "native snapshot", tamper: func(t *testing.T, _ string, manifest fold.Manifest, _ SessionState) {
			if err := os.WriteFile(manifest.Session.RolloutPath, []byte("tampered native"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			manifest, reader, _ := sessionFixture(t, root)
			session := openFixtureSession(t, root, manifest, reader, nil)
			statePath := filepath.Join(root, "fs", "sessions", manifest.Session.ID, "state.json")
			test.tamper(t, root, manifest, session.State())
			if err := os.Remove(statePath); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadSessionState(statePath); err == nil {
				t.Fatal("state recovery accepted tampered checkpoint artifacts")
			}
			if _, err := os.Lstat(statePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed recovery republished state: %v", err)
			}
		})
	}
}

func TestStateRecoveryRejectsAmbiguousCheckpointHistory(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
	chain, err := loadStateCheckpointChain(directory, manifest.Session.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	stateData, err := encodeSessionState(session.State())
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := buildStateCheckpoint(session.State(), digestStateBytes(stateData), chain.catalog.CheckpointSHA256, chain.catalog.Sequence+1, directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	orphanData, err := encodeStateCheckpoint(orphan)
	if err != nil {
		t.Fatal(err)
	}
	orphanDigest := digestStateBytes(orphanData)
	if err := writeImmutableStateCheckpoint(directory, stateCheckpointFilename(orphan.State.Generation, orphan.Sequence, orphanDigest), orphanData); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(directory, "state.json")
	if _, err := LoadSessionState(statePath); err != nil {
		t.Fatalf("healthy primary state was blocked by an uncommitted orphan: %v", err)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionState(statePath); err == nil {
		t.Fatal("state recovery accepted an ambiguous checkpoint branch")
	}
}

func TestCheckpointRefreshReconcilesCatalogPublicationFailure(t *testing.T) {
	root := t.TempDir()
	manifest, reader, source := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
	tail := []byte("-catalog-publication-retry")
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Append(context.Background(), tail); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	chain, err := loadStateCheckpointChain(directory, manifest.Session.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	stateData, err := encodeSessionState(session.State())
	if err != nil {
		t.Fatal(err)
	}
	active, err := captureRegularFileIdentity(session.State().DeltaPath)
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := buildRefreshedStateCheckpoint(session.State(), digestStateBytes(stateData), chain.checkpoint.PreviousCheckpointSHA256, chain.catalog.Sequence+1, directory, chain.checkpoint, &active)
	if err != nil {
		t.Fatal(err)
	}
	orphanData, err := encodeStateCheckpoint(orphan)
	if err != nil {
		t.Fatal(err)
	}
	orphanDigest := digestStateBytes(orphanData)
	orphanName := stateCheckpointFilename(orphan.State.Generation, orphan.Sequence, orphanDigest)
	if err := writeImmutableStateCheckpoint(directory, orphanName, orphanData); err != nil {
		t.Fatal(err)
	}

	if err := writer.Sync(); err != nil {
		_ = writer.Close()
		t.Fatalf("retry Sync: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	retriedCatalog, err := loadStateCatalog(directory)
	if err != nil {
		t.Fatal(err)
	}
	if retriedCatalog.Checkpoint != orphanName {
		t.Fatalf("retry catalog target = %q, want exact orphan %q", retriedCatalog.Checkpoint, orphanName)
	}
	statePath := filepath.Join(directory, "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	recovered, err := OpenSession(context.Background(), sessionOptions(root, manifest, reader))
	if err != nil {
		t.Fatalf("OpenSession after reconciled state loss: %v", err)
	}
	handle, err := recovered.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	want := append(append([]byte(nil), source...), tail...)
	if got := readHandle(t, handle); !bytes.Equal(got, want) {
		t.Fatalf("recovered bytes = %q, want %q", got, want)
	}
}

func TestWriterLeaseRecoveryCompletesInterruptedPinnedCheckpointPublication(t *testing.T) {
	previousCandidateSynced := stateCheckpointCandidateSynced
	t.Cleanup(func() { stateCheckpointCandidateSynced = previousCandidateSynced })
	for _, test := range []struct {
		name      string
		linkFinal bool
	}{
		{name: "temporary synced before link"},
		{name: "final linked before catalog", linkFinal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			syncs := 0
			stateCheckpointCandidateSynced = func(string) { syncs++ }
			root := t.TempDir()
			manifest, reader, _ := sessionFixture(t, root)
			session := openFixtureSession(t, root, manifest, reader, nil)
			directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
			statePath := filepath.Join(directory, "state.json")
			chain, err := loadStateCheckpointChain(directory, manifest.Session.ID, true)
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

			tail := []byte("-append-synced-before-checkpoint-publication-crash")
			active, err := os.OpenFile(session.State().DeltaPath, os.O_APPEND|os.O_WRONLY, 0)
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
			activeIdentity, err := captureRegularFileIdentity(session.State().DeltaPath)
			if err != nil {
				t.Fatal(err)
			}
			stateData, err := encodeSessionState(session.State())
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err := buildRefreshedStateCheckpoint(
				session.State(), digestStateBytes(stateData), chain.catalog.CheckpointSHA256,
				chain.catalog.Sequence+1, directory, chain.checkpoint, &activeIdentity,
			)
			if err != nil {
				t.Fatal(err)
			}
			checkpointData, err := encodeStateCheckpoint(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			checkpointSHA256 := digestStateBytes(checkpointData)
			checkpointName := stateCheckpointFilename(checkpoint.State.Generation, checkpoint.Sequence, checkpointSHA256)
			generations := filepath.Join(directory, stateGenerationsDirectoryName)
			temporary, err := os.CreateTemp(generations, ".state-checkpoint-*.tmp")
			if err != nil {
				t.Fatal(err)
			}
			temporaryPath := temporary.Name()
			if _, err := temporary.Write(checkpointData); err != nil {
				_ = temporary.Close()
				t.Fatal(err)
			}
			if err := temporary.Sync(); err != nil {
				_ = temporary.Close()
				t.Fatal(err)
			}
			if err := temporary.Close(); err != nil {
				t.Fatal(err)
			}
			if test.linkFinal {
				if err := os.Link(temporaryPath, filepath.Join(generations, checkpointName)); err != nil {
					t.Fatal(err)
				}
				if err := syncStateDirectory(generations); err != nil {
					t.Fatal(err)
				}
			}

			guard, acquired, err := TryAcquireWriterLeaseGuardAtPath(filepath.Join(directory, "writer.lease"))
			if err != nil || !acquired {
				t.Fatalf("acquire writer guard: acquired=%v err=%v", acquired, err)
			}
			defer guard.Close()
			recovered, err := LoadSessionStateWithWriterLease(statePath)
			if err != nil {
				t.Fatalf("LoadSessionStateWithWriterLease: %v", err)
			}
			if recovered != session.State() {
				t.Fatalf("recovered state = %#v, want %#v", recovered, session.State())
			}
			if syncs != 1 {
				t.Fatalf("checkpoint candidate sync count = %d, want 1", syncs)
			}
			recoveredChain, err := loadStateCheckpointChain(directory, manifest.Session.ID, true)
			if err != nil {
				t.Fatalf("load recovered checkpoint lineage: %v", err)
			}
			if recoveredChain.catalog.Checkpoint != checkpointName || recoveredChain.catalog.CheckpointSHA256 != checkpointSHA256 || recoveredChain.catalog.Sequence != chain.catalog.Sequence+1 {
				t.Fatalf("recovered catalog = %#v, want checkpoint %q", recoveredChain.catalog, checkpointName)
			}
			if _, pinned := recoveredChain.lineage[chain.catalog.CheckpointSHA256]; !pinned {
				t.Fatal("retirement request checkpoint was not retained in recovered lineage")
			}
			if err := verifyStateCheckpointArtifacts(directory, recoveredChain.checkpoint); err != nil {
				t.Fatalf("verify recovered checkpoint artifacts: %v", err)
			}
			if _, err := os.Lstat(temporaryPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("interrupted checkpoint temporary remains: %v", err)
			}
			visible, err := os.ReadFile(session.State().DeltaPath)
			if err != nil || !bytes.HasSuffix(visible, tail) {
				t.Fatalf("durable append changed during checkpoint recovery: %q err=%v", visible, err)
			}
		})
	}
}

func TestWriterLeaseRecoveryPreservesUnprovenPartialCheckpoint(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
	chain, err := loadStateCheckpointChain(directory, manifest.Session.ID, true)
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

	tail := []byte("-durable-but-not-checkpoint-proven")
	active, err := os.OpenFile(session.State().DeltaPath, os.O_APPEND|os.O_WRONLY, 0)
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
	temporary := filepath.Join(directory, stateGenerationsDirectoryName, ".state-checkpoint-partial.tmp")
	partial := []byte("{\n  \"version\": 1,\n  \"session_id\": \"session\"")
	if err := os.WriteFile(temporary, partial, 0o600); err != nil {
		t.Fatal(err)
	}

	guard, acquired, err := TryAcquireWriterLeaseGuardAtPath(filepath.Join(directory, "writer.lease"))
	if err != nil || !acquired {
		t.Fatalf("acquire writer guard: acquired=%v err=%v", acquired, err)
	}
	defer guard.Close()
	if _, err := LoadSessionStateWithWriterLease(filepath.Join(directory, "state.json")); err == nil || !strings.Contains(err.Error(), "no complete direct-child checkpoint proof") {
		t.Fatalf("partial checkpoint recovery error = %v", err)
	}
	actualPartial, err := os.ReadFile(temporary)
	if err != nil || !bytes.Equal(actualPartial, partial) {
		t.Fatalf("partial checkpoint evidence changed: %q err=%v", actualPartial, err)
	}
	unchangedCatalog, err := loadStateCatalog(directory)
	if err != nil {
		t.Fatal(err)
	}
	if unchangedCatalog != chain.catalog {
		t.Fatalf("unproven checkpoint changed catalog: got=%#v want=%#v", unchangedCatalog, chain.catalog)
	}
	visible, err := os.ReadFile(session.State().DeltaPath)
	if err != nil || !bytes.HasSuffix(visible, tail) {
		t.Fatalf("unproven durable bytes were not preserved: %q err=%v", visible, err)
	}
}

func TestWriterLeaseRecoveryAdoptsPinnedGenerationSuccessorCheckpoint(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
	chain, err := loadStateCheckpointChain(directory, manifest.Session.ID, true)
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
	successor := session.State()
	successor.Generation++
	stateData, err := encodeSessionState(successor)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := buildStateCheckpoint(successor, digestStateBytes(stateData), chain.catalog.CheckpointSHA256, chain.catalog.Sequence+1, directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkpointData, err := encodeStateCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	checkpointSHA256 := digestStateBytes(checkpointData)
	checkpointName := stateCheckpointFilename(successor.Generation, checkpoint.Sequence, checkpointSHA256)
	generations := filepath.Join(directory, stateGenerationsDirectoryName)
	temporary, err := os.CreateTemp(generations, ".state-checkpoint-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	temporaryPath := temporary.Name()
	if _, err := temporary.Write(checkpointData); err != nil {
		_ = temporary.Close()
		t.Fatal(err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		t.Fatal(err)
	}
	if err := temporary.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(temporaryPath, filepath.Join(generations, checkpointName)); err != nil {
		t.Fatal(err)
	}
	if err := syncStateDirectory(generations); err != nil {
		t.Fatal(err)
	}

	guard, acquired, err := TryAcquireWriterLeaseGuardAtPath(filepath.Join(directory, "writer.lease"))
	if err != nil || !acquired {
		t.Fatalf("acquire writer guard: acquired=%v err=%v", acquired, err)
	}
	defer guard.Close()
	recovered, err := LoadSessionStateWithWriterLease(filepath.Join(directory, "state.json"))
	if err != nil {
		t.Fatalf("LoadSessionStateWithWriterLease: %v", err)
	}
	if recovered != successor {
		t.Fatalf("recovered state = %#v, want %#v", recovered, successor)
	}
	recoveredChain, err := loadStateCheckpointChain(directory, manifest.Session.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredChain.catalog.Checkpoint != checkpointName || recoveredChain.catalog.Generation != successor.Generation || recoveredChain.catalog.Sequence != chain.catalog.Sequence+1 {
		t.Fatalf("recovered catalog = %#v, want checkpoint %q", recoveredChain.catalog, checkpointName)
	}
	if _, pinned := recoveredChain.lineage[chain.catalog.CheckpointSHA256]; !pinned {
		t.Fatal("retirement request checkpoint was not retained after generation successor adoption")
	}
	if _, err := os.Lstat(temporaryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generation successor checkpoint temporary remains: %v", err)
	}
}

func TestWriterLeaseRecoveryCleansPrimaryStateTemporaryAfterCatalogAdoption(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
	chain, err := loadStateCheckpointChain(directory, manifest.Session.ID, true)
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
	successor := session.State()
	successor.Generation++
	stateData, err := encodeSessionState(successor)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := buildStateCheckpoint(successor, digestStateBytes(stateData), chain.catalog.CheckpointSHA256, chain.catalog.Sequence+1, directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkpointData, err := encodeStateCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	checkpointSHA256 := digestStateBytes(checkpointData)
	checkpointName := stateCheckpointFilename(successor.Generation, checkpoint.Sequence, checkpointSHA256)
	if err := writeImmutableStateCheckpoint(directory, checkpointName, checkpointData); err != nil {
		t.Fatal(err)
	}
	if err := writeStateCatalog(directory, sessionStateCatalog{
		Version: sessionStateCatalogVersion, SessionID: manifest.Session.ID,
		Generation: successor.Generation, Sequence: checkpoint.Sequence, StateSHA256: checkpoint.StateSHA256,
		Checkpoint: checkpointName, CheckpointSHA256: checkpointSHA256,
	}); err != nil {
		t.Fatal(err)
	}
	temporary, err := os.CreateTemp(directory, ".state-primary-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	temporaryPath := temporary.Name()
	if _, err := temporary.Write(stateData); err != nil {
		_ = temporary.Close()
		t.Fatal(err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		t.Fatal(err)
	}
	if err := temporary.Close(); err != nil {
		t.Fatal(err)
	}

	guard, acquired, err := TryAcquireWriterLeaseGuardAtPath(filepath.Join(directory, "writer.lease"))
	if err != nil || !acquired {
		t.Fatalf("acquire writer guard: acquired=%v err=%v", acquired, err)
	}
	defer guard.Close()
	recovered, err := LoadSessionStateWithWriterLease(filepath.Join(directory, "state.json"))
	if err != nil {
		t.Fatalf("LoadSessionStateWithWriterLease: %v", err)
	}
	if recovered != successor {
		t.Fatalf("recovered state = %#v, want %#v", recovered, successor)
	}
	if _, err := os.Lstat(temporaryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("superseded primary state temporary remains: %v", err)
	}
}

func TestStateRecoveryPreservesUnknownNonemptyDelta(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	openFixtureSession(t, root, manifest, reader, nil)
	directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
	unknown := filepath.Join(directory, "delta-99999999999999999999.jsonl")
	contents := []byte("unrecognized bytes must be preserved")
	if err := os.WriteFile(unknown, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(directory, "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionState(statePath); err == nil {
		t.Fatal("state recovery accepted an unknown nonempty delta")
	}
	got, err := os.ReadFile(unknown)
	if err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("unknown delta changed: got=%q err=%v", got, err)
	}
}

func TestHealthyStateIgnoresPartialCheckpointPublication(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
	temporary := filepath.Join(directory, stateGenerationsDirectoryName, ".state-checkpoint-crashed.tmp")
	if err := os.WriteFile(temporary, []byte("partial checkpoint"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSessionState(filepath.Join(directory, "state.json"))
	if err != nil {
		t.Fatalf("LoadSessionState: %v", err)
	}
	if loaded != session.State() {
		t.Fatalf("loaded state = %#v, want %#v", loaded, session.State())
	}
}

func TestInitialPublicationCleanupAcceptsOnlyCompleteCheckpointMetadata(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	parent := filepath.Join(root, "fs", "sessions")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	finalDirectory := filepath.Join(parent, manifest.Session.ID)
	staging, err := os.MkdirTemp(parent, initialSessionStagingNamePrefix(manifest.Session.ID)+"*")
	if err != nil {
		t.Fatal(err)
	}
	stagedDelta := filepath.Join(staging, "delta.jsonl")
	if err := os.WriteFile(stagedDelta, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestIdentity, _, err := captureManifestIdentity(fold.ManifestPath(root, manifest.Session.ID), manifest.Session.ID, manifest.Source.Bytes, manifest.Source.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	state := SessionState{
		Version: sessionStateVersion, SessionID: manifest.Session.ID, Generation: 1,
		ManifestPath: fold.ManifestPath(root, manifest.Session.ID), ManifestSHA256: manifestIdentity.SHA256,
		BaseBytes: manifest.Source.Bytes, BaseSHA256: manifest.Source.SHA256,
		DeltaPath:      filepath.Join(finalDirectory, "delta.jsonl"),
		NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
	}
	if err := publishInitialSessionState(filepath.Join(staging, "state.json"), state, finalDirectory, stagedDelta); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSession(context.Background(), sessionOptions(root, manifest, reader)); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if _, err := os.Lstat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("complete abandoned staging was not removed: %v", err)
	}
}

func TestStateRecoveryRejectsOldCheckpointSchema(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	openFixtureSession(t, root, manifest, reader, nil)
	directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
	catalog, err := loadStateCatalog(directory)
	if err != nil {
		t.Fatal(err)
	}
	checkpointPath := filepath.Join(directory, stateGenerationsDirectoryName, catalog.Checkpoint)
	data, err := os.ReadFile(checkpointPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["version"] = float64(0)
	oldData, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checkpointPath, append(oldData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(directory, "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionState(statePath); err == nil {
		t.Fatal("state recovery accepted an old checkpoint schema")
	}
}

func TestMetadataOnlyManifestUpdateIsNotOverwrittenByOldCheckpoint(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	openFixtureSession(t, root, manifest, reader, nil)
	manifest.Session.Title = "new metadata"
	manifestPath := fold.ManifestPath(root, manifest.Session.ID)
	persistManifestFixture(t, manifestPath, manifest)
	expectedManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "fs", "sessions", manifest.Session.ID, "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionState(statePath); err == nil {
		t.Fatal("state recovery accepted a stale manifest identity")
	}
	actualManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actualManifest, expectedManifest) {
		t.Fatal("failed state recovery changed the current manifest")
	}
}

func TestLegacyStateMigratesAndRecovers(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
	if err := os.Remove(filepath.Join(directory, stateCatalogFilename)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(directory, stateGenerationsDirectoryName)); err != nil {
		t.Fatal(err)
	}
	legacy := session.State()
	legacy.Version = legacySessionStateVersion
	legacy.ManifestSHA256 = ""
	statePath := filepath.Join(directory, "state.json")
	legacyData, err := encodeSessionState(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var legacyJSON map[string]json.RawMessage
	if err := json.Unmarshal(legacyData, &legacyJSON); err != nil {
		t.Fatal(err)
	}
	delete(legacyJSON, "manifest_sha256")
	legacyData, err = json.MarshalIndent(legacyJSON, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, append(legacyData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	migrated, err := LoadSessionState(statePath)
	if err != nil {
		t.Fatalf("migrate legacy state: %v", err)
	}
	if migrated.Version != sessionStateVersion || !validStateSHA256(migrated.ManifestSHA256) {
		t.Fatalf("migrated state = %#v", migrated)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	recovered, err := LoadSessionState(statePath)
	if err != nil {
		t.Fatalf("recover migrated state: %v", err)
	}
	if recovered != migrated {
		t.Fatalf("recovered state = %#v, want %#v", recovered, migrated)
	}
}

func TestLegacyMigrationAdoptsExactOrphanCheckpoint(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	directory := filepath.Join(root, "fs", "sessions", manifest.Session.ID)
	if err := os.Remove(filepath.Join(directory, stateCatalogFilename)); err != nil {
		t.Fatal(err)
	}
	legacy := session.State()
	legacy.Version = legacySessionStateVersion
	legacy.ManifestSHA256 = ""
	legacyData, err := encodeSessionState(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(legacyData, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "manifest_sha256")
	legacyData, err = json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(directory, "state.json")
	if err := os.WriteFile(statePath, append(legacyData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	migrated, err := LoadSessionState(statePath)
	if err != nil {
		t.Fatalf("migrate with exact orphan checkpoint: %v", err)
	}
	if migrated != session.State() {
		t.Fatalf("migrated state = %#v, want %#v", migrated, session.State())
	}
}

func TestDiscoveryDoesNotRequireCurrentManifestWhenPrimaryStateIsHealthy(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	openFixtureSession(t, root, manifest, reader, nil)
	if err := os.Remove(fold.ManifestPath(root, manifest.Session.ID)); err != nil {
		t.Fatal(err)
	}
	states, issues, err := DiscoverSessionStatesDetailed(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 || len(states) != 1 || states[0].SessionID != manifest.Session.ID {
		t.Fatalf("states=%#v issues=%#v", states, issues)
	}
}

func sessionOptions(root string, manifest fold.Manifest, reader memoryReader) SessionOptions {
	return SessionOptions{
		Root: root, ManifestPath: fold.ManifestPath(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
		NativeSnapshot: NativeFile{Path: manifest.Session.RolloutPath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
	}
}

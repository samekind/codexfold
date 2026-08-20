package vfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSessionDeletionPublishesExactProofAndPurgesData(t *testing.T) {
	root, session, tombstone := sessionDeletionFixture(t)

	loaded, err := LoadSessionDeletion(root, session.State().SessionID)
	if err != nil {
		t.Fatalf("LoadSessionDeletion: %v", err)
	}
	if loaded != tombstone {
		t.Fatalf("loaded tombstone differs:\n got=%#v\nwant=%#v", loaded, tombstone)
	}
	completed, err := AdvanceSessionDeletion(root, tombstone)
	if err != nil || !completed {
		t.Fatalf("AdvanceSessionDeletion completed=%t err=%v", completed, err)
	}
	for _, source := range []string{tombstone.SessionPath, tombstone.ManifestPath} {
		if _, err := os.Lstat(source); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deletion source remains at %s: %v", source, err)
		}
	}
	assertSessionDeletionPurged(t, root, tombstone)
	if completed, err := AdvanceSessionDeletion(root, tombstone); err != nil || !completed {
		t.Fatalf("idempotent replay completed=%t err=%v", completed, err)
	}
}

func TestSessionDeletionDefersForWriterAndReaderLeases(t *testing.T) {
	root, session, tombstone := sessionDeletionFixture(t)

	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || !errors.Is(err, ErrSessionDeletionBusy) {
		t.Fatalf("writer replay completed=%t err=%v", completed, err)
	}
	assertDeletionSourcesPresent(t, tombstone)
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	reader, err := session.OpenReader()
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || !errors.Is(err, ErrSessionDeletionBusy) {
		t.Fatalf("reader replay completed=%t err=%v", completed, err)
	}
	assertDeletionSourcesPresent(t, tombstone)
	if err := reader.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}
	if completed, err := AdvanceSessionDeletion(root, tombstone); err != nil || !completed {
		t.Fatalf("replay after leases completed=%t err=%v", completed, err)
	}
}

func TestSessionDeletionRefreshesProofAfterOpenWriterFinishes(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	hiddenSnapshot := filepath.Join(root, "fs", "snapshots", manifest.Session.ID, "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(hiddenSnapshot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(manifest.Session.RolloutPath, hiddenSnapshot); err != nil {
		t.Fatal(err)
	}
	session, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: manifestPathForDeletionTest(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
		NativeSnapshot: NativeFile{Path: hiddenSnapshot, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
	})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	tombstone, err := PublishSessionDeletion(root, session.State(), "/sessions/2026/07/25/session.jsonl")
	if err != nil {
		t.Fatalf("PublishSessionDeletion: %v", err)
	}
	if _, err := writer.WriteAt(context.Background(), []byte("X"), 0); err != nil {
		t.Fatalf("write after deletion publication: %v", err)
	}
	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || !errors.Is(err, ErrSessionDeletionBusy) {
		t.Fatalf("writer replay completed=%t err=%v", completed, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	stop := errors.New("stop after refreshed deletion proof")
	sessionDeletionPurgeHook = func(phase string) error {
		if phase == sessionDeletionPurgePrepared {
			return stop
		}
		return nil
	}
	t.Cleanup(func() { sessionDeletionPurgeHook = nil })
	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || !errors.Is(err, stop) {
		t.Fatalf("refreshed deletion proof completed=%t err=%v", completed, err)
	}
	updated, err := LoadSessionDeletion(root, tombstone.SessionID)
	if err != nil {
		t.Fatalf("load refreshed tombstone: %v", err)
	}
	if updated.InitialStateGeneration != tombstone.InitialStateGeneration || updated.InitialStateSHA256 != tombstone.InitialStateSHA256 {
		t.Fatalf("initial deletion proof changed: before=%#v after=%#v", tombstone, updated)
	}
	if updated.InitialCheckpointSequence != tombstone.InitialCheckpointSequence || updated.InitialCheckpointSHA256 != tombstone.InitialCheckpointSHA256 {
		t.Fatalf("initial checkpoint pin changed: before=%#v after=%#v", tombstone, updated)
	}
	if updated.StateGeneration < tombstone.StateGeneration {
		t.Fatalf("current deletion generation moved backwards: before=%d after=%d", tombstone.StateGeneration, updated.StateGeneration)
	}
	currentState, currentData, err := readSessionState(filepath.Join(updated.RetiredSessionPath, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	chain, err := verifyDeletionCheckpointDescendant(updated.RetiredSessionPath, updated, currentState, digestDeletionBytes(currentData), false)
	if err != nil {
		t.Fatalf("verify refreshed deletion checkpoint lineage: %v", err)
	}
	if _, exists := chain.lineage[tombstone.InitialCheckpointSHA256]; !exists {
		t.Fatal("refreshed deletion lineage lost its exact initial checkpoint pin")
	}
	if chain.catalog.CheckpointSHA256 == tombstone.InitialCheckpointSHA256 {
		t.Fatal("open writer did not publish a descendant checkpoint")
	}
	sessionDeletionPurgeHook = nil
	if completed, err := AdvanceSessionDeletion(root, updated); err != nil || !completed {
		t.Fatalf("replay after refreshed proof completed=%t err=%v", completed, err)
	}
	if _, err := os.Lstat(updated.SessionPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted session state revived: %v", err)
	}
}

func TestSessionDeletionPublicationSerializesCheckpointTransition(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	hiddenSnapshot := filepath.Join(root, "fs", "snapshots", manifest.Session.ID, "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(hiddenSnapshot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(manifest.Session.RolloutPath, hiddenSnapshot); err != nil {
		t.Fatal(err)
	}
	session, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: manifestPathForDeletionTest(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
		NativeSnapshot: NativeFile{Path: hiddenSnapshot, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
	})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })

	captured := make(chan struct{})
	release := make(chan struct{})
	sessionDeletionPublicationHook = func(phase string) error {
		if phase == "checkpoint-captured" {
			close(captured)
			<-release
		}
		return nil
	}
	t.Cleanup(func() { sessionDeletionPublicationHook = nil })
	type publicationResult struct {
		tombstone SessionDeletion
		err       error
	}
	published := make(chan publicationResult, 1)
	go func() {
		tombstone, err := PublishSessionDeletion(root, session.State(), "/sessions/2026/07/25/session.jsonl")
		published <- publicationResult{tombstone: tombstone, err: err}
	}()
	<-captured

	written := make(chan error, 1)
	go func() {
		_, err := writer.WriteAt(context.Background(), []byte("Z"), 0)
		written <- err
	}()
	select {
	case err := <-written:
		t.Fatalf("writer checkpoint transition escaped deletion publication lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	result := <-published
	if result.err != nil {
		t.Fatalf("PublishSessionDeletion: %v", result.err)
	}
	if err := <-written; err != nil {
		t.Fatalf("writer transition after deletion publication: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	updated, err := LoadSessionDeletion(root, result.tombstone.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.InitialCheckpointSHA256 != result.tombstone.InitialCheckpointSHA256 || updated.InitialCheckpointSequence != result.tombstone.InitialCheckpointSequence {
		t.Fatalf("publication lock changed initial pin: before=%#v after=%#v", result.tombstone, updated)
	}
	state, data, err := readSessionState(filepath.Join(updated.SessionPath, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	chain, err := verifyDeletionCheckpointDescendant(updated.SessionPath, updated, state, digestDeletionBytes(data), true)
	if err != nil {
		t.Fatalf("verify serialized checkpoint descendant: %v", err)
	}
	if _, exists := chain.lineage[updated.InitialCheckpointSHA256]; !exists || chain.catalog.CheckpointSHA256 == updated.InitialCheckpointSHA256 {
		t.Fatalf("serialized writer transition lost or failed to descend from pin: %#v", chain.catalog)
	}
	if completed, err := AdvanceSessionDeletion(root, updated); err != nil || !completed {
		t.Fatalf("advance serialized deletion completed=%t err=%v", completed, err)
	}
}

func TestSessionDeletionRestoresManifestFirstCrashBeforeReplayingCompletedCOWJournal(t *testing.T) {
	root, session, tombstone := sessionDeletionFixture(t)
	writer, err := session.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteAt(context.Background(), []byte("Z"), 0); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(tombstone.RetiredManifestPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tombstone.ManifestPath, tombstone.RetiredManifestPath); err != nil {
		t.Fatal(err)
	}

	if completed, err := AdvanceSessionDeletion(root, tombstone); err != nil || !completed {
		t.Fatalf("manifest-first COW replay completed=%t err=%v", completed, err)
	}
	assertSessionDeletionPurged(t, root, tombstone)
}

func TestSessionDeletionReplayRecoversEveryRenamePhase(t *testing.T) {
	for _, test := range []struct {
		name         string
		moveSession  bool
		moveSnapshot bool
		moveManifest bool
	}{
		{name: "published-only"},
		{name: "session-renamed", moveSession: true},
		{name: "session-and-snapshot-renamed", moveSession: true, moveSnapshot: true},
		{name: "manifest-renamed", moveManifest: true},
		{name: "both-renamed", moveSession: true, moveManifest: true},
		{name: "all-renamed", moveSession: true, moveSnapshot: true, moveManifest: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, _, tombstone := sessionDeletionFixture(t)
			if err := os.MkdirAll(filepath.Dir(tombstone.RetiredSessionPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if test.moveSession {
				if err := os.Rename(tombstone.SessionPath, tombstone.RetiredSessionPath); err != nil {
					t.Fatalf("stage session rename: %v", err)
				}
			}
			if test.moveSnapshot {
				target := filepath.Join(tombstone.RetiredSessionPath, "retained-native", "store-snapshot")
				if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(root, "fs", "snapshots", tombstone.SessionID), target); err != nil {
					t.Fatalf("stage native snapshot rename: %v", err)
				}
			}
			if test.moveManifest {
				if err := os.Rename(tombstone.ManifestPath, tombstone.RetiredManifestPath); err != nil {
					t.Fatalf("stage manifest rename: %v", err)
				}
			}
			completed, err := AdvanceSessionDeletion(root, tombstone)
			if err != nil || !completed {
				t.Fatalf("replay completed=%t err=%v", completed, err)
			}
			assertSessionDeletionPurged(t, root, tombstone)
		})
	}
}

func TestSessionDeletionPurgeReplaysEveryDurablePhase(t *testing.T) {
	for _, phase := range []string{sessionDeletionPurgePrepared, sessionDeletionPurgeRenamed, "removed"} {
		t.Run(phase, func(t *testing.T) {
			root, _, tombstone := sessionDeletionFixture(t)
			stop := errors.New("stop deletion purge")
			stopped := false
			sessionDeletionPurgeHook = func(current string) error {
				if current == phase && !stopped {
					stopped = true
					return stop
				}
				return nil
			}
			t.Cleanup(func() { sessionDeletionPurgeHook = nil })

			if completed, err := AdvanceSessionDeletion(root, tombstone); completed || !errors.Is(err, stop) {
				t.Fatalf("interrupted purge completed=%t err=%v", completed, err)
			}
			sessionDeletionPurgeHook = nil
			if completed, err := AdvanceSessionDeletion(root, tombstone); err != nil || !completed {
				t.Fatalf("replayed purge completed=%t err=%v", completed, err)
			}
			assertSessionDeletionPurged(t, root, tombstone)
		})
	}
}

func TestSessionDeletionPurgeResumesAfterPartialEntryRemoval(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	stop := errors.New("stop after one exact entry removal")
	removed := 0
	sessionDeletionPurgeHook = func(phase string) error {
		if strings.HasPrefix(phase, "removed-entry:") {
			removed++
			if removed == 1 {
				return stop
			}
		}
		return nil
	}
	t.Cleanup(func() { sessionDeletionPurgeHook = nil })

	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || !errors.Is(err, stop) {
		t.Fatalf("partial purge completed=%t err=%v", completed, err)
	}
	receipt, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
	if err != nil || receipt.Phase != sessionDeletionPurgeRenamed {
		t.Fatalf("partial purge receipt=%#v err=%v", receipt, err)
	}
	if _, err := os.Lstat(receipt.StagingPath); err != nil {
		t.Fatalf("partial purge staging disappeared: %v", err)
	}

	sessionDeletionPurgeHook = nil
	if completed, err := AdvanceSessionDeletion(root, tombstone); err != nil || !completed {
		t.Fatalf("partial purge replay completed=%t err=%v", completed, err)
	}
	assertSessionDeletionPurged(t, root, tombstone)
}

func TestSessionDeletionPurgeReplaysEntryStagingProgress(t *testing.T) {
	for _, phase := range []string{"after-entry-stage:manifest.json", "after-entry-marked:manifest.json"} {
		t.Run(phase, func(t *testing.T) {
			root, _, tombstone := sessionDeletionFixture(t)
			stop := errors.New("stop at entry staging boundary")
			stopped := false
			sessionDeletionPurgeHook = func(current string) error {
				if current == phase && !stopped {
					stopped = true
					return stop
				}
				return nil
			}
			t.Cleanup(func() { sessionDeletionPurgeHook = nil })

			if completed, err := AdvanceSessionDeletion(root, tombstone); completed || !errors.Is(err, stop) {
				t.Fatalf("entry staging interruption completed=%t err=%v", completed, err)
			}
			receipt, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
			if err != nil || receipt.Phase != sessionDeletionPurgeRenamed {
				t.Fatalf("entry staging receipt=%#v err=%v", receipt, err)
			}
			aliasEntry := SessionDeletionPurgeEntry{}
			for _, entry := range receipt.Entries {
				if entry.Path == "manifest.json" {
					aliasEntry = entry
					break
				}
			}
			alias := deletionPurgeEntryAliasPath(deletionPurgeAliasRoot(receipt.StagingPath, receipt), aliasEntry)
			if _, err := os.Lstat(alias); err != nil {
				t.Fatalf("entry staging alias missing after interruption: %v", err)
			}

			sessionDeletionPurgeHook = nil
			if completed, err := AdvanceSessionDeletion(root, tombstone); err != nil || !completed {
				t.Fatalf("entry staging replay completed=%t err=%v", completed, err)
			}
			assertSessionDeletionPurged(t, root, tombstone)
		})
	}
}

func TestSessionDeletionPurgeRestagesDurablyMarkedEntryAfterRenameRollback(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	stop := errors.New("stop after durable entry staging")
	stopped := false
	sessionDeletionPurgeHook = func(phase string) error {
		if phase == "after-entry-marked:manifest.json" && !stopped {
			stopped = true
			return stop
		}
		return nil
	}
	t.Cleanup(func() { sessionDeletionPurgeHook = nil })

	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || !errors.Is(err, stop) {
		t.Fatalf("durable staging interruption completed=%t err=%v", completed, err)
	}
	receipt, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var entry SessionDeletionPurgeEntry
	for _, candidate := range receipt.Entries {
		if candidate.Path == "manifest.json" {
			entry = candidate
			break
		}
	}
	alias := deletionPurgeEntryAliasPath(deletionPurgeAliasRoot(receipt.StagingPath, receipt), entry)
	original := filepath.Join(receipt.StagingPath, "manifest.json")
	if err := os.Rename(alias, original); err != nil {
		t.Fatalf("simulate non-durable rename rollback: %v", err)
	}
	if err := syncDeletionRenameParents(alias, original); err != nil {
		t.Fatal(err)
	}

	sessionDeletionPurgeHook = nil
	if completed, err := AdvanceSessionDeletion(root, tombstone); err != nil || !completed {
		t.Fatalf("rename rollback replay completed=%t err=%v", completed, err)
	}
	assertSessionDeletionPurged(t, root, tombstone)
}

func TestSessionDeletionPurgeNeverDeletesIdenticalAliasReplacementAfterUnlink(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	manifestBytes, err := os.ReadFile(tombstone.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop after rebuilding removed alias")
	rebuilt := false
	var alias string
	sessionDeletionPurgeHook = func(phase string) error {
		if phase != "removed-entry:manifest.json" || rebuilt {
			return nil
		}
		rebuilt = true
		receipt, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
		if err != nil {
			return err
		}
		var entry SessionDeletionPurgeEntry
		for _, candidate := range receipt.Entries {
			if candidate.Path == "manifest.json" {
				entry = candidate
				break
			}
		}
		alias = deletionPurgeEntryAliasPath(deletionPurgeAliasRoot(receipt.StagingPath, receipt), entry)
		if err := os.WriteFile(alias, manifestBytes, os.FileMode(entry.Mode)); err != nil {
			return err
		}
		return stop
	}
	t.Cleanup(func() { sessionDeletionPurgeHook = nil })

	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || !errors.Is(err, stop) {
		t.Fatalf("identical alias replacement interruption completed=%t err=%v", completed, err)
	}
	sessionDeletionPurgeHook = nil
	for attempt := 1; attempt <= 2; attempt++ {
		if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
			t.Fatalf("identical alias replacement replay %d completed=%t err=%v", attempt, completed, err)
		}
		got, err := os.ReadFile(alias)
		if err != nil || !bytes.Equal(got, manifestBytes) {
			t.Fatalf("identical alias replacement changed after replay %d: got=%q err=%v", attempt, got, err)
		}
	}
	receipt, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, removed := slices.BinarySearch(receipt.RemovedEntries, "manifest.json"); !removed {
		t.Fatalf("removed manifest progress was not made durable: %#v", receipt.RemovedEntries)
	}
}

func TestLegacySessionDeletionCannotUseHandcraftedValidPurgeReceipt(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	stageSessionDeletionQuarantine(t, tombstone)

	legacy := tombstone
	legacy.Version = sidecarSessionDeletionVersion
	legacy.InitialCheckpointSequence = 0
	legacy.InitialCheckpointSHA256 = ""
	legacy.RetirementRequest = NativeFile{}
	legacy.RetirementAcknowledgement = NativeFile{}
	legacy.MountAcknowledgement = NativeFile{}
	legacy.Journal = NativeFile{}
	legacy.NativeRetirementProof = NativeFile{}
	if err := validateSessionDeletion(root, legacy); err != nil {
		t.Fatalf("legacy tombstone fixture is invalid: %v", err)
	}
	if err := replaceSessionDeletion(root, legacy); err != nil {
		t.Fatal(err)
	}
	quarantineRoot := filepath.Dir(legacy.RetiredSessionPath)
	identity, err := captureDeletionPurgeTreeIdentity(root, quarantineRoot)
	if err != nil {
		t.Fatal(err)
	}
	tombstoneIdentity, err := captureStableDeletionFileWithinStore(root, SessionDeletionPath(root, legacy.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	receipt := SessionDeletionPurge{
		Version: sessionDeletionPurgeVersion, Kind: sessionDeletionPurgeKind,
		SessionID: legacy.SessionID, OperationToken: legacy.OperationToken,
		TombstoneSHA256: tombstoneIdentity.SHA256, QuarantineTreeSHA256: identity.SHA256,
		QuarantineBytes: identity.Bytes, QuarantineEntries: identity.Entries,
		QuarantineRootMode: identity.RootMode, QuarantineRootObject: identity.RootObject,
		QuarantineRootXattrs: identity.RootXattrs, Entries: identity.Tree,
		StagingPath: deletionPurgeStagingPath(root, legacy),
		Phase:       sessionDeletionPurgePrepared, PreparedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeSessionDeletionPurge(root, receipt); err != nil {
		t.Fatal(err)
	}

	if completed, err := AdvanceSessionDeletion(root, legacy); completed || err == nil {
		t.Fatalf("legacy purge receipt completed=%t err=%v", completed, err)
	}
	for _, path := range []string{legacy.RetiredSessionPath, legacy.RetiredManifestPath} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("legacy purge evidence was not preserved at %s: %v", path, err)
		}
	}
}

func TestSessionDeletionPurgePreservesCanonicalReplacementAfterQuarantine(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	replacement := filepath.Join(tombstone.SessionPath, "replacement-evidence")
	created := false
	sessionDeletionPurgeHook = func(phase string) error {
		if phase != sessionDeletionPurgePrepared || created {
			return nil
		}
		created = true
		if err := os.MkdirAll(tombstone.SessionPath, 0o700); err != nil {
			return err
		}
		return os.WriteFile(replacement, []byte("new authority must survive"), 0o600)
	}
	t.Cleanup(func() { sessionDeletionPurgeHook = nil })

	if completed, err := AdvanceSessionDeletion(root, tombstone); err != nil || !completed {
		t.Fatalf("replacement-preserving purge completed=%t err=%v", completed, err)
	}
	if got, err := os.ReadFile(replacement); err != nil || string(got) != "new authority must survive" {
		t.Fatalf("canonical replacement changed: got=%q err=%v", got, err)
	}
}

func TestSessionDeletionPurgePreservesReplacementAtEntryStagingBoundaries(t *testing.T) {
	t.Run("before atomic entry staging", func(t *testing.T) {
		root, _, tombstone := sessionDeletionFixture(t)
		replacementBytes := []byte("replacement before entry staging")
		replaced := false
		sessionDeletionPurgeHook = func(phase string) error {
			if phase != "before-entry-stage:manifest.json" || replaced {
				return nil
			}
			replaced = true
			receipt, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
			if err != nil {
				return err
			}
			original := filepath.Join(receipt.StagingPath, "manifest.json")
			if err := os.Remove(original); err != nil {
				return err
			}
			return os.WriteFile(original, replacementBytes, 0o600)
		}
		t.Cleanup(func() { sessionDeletionPurgeHook = nil })

		if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
			t.Fatalf("entry-stage replacement completed=%t err=%v", completed, err)
		}
		receipt, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		original := filepath.Join(receipt.StagingPath, "manifest.json")
		if got, err := os.ReadFile(original); err != nil || !bytes.Equal(got, replacementBytes) {
			t.Fatalf("entry-stage replacement was not restored: got=%q err=%v", got, err)
		}
	})

	t.Run("before staged alias removal", func(t *testing.T) {
		root, _, tombstone := sessionDeletionFixture(t)
		replacementBytes := []byte("replacement at private alias")
		replaced := false
		sessionDeletionPurgeHook = func(phase string) error {
			if phase != "before-staged-entry-remove:manifest.json" || replaced {
				return nil
			}
			replaced = true
			receipt, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
			if err != nil {
				return err
			}
			var entry SessionDeletionPurgeEntry
			for _, candidate := range receipt.Entries {
				if candidate.Path == "manifest.json" {
					entry = candidate
					break
				}
			}
			alias := deletionPurgeEntryAliasPath(deletionPurgeAliasRoot(receipt.StagingPath, receipt), entry)
			if err := os.Remove(alias); err != nil {
				return err
			}
			return os.WriteFile(alias, replacementBytes, 0o600)
		}
		t.Cleanup(func() { sessionDeletionPurgeHook = nil })

		if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
			t.Fatalf("alias replacement completed=%t err=%v", completed, err)
		}
		receipt, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		var entry SessionDeletionPurgeEntry
		for _, candidate := range receipt.Entries {
			if candidate.Path == "manifest.json" {
				entry = candidate
				break
			}
		}
		alias := deletionPurgeEntryAliasPath(deletionPurgeAliasRoot(receipt.StagingPath, receipt), entry)
		if got, err := os.ReadFile(alias); err != nil || !bytes.Equal(got, replacementBytes) {
			t.Fatalf("private alias replacement was not preserved: got=%q err=%v", got, err)
		}
	})

	t.Run("alias target appears before no-replace rename", func(t *testing.T) {
		root, _, tombstone := sessionDeletionFixture(t)
		foreignAliasBytes := []byte("foreign alias target must survive")
		created := false
		var alias string
		sessionDeletionPurgeHook = func(phase string) error {
			if phase != "before-entry-stage:manifest.json" || created {
				return nil
			}
			created = true
			receipt, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
			if err != nil {
				return err
			}
			var entry SessionDeletionPurgeEntry
			for _, candidate := range receipt.Entries {
				if candidate.Path == "manifest.json" {
					entry = candidate
					break
				}
			}
			alias = deletionPurgeEntryAliasPath(deletionPurgeAliasRoot(receipt.StagingPath, receipt), entry)
			return os.WriteFile(alias, foreignAliasBytes, 0o600)
		}
		t.Cleanup(func() { sessionDeletionPurgeHook = nil })

		if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
			t.Fatalf("alias no-replace race completed=%t err=%v", completed, err)
		}
		if got, err := os.ReadFile(alias); err != nil || !bytes.Equal(got, foreignAliasBytes) {
			t.Fatalf("foreign alias target was not preserved: got=%q err=%v", got, err)
		}
	})

	t.Run("original path rebuilt after durable staging", func(t *testing.T) {
		root, _, tombstone := sessionDeletionFixture(t)
		replacementBytes := []byte("replacement rebuilt at the original staging path")
		stop := errors.New("stop after rebuilding original staging path")
		rebuilt := false
		sessionDeletionPurgeHook = func(phase string) error {
			if phase != "after-entry-marked:manifest.json" || rebuilt {
				return nil
			}
			rebuilt = true
			receipt, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(receipt.StagingPath, "manifest.json"), replacementBytes, 0o600); err != nil {
				return err
			}
			return stop
		}
		t.Cleanup(func() { sessionDeletionPurgeHook = nil })

		if completed, err := AdvanceSessionDeletion(root, tombstone); completed || !errors.Is(err, stop) {
			t.Fatalf("rebuilt staging path interruption completed=%t err=%v", completed, err)
		}
		sessionDeletionPurgeHook = nil
		if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
			t.Fatalf("rebuilt staging path replay completed=%t err=%v", completed, err)
		}
		receipt, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(receipt.StagingPath, "manifest.json")); err != nil || !bytes.Equal(got, replacementBytes) {
			t.Fatalf("rebuilt staging replacement was not preserved: got=%q err=%v", got, err)
		}
	})
}

func TestSessionDeletionPurgeRequiresExactObjectIdentityAtEntryStagingBoundaries(t *testing.T) {
	t.Run("identical original replacement before stage", func(t *testing.T) {
		root, _, tombstone := sessionDeletionFixture(t)
		replaced := false
		var original string
		var originalBytes []byte
		sessionDeletionPurgeHook = func(phase string) error {
			if phase != "before-entry-stage:manifest.json" || replaced {
				return nil
			}
			replaced = true
			receipt, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
			if err != nil {
				return err
			}
			original = filepath.Join(receipt.StagingPath, "manifest.json")
			originalBytes, err = os.ReadFile(original)
			if err != nil {
				return err
			}
			before, err := os.Lstat(original)
			if err != nil {
				return err
			}
			held, err := os.Open(original)
			if err != nil {
				return err
			}
			if err := os.Remove(original); err != nil {
				_ = held.Close()
				return err
			}
			if err := os.WriteFile(original, originalBytes, before.Mode().Perm()); err != nil {
				_ = held.Close()
				return err
			}
			after, err := os.Lstat(original)
			if err != nil {
				_ = held.Close()
				return err
			}
			sameObject := os.SameFile(before, after)
			closeErr := held.Close()
			if sameObject {
				return errors.New("test replacement unexpectedly reused the original object identity")
			}
			return closeErr
		}
		t.Cleanup(func() { sessionDeletionPurgeHook = nil })

		if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
			t.Fatalf("identical original replacement completed=%t err=%v", completed, err)
		}
		if got, err := os.ReadFile(original); err != nil || !bytes.Equal(got, originalBytes) {
			t.Fatalf("identical original replacement changed: got=%q err=%v", got, err)
		}
	})

	t.Run("identical alias replacement before remove", func(t *testing.T) {
		root, _, tombstone := sessionDeletionFixture(t)
		replaced := false
		var alias string
		var aliasBytes []byte
		sessionDeletionPurgeHook = func(phase string) error {
			if phase != "before-staged-entry-remove:manifest.json" || replaced {
				return nil
			}
			replaced = true
			receipt, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
			if err != nil {
				return err
			}
			var entry SessionDeletionPurgeEntry
			for _, candidate := range receipt.Entries {
				if candidate.Path == "manifest.json" {
					entry = candidate
					break
				}
			}
			alias = deletionPurgeEntryAliasPath(deletionPurgeAliasRoot(receipt.StagingPath, receipt), entry)
			aliasBytes, err = os.ReadFile(alias)
			if err != nil {
				return err
			}
			before, err := os.Lstat(alias)
			if err != nil {
				return err
			}
			held, err := os.Open(alias)
			if err != nil {
				return err
			}
			if err := os.Remove(alias); err != nil {
				_ = held.Close()
				return err
			}
			if err := os.WriteFile(alias, aliasBytes, before.Mode().Perm()); err != nil {
				_ = held.Close()
				return err
			}
			after, err := os.Lstat(alias)
			if err != nil {
				_ = held.Close()
				return err
			}
			sameObject := os.SameFile(before, after)
			closeErr := held.Close()
			if sameObject {
				return errors.New("test alias replacement unexpectedly reused the original object identity")
			}
			return closeErr
		}
		t.Cleanup(func() { sessionDeletionPurgeHook = nil })

		if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
			t.Fatalf("identical alias replacement completed=%t err=%v", completed, err)
		}
		if got, err := os.ReadFile(alias); err != nil || !bytes.Equal(got, aliasBytes) {
			t.Fatalf("identical alias replacement changed: got=%q err=%v", got, err)
		}
	})
}

func TestSessionDeletionPurgeRevalidatesExactTreeAndPreservesTampering(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	stop := errors.New("stop after purge proof")
	sessionDeletionPurgeHook = func(phase string) error {
		if phase == sessionDeletionPurgePrepared {
			return stop
		}
		return nil
	}
	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || !errors.Is(err, stop) {
		t.Fatalf("prepared purge completed=%t err=%v", completed, err)
	}
	sessionDeletionPurgeHook = nil
	t.Cleanup(func() { sessionDeletionPurgeHook = nil })

	state, _, err := readSessionState(filepath.Join(tombstone.RetiredSessionPath, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	delta := filepath.Join(tombstone.RetiredSessionPath, filepath.Base(state.DeltaPath))
	file, err := os.OpenFile(delta, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("tamper"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
		t.Fatalf("tampered purge completed=%t err=%v", completed, err)
	}
	if got, err := os.ReadFile(delta); err != nil || len(got) < len("tamper") || string(got[len(got)-len("tamper"):]) != "tamper" {
		t.Fatalf("tampered evidence changed: bytes=%q err=%v", got, err)
	}
}

func TestSessionDeletionPurgeRejectsContentInsertedAcrossOwnershipCapture(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	foreign := filepath.Join(filepath.Dir(tombstone.RetiredSessionPath), "foreign-after-semantic-validation")
	injected := false
	sessionDeletionPurgeHook = func(phase string) error {
		if phase != "semantic-validated" || injected {
			return nil
		}
		injected = true
		return os.WriteFile(foreign, []byte("must not enter the durable purge receipt"), 0o600)
	}
	t.Cleanup(func() { sessionDeletionPurgeHook = nil })

	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
		t.Fatalf("ownership-capture race completed=%t err=%v", completed, err)
	}
	if got, err := os.ReadFile(foreign); err != nil || string(got) != "must not enter the durable purge receipt" {
		t.Fatalf("ownership-race evidence changed: got=%q err=%v", got, err)
	}
	if _, err := os.Lstat(SessionDeletionPurgePath(root, tombstone.SessionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("purge receipt was published for a tree that changed across ownership validation: %v", err)
	}
}

func TestSessionDeletionPurgeRejectsTamperedNativeSidecarIdentity(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	stageSessionDeletionQuarantine(t, tombstone)
	target := filepath.Join(tombstone.RetiredSessionPath, "retained-native", "store-snapshot")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "fs", "snapshots", tombstone.SessionID), target); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(target, "._native.jsonl")
	if err := os.WriteFile(sidecar, []byte("same name, different bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
		t.Fatalf("tampered sidecar purge completed=%t err=%v", completed, err)
	}
	if got, err := os.ReadFile(sidecar); err != nil || string(got) != "same name, different bytes" {
		t.Fatalf("tampered sidecar was not preserved: got=%q err=%v", got, err)
	}
}

func TestSessionDeletionPurgePreservesUnknownNonemptyContent(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	unknown := filepath.Join(tombstone.SessionPath, "foreign-evidence")
	if err := os.WriteFile(unknown, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
		t.Fatalf("unknown nonempty purge completed=%t err=%v", completed, err)
	}
	quarantined := filepath.Join(tombstone.RetiredSessionPath, filepath.Base(unknown))
	if got, err := os.ReadFile(quarantined); err != nil || string(got) != "preserve" {
		t.Fatalf("unknown evidence changed: got=%q err=%v", got, err)
	}
}

func TestSessionDeletionPurgePreservesUnknownNonemptyContentInsideKnownDirectories(t *testing.T) {
	for _, relative := range []string{
		filepath.Join(stateGenerationsDirectoryName, "foreign-evidence"),
		filepath.Join("state-orphans", "foreign-evidence"),
		filepath.Join("leases", "generation-00000000000000000001", "foreign-evidence"),
		filepath.Join("retained-native", "nested", "foreign-evidence"),
	} {
		t.Run(filepath.ToSlash(relative), func(t *testing.T) {
			root, _, tombstone := sessionDeletionFixture(t)
			if err := os.MkdirAll(filepath.Dir(tombstone.RetiredSessionPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tombstone.SessionPath, tombstone.RetiredSessionPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tombstone.ManifestPath, tombstone.RetiredManifestPath); err != nil {
				t.Fatal(err)
			}
			evidence := filepath.Join(tombstone.RetiredSessionPath, relative)
			if err := os.MkdirAll(filepath.Dir(evidence), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(evidence, []byte("preserve nested evidence"), 0o600); err != nil {
				t.Fatal(err)
			}

			if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
				t.Fatalf("nested unknown purge completed=%t err=%v", completed, err)
			}
			if got, err := os.ReadFile(evidence); err != nil || string(got) != "preserve nested evidence" {
				t.Fatalf("nested unknown evidence changed: got=%q err=%v", got, err)
			}
			if _, err := os.Lstat(SessionDeletionPurgePath(root, tombstone.SessionID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("purge receipt exists despite ambiguous nested content: %v", err)
			}
		})
	}
}

func TestSessionDeletionPurgeQuarantinesAndPreservesUnknownSnapshotSibling(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	foreignSource := filepath.Join(root, "fs", "snapshots", tombstone.SessionID, "foreign-evidence")
	if err := os.WriteFile(foreignSource, []byte("snapshot sibling must survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
		t.Fatalf("unknown snapshot sibling purge completed=%t err=%v", completed, err)
	}
	if _, err := os.Lstat(foreignSource); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot sibling remained outside quarantine: %v", err)
	}
	quarantined := filepath.Join(tombstone.RetiredSessionPath, "retained-native", "store-snapshot", "foreign-evidence")
	if got, err := os.ReadFile(quarantined); err != nil || string(got) != "snapshot sibling must survive" {
		t.Fatalf("quarantined snapshot sibling changed: got=%q err=%v", got, err)
	}
}

func TestSessionDeletionPurgeAcceptsStrictlyOwnedNestedProtocolArtifacts(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	stageSessionDeletionQuarantine(t, tombstone)

	generations := filepath.Join(tombstone.RetiredSessionPath, stateGenerationsDirectoryName)
	entries, err := os.ReadDir(generations)
	if err != nil || len(entries) == 0 {
		t.Fatalf("read checkpoint history: entries=%d err=%v", len(entries), err)
	}
	checkpointData, err := os.ReadFile(filepath.Join(generations, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(tombstone.RetiredSessionPath, "state-orphans", entries[0].Name())
	if err := os.MkdirAll(filepath.Dir(orphan), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, checkpointData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tombstone.RetiredSessionPath, "leases", "generation-00000000000000000001"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tombstone.RetiredSessionPath, "retained-native", "empty"), 0o700); err != nil {
		t.Fatal(err)
	}

	if completed, err := AdvanceSessionDeletion(root, tombstone); err != nil || !completed {
		t.Fatalf("strictly owned nested protocol purge completed=%t err=%v", completed, err)
	}
	assertSessionDeletionPurged(t, root, tombstone)
}

func TestSessionDeletionPurgeRejectsSelfAuthenticatingCheckpointFromAnotherStore(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	stageSessionDeletionQuarantine(t, tombstone)
	generations := filepath.Join(tombstone.RetiredSessionPath, stateGenerationsDirectoryName)
	entries, err := os.ReadDir(generations)
	if err != nil || len(entries) == 0 {
		t.Fatalf("read checkpoint history: entries=%d err=%v", len(entries), err)
	}
	data, err := os.ReadFile(filepath.Join(generations, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := decodeStateCheckpoint(data)
	if err != nil {
		t.Fatal(err)
	}
	otherRoot := t.TempDir()
	otherSession := filepath.Join(otherRoot, "fs", "sessions", tombstone.SessionID)
	checkpoint.State.ManifestPath = filepath.Join(otherRoot, "manifests", tombstone.SessionID+".json")
	checkpoint.Manifest.Path = checkpoint.State.ManifestPath
	checkpoint.State.DeltaPath = filepath.Join(otherSession, "delta.jsonl")
	checkpoint.Delta.Path = checkpoint.State.DeltaPath
	stateData, err := encodeSessionState(checkpoint.State)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.StateSHA256 = digestStateBytes(stateData)
	foreignData, err := encodeStateCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	foreignName := stateCheckpointFilename(checkpoint.State.Generation, checkpoint.Sequence, digestStateBytes(foreignData))
	foreign := filepath.Join(tombstone.RetiredSessionPath, "state-orphans", foreignName)
	if err := os.MkdirAll(filepath.Dir(foreign), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, foreignData, 0o600); err != nil {
		t.Fatal(err)
	}

	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
		t.Fatalf("other-store checkpoint purge completed=%t err=%v", completed, err)
	}
	if _, err := os.Lstat(foreign); err != nil {
		t.Fatalf("other-store checkpoint evidence was removed: %v", err)
	}
}

func TestSessionDeletionPurgeRejectsUnrelatedSelfAuthenticatingCheckpointFromSameStore(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	stageSessionDeletionQuarantine(t, tombstone)
	generations := filepath.Join(tombstone.RetiredSessionPath, stateGenerationsDirectoryName)
	entries, err := os.ReadDir(generations)
	if err != nil || len(entries) == 0 {
		t.Fatalf("read checkpoint history: entries=%d err=%v", len(entries), err)
	}
	data, err := os.ReadFile(filepath.Join(generations, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := decodeStateCheckpoint(data)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.Sequence += 100
	foreignData, err := encodeStateCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	foreignName := stateCheckpointFilename(checkpoint.State.Generation, checkpoint.Sequence, digestStateBytes(foreignData))
	foreign := filepath.Join(tombstone.RetiredSessionPath, "state-orphans", foreignName)
	if err := os.MkdirAll(filepath.Dir(foreign), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, foreignData, 0o600); err != nil {
		t.Fatal(err)
	}

	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
		t.Fatalf("unrelated same-store checkpoint purge completed=%t err=%v", completed, err)
	}
	if got, err := os.ReadFile(foreign); err != nil || !bytes.Equal(got, foreignData) {
		t.Fatalf("unrelated same-store checkpoint was not preserved: bytes=%d err=%v", len(got), err)
	}
}

func TestSessionDeletionPurgeAcceptsExactRetainedNativeSnapshot(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	hiddenSnapshot := filepath.Join(root, "fs", "snapshots", manifest.Session.ID, "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(hiddenSnapshot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(manifest.Session.RolloutPath, hiddenSnapshot); err != nil {
		t.Fatal(err)
	}
	session, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: manifestPathForDeletionTest(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
		NativeSnapshot: NativeFile{Path: hiddenSnapshot, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
	})
	if err != nil {
		t.Fatal(err)
	}
	tombstone, err := PublishSessionDeletion(root, session.State(), "/sessions/2026/07/25/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	stageSessionDeletionQuarantine(t, tombstone)
	retainedDirectory := filepath.Join(tombstone.RetiredSessionPath, "retained-native", "store-snapshot")
	if err := os.MkdirAll(filepath.Dir(retainedDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Dir(hiddenSnapshot), retainedDirectory); err != nil {
		t.Fatal(err)
	}

	if completed, err := AdvanceSessionDeletion(root, tombstone); err != nil || !completed {
		t.Fatalf("exact retained native purge completed=%t err=%v", completed, err)
	}
	assertSessionDeletionPurged(t, root, tombstone)
}

func TestSessionDeletionPurgePreservesNestedSymlink(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	stageSessionDeletionQuarantine(t, tombstone)
	outside := filepath.Join(t.TempDir(), "outside-evidence")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(tombstone.RetiredSessionPath, "retained-native", "nested", "foreign-link")
	if err := os.MkdirAll(filepath.Dir(symlink), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, symlink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
		t.Fatalf("nested symlink purge completed=%t err=%v", completed, err)
	}
	if info, err := os.Lstat(symlink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("nested symlink was not preserved: info=%v err=%v", info, err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "outside" {
		t.Fatalf("symlink target changed: got=%q err=%v", got, err)
	}
}

func TestLoadSessionDeletionPurgeRejectsSymlinkReceipt(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	if completed, err := AdvanceSessionDeletion(root, tombstone); err != nil || !completed {
		t.Fatalf("prepare completed purge receipt: completed=%t err=%v", completed, err)
	}
	receiptPath := SessionDeletionPurgePath(root, tombstone.SessionID)
	data, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "receipt.json")
	if err := os.WriteFile(outside, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(receiptPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, receiptPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := LoadSessionDeletionPurge(root, tombstone.SessionID); err == nil {
		t.Fatal("symlink purge receipt was accepted as durable authority")
	}
}

func TestSessionDeletionRefusesSymlinkedQuarantineAncestor(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	outside := t.TempDir()
	deletedRoot := filepath.Join(root, "fs", "deleted")
	if err := os.MkdirAll(deletedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	unsafeAncestor := filepath.Join(deletedRoot, tombstone.SessionID)
	if err := os.Symlink(outside, unsafeAncestor); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
		t.Fatalf("symlinked quarantine ancestor completed=%t err=%v", completed, err)
	}
	assertDeletionSourcesPresent(t, tombstone)
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("deletion created content through an outside symlink: %#v", entries)
	}
}

func TestSessionDeletionRefusesSymlinkedSourceAncestors(t *testing.T) {
	for _, test := range []struct {
		name      string
		directory func(string) string
		proof     func(SessionDeletion) string
	}{
		{name: "sessions", directory: func(root string) string { return filepath.Join(root, "fs", "sessions") }, proof: func(tombstone SessionDeletion) string { return filepath.Join(tombstone.SessionPath, "state.json") }},
		{name: "manifests", directory: func(root string) string { return filepath.Join(root, "manifests") }, proof: func(tombstone SessionDeletion) string { return tombstone.ManifestPath }},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, _, tombstone := sessionDeletionFixture(t)
			sourceDirectory := test.directory(root)
			outsideDirectory := filepath.Join(t.TempDir(), filepath.Base(sourceDirectory))
			if err := os.Rename(sourceDirectory, outsideDirectory); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outsideDirectory, sourceDirectory); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			proofPath := test.proof(tombstone)
			before, err := os.ReadFile(proofPath)
			if err != nil {
				t.Fatal(err)
			}

			if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
				t.Fatalf("symlinked %s source completed=%t err=%v", test.name, completed, err)
			}
			after, err := os.ReadFile(proofPath)
			if err != nil || !bytes.Equal(after, before) {
				t.Fatalf("outside source changed: before=%q after=%q err=%v", before, after, err)
			}
		})
	}
}

func TestSessionDeletionRequiresCompleteCheckpointLineageForRefresh(t *testing.T) {
	root, session, tombstone := sessionDeletionFixture(t)
	state := session.State()
	foreign := filepath.Join(tombstone.SessionPath, "delta-forged.jsonl")
	if err := os.WriteFile(foreign, []byte("must survive incomplete lineage"), 0o600); err != nil {
		t.Fatal(err)
	}
	next := state
	next.Generation++
	next.DeltaPath = foreign
	if err := publishSessionState(filepath.Join(tombstone.SessionPath, "state.json"), next); err != nil {
		t.Fatal(err)
	}
	chain, err := loadStateCheckpointChain(tombstone.SessionPath, tombstone.SessionID, true)
	if err != nil {
		t.Fatal(err)
	}
	removed := false
	for digest, checkpoint := range chain.lineage {
		if checkpoint.State.Generation != tombstone.InitialStateGeneration || checkpoint.StateSHA256 != tombstone.InitialStateSHA256 {
			continue
		}
		path := filepath.Join(tombstone.SessionPath, stateGenerationsDirectoryName, stateCheckpointFilename(checkpoint.State.Generation, checkpoint.Sequence, digest))
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		removed = true
		break
	}
	if !removed {
		t.Fatal("initial deletion checkpoint was not found")
	}

	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
		t.Fatalf("incomplete lineage deletion completed=%t err=%v", completed, err)
	}
	if got, err := os.ReadFile(foreign); err != nil || string(got) != "must survive incomplete lineage" {
		t.Fatalf("foreign data changed after rejected lineage: got=%q err=%v", got, err)
	}
}

func TestSessionDeletionRejectsForgedRootWithSameStateAndDifferentDeltaIdentity(t *testing.T) {
	root, session, tombstone := sessionDeletionFixture(t)
	state := session.State()
	chain, err := loadStateCheckpointChain(tombstone.SessionPath, tombstone.SessionID, true)
	if err != nil {
		t.Fatal(err)
	}
	foreignBytes := []byte("foreign delta under an otherwise identical SessionState")
	if err := os.WriteFile(state.DeltaPath, foreignBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	foreignIdentity, err := captureRegularFileIdentity(state.DeltaPath)
	if err != nil {
		t.Fatal(err)
	}
	forged := chain.checkpoint
	forged.Sequence += 100
	forged.PreviousCheckpointSHA256 = ""
	forged.Delta = foreignIdentity
	forgedData, err := encodeStateCheckpoint(forged)
	if err != nil {
		t.Fatal(err)
	}
	forgedDigest := digestStateBytes(forgedData)
	generations := filepath.Join(tombstone.SessionPath, stateGenerationsDirectoryName)
	if err := os.RemoveAll(generations); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(generations, 0o700); err != nil {
		t.Fatal(err)
	}
	forgedName := stateCheckpointFilename(forged.State.Generation, forged.Sequence, forgedDigest)
	if err := os.WriteFile(filepath.Join(generations, forgedName), forgedData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeStateCatalog(tombstone.SessionPath, sessionStateCatalog{
		Version: sessionStateCatalogVersion, SessionID: state.SessionID,
		Generation: state.Generation, Sequence: forged.Sequence, StateSHA256: forged.StateSHA256,
		Checkpoint: forgedName, CheckpointSHA256: forgedDigest,
	}); err != nil {
		t.Fatal(err)
	}

	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
		t.Fatalf("forged root lineage deletion completed=%t err=%v", completed, err)
	}
	if got, err := os.ReadFile(state.DeltaPath); err != nil || !bytes.Equal(got, foreignBytes) {
		t.Fatalf("forged-root delta was not preserved: got=%q err=%v", got, err)
	}
}

func TestSessionDeletionRefusesStateChangeAndConflictingQuarantine(t *testing.T) {
	t.Run("manifest changed", func(t *testing.T) {
		root, _, tombstone := sessionDeletionFixture(t)
		if err := os.WriteFile(tombstone.ManifestPath, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
			t.Fatalf("changed manifest replay completed=%t err=%v", completed, err)
		}
		assertDeletionSourcesPresent(t, tombstone)
	})

	t.Run("conflicting target", func(t *testing.T) {
		root, _, tombstone := sessionDeletionFixture(t)
		if err := os.MkdirAll(tombstone.RetiredSessionPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tombstone.RetiredSessionPath, "foreign"), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
			t.Fatalf("conflicting replay completed=%t err=%v", completed, err)
		}
		assertDeletionSourcesPresent(t, tombstone)
		if got, err := os.ReadFile(filepath.Join(tombstone.RetiredSessionPath, "foreign")); err != nil || string(got) != "keep" {
			t.Fatalf("conflicting target changed: %q err=%v", got, err)
		}
	})
}

func TestSessionDeletionPublicationRequiresExactManifestBytes(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	if err := os.WriteFile(session.State().ManifestPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishSessionDeletion(root, session.State(), "/sessions/2026/07/25/session.jsonl"); err == nil {
		t.Fatal("manifest byte mismatch published a deletion tombstone")
	}
	if _, err := os.Lstat(SessionDeletionPath(root, session.State().SessionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tombstone exists after rejected publication: %v", err)
	}
}

func TestSessionDeletionPhysicalPurgeRejectsNoncanonicalNativeSnapshot(t *testing.T) {
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	session := openFixtureSession(t, root, manifest, reader, nil)
	tombstone, err := PublishSessionDeletion(root, session.State(), "/sessions/2026/07/25/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
		t.Fatalf("noncanonical snapshot purge completed=%t err=%v", completed, err)
	}
	assertDeletionSourcesPresent(t, tombstone)
	if got, err := os.ReadFile(manifest.Session.RolloutPath); err != nil || len(got) == 0 {
		t.Fatalf("noncanonical snapshot changed: bytes=%d err=%v", len(got), err)
	}
}

func sessionDeletionFixture(t *testing.T) (string, *Session, SessionDeletion) {
	t.Helper()
	root := t.TempDir()
	manifest, reader, _ := sessionFixture(t, root)
	hiddenSnapshot := filepath.Join(root, "fs", "snapshots", manifest.Session.ID, "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(hiddenSnapshot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(manifest.Session.RolloutPath, hiddenSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(hiddenSnapshot), "._native.jsonl"), []byte("appledouble-sidecar"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := OpenSession(context.Background(), SessionOptions{
		Root: root, ManifestPath: manifestPathForDeletionTest(root, manifest.Session.ID), Manifest: manifest, Reader: reader,
		NativeSnapshot: NativeFile{Path: hiddenSnapshot, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
	})
	if err != nil {
		t.Fatal(err)
	}
	tombstone, err := PublishSessionDeletion(root, session.State(), "/sessions/2026/07/25/session.jsonl")
	if err != nil {
		t.Fatalf("PublishSessionDeletion: %v", err)
	}
	return root, session, tombstone
}

func stageSessionDeletionQuarantine(t *testing.T, tombstone SessionDeletion) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(tombstone.RetiredSessionPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tombstone.SessionPath, tombstone.RetiredSessionPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tombstone.ManifestPath, tombstone.RetiredManifestPath); err != nil {
		t.Fatal(err)
	}
}

func manifestPathForDeletionTest(root string, sessionID string) string {
	return filepath.Join(root, "manifests", sessionID+".json")
}

func assertDeletionSourcesPresent(t *testing.T, tombstone SessionDeletion) {
	t.Helper()
	for _, source := range []string{tombstone.SessionPath, tombstone.ManifestPath} {
		if _, err := os.Lstat(source); err != nil {
			t.Fatalf("deletion source disappeared at %s: %v", source, err)
		}
	}
}

func assertSessionDeletionPurged(t *testing.T, root string, tombstone SessionDeletion) {
	t.Helper()
	for _, path := range []string{
		tombstone.SessionPath,
		tombstone.ManifestPath,
		filepath.Join(root, "fs", "snapshots", tombstone.SessionID, "native.jsonl"),
		filepath.Join(root, "fs", "snapshots", tombstone.SessionID, "._native.jsonl"),
		filepath.Join(root, "fs", "snapshots", tombstone.SessionID),
		tombstone.RetiredSessionPath,
		tombstone.RetiredManifestPath,
		filepath.Dir(tombstone.RetiredSessionPath),
		deletionPurgeStagingPath(root, tombstone),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deleted session data remains at %s: %v", path, err)
		}
	}
	receipt, err := LoadSessionDeletionPurge(root, tombstone.SessionID)
	if err != nil {
		t.Fatalf("load session deletion purge receipt: %v", err)
	}
	if receipt.Phase != sessionDeletionPurgeComplete || receipt.OperationToken != tombstone.OperationToken || receipt.CompletedAt == "" {
		t.Fatalf("invalid completed purge receipt: %#v", receipt)
	}
	if _, err := LoadSessionDeletion(root, tombstone.SessionID); err != nil {
		t.Fatalf("durable deletion tombstone disappeared after purge: %v", err)
	}
}

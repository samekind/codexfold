package prune

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/fold"
	_ "modernc.org/sqlite"
)

func TestRemoveContainedDryRunProvesWithoutMutation(t *testing.T) {
	fixture := newRemovalFixture(t)
	result, err := RemoveContained(context.Background(), fixture.home, fixture.store, fixture.contained, fixture.container, Options{})
	if err != nil {
		t.Fatalf("RemoveContained returned error: %v", err)
	}
	if !result.DryRun || !result.Contained || !result.FoldVerified || !result.UnfoldVerified || result.Removed {
		t.Fatalf("unexpected dry-run result: %#v", result)
	}
	assertRemovalState(t, fixture, true)
	if _, err := os.Stat(TombstonePath(fixture.store, fixture.contained.ID)); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote tombstone: %v", err)
	}
}

func TestRemoveContainedApplyCleansStateAndKeepsRecoveryManifest(t *testing.T) {
	fixture := newRemovalFixture(t)
	result, err := RemoveContained(context.Background(), fixture.home, fixture.store, fixture.contained, fixture.container, applyRemovalOptions())
	if err != nil {
		t.Fatalf("RemoveContained returned error: %v", err)
	}
	if !result.Removed || result.DryRun {
		t.Fatalf("unexpected apply result: %#v", result)
	}
	assertRemovalState(t, fixture, false)
	if _, err := fold.LoadManifest(fixture.store, fixture.contained.ID); err != nil {
		t.Fatalf("recovery manifest was removed: %v", err)
	}
	data, err := os.ReadFile(TombstonePath(fixture.store, fixture.contained.ID))
	if err != nil {
		t.Fatalf("read tombstone: %v", err)
	}
	var tombstone Tombstone
	if err := json.Unmarshal(data, &tombstone); err != nil {
		t.Fatalf("decode tombstone: %v", err)
	}
	if tombstone.ContainedSessionID != fixture.contained.ID || tombstone.ContainerSessionID != fixture.container.ID || tombstone.SourceSHA256 == "" {
		t.Fatalf("unexpected tombstone: %#v", tombstone)
	}

	restoredPath := filepath.Join(fixture.home, "restored.jsonl")
	if _, err := fold.Unfold(context.Background(), fixture.store, fixture.contained.ID, restoredPath, false); err != nil {
		t.Fatalf("retained fold cannot restore removed rollout: %v", err)
	}
}

func TestRemoveContainedRejectsActiveSessionAndMissingFold(t *testing.T) {
	fixture := newRemovalFixture(t)
	active := fixture.contained
	active.Archived = false
	if _, err := RemoveContained(context.Background(), fixture.home, fixture.store, active, fixture.container, Options{}); err == nil {
		t.Fatalf("active contained session should be rejected")
	}
	if err := os.Remove(fold.ManifestPath(fixture.store, fixture.contained.ID)); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}
	if _, err := RemoveContained(context.Background(), fixture.home, fixture.store, fixture.contained, fixture.container, Options{}); err == nil {
		t.Fatalf("missing fold manifest should be rejected")
	}
}

func TestRemoveContainedRollsBackSourceGlobalStateAndTombstoneOnDatabaseFailure(t *testing.T) {
	fixture := newRemovalFixture(t)
	db, err := sql.Open("sqlite", filepath.Join(fixture.home, "state_5.sqlite"))
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	if _, err := db.Exec(`drop table thread_dynamic_tools; create table thread_dynamic_tools (wrong_column text)`); err != nil {
		_ = db.Close()
		t.Fatalf("break dynamic tools schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close state db: %v", err)
	}

	if _, err := RemoveContained(context.Background(), fixture.home, fixture.store, fixture.contained, fixture.container, applyRemovalOptions()); err == nil {
		t.Fatalf("RemoveContained should fail on incompatible database schema")
	}
	if _, err := os.Stat(fixture.contained.RolloutPath); err != nil {
		t.Fatalf("source was not restored after rollback: %v", err)
	}
	if _, err := os.Stat(fixture.contained.RolloutPath + ".codexfold-remove-pending"); !os.IsNotExist(err) {
		t.Fatalf("pending source remains after rollback: %v", err)
	}
	if _, err := os.Stat(TombstonePath(fixture.store, fixture.contained.ID)); !os.IsNotExist(err) {
		t.Fatalf("tombstone remains after rollback: %v", err)
	}
	globalData, err := os.ReadFile(filepath.Join(fixture.home, ".codex-global-state.json"))
	if err != nil || !bytes.Contains(globalData, []byte(fixture.contained.ID)) {
		t.Fatalf("global state was not restored: err=%v data=%s", err, globalData)
	}
	db, err = sql.Open("sqlite", filepath.Join(fixture.home, "state_5.sqlite"))
	if err != nil {
		t.Fatalf("reopen state db: %v", err)
	}
	defer func() { _ = db.Close() }()
	var count int
	if err := db.QueryRow(`select count(*) from threads where id = ?`, fixture.contained.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("thread row was not rolled back: count=%d err=%v", count, err)
	}
}

func TestRemoveContainedRollsBackWhenDatabaseCommitDoesNotApply(t *testing.T) {
	fixture := newRemovalFixture(t)
	commitErr := errors.New("simulated commit failure")
	previous := commitContainedTransaction
	commitContainedTransaction = func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return commitErr
	}
	t.Cleanup(func() { commitContainedTransaction = previous })

	if _, err := RemoveContained(context.Background(), fixture.home, fixture.store, fixture.contained, fixture.container, applyRemovalOptions()); !errors.Is(err, commitErr) {
		t.Fatalf("commit failure error = %v", err)
	}
	assertRemovalState(t, fixture, true)
	if _, err := os.Lstat(fixture.contained.RolloutPath + ".codexfold-remove-pending"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("commit rollback left pending source: %v", err)
	}
	if _, err := os.Lstat(TombstonePath(fixture.store, fixture.contained.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("commit rollback left tombstone: %v", err)
	}
}

func TestRemoveContainedFinalizesWhenCommitAppliedButAcknowledgementFails(t *testing.T) {
	fixture := newRemovalFixture(t)
	ackErr := errors.New("simulated lost commit acknowledgement")
	previous := commitContainedTransaction
	commitContainedTransaction = func(tx *sql.Tx) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		return ackErr
	}
	t.Cleanup(func() { commitContainedTransaction = previous })

	result, err := RemoveContained(context.Background(), fixture.home, fixture.store, fixture.contained, fixture.container, applyRemovalOptions())
	if !errors.Is(err, ackErr) || !result.Removed {
		t.Fatalf("lost acknowledgement result=%#v err=%v", result, err)
	}
	assertRemovalState(t, fixture, false)
	data, readErr := os.ReadFile(TombstonePath(fixture.store, fixture.contained.ID))
	if readErr != nil {
		t.Fatal(readErr)
	}
	var tombstone Tombstone
	if err := json.Unmarshal(data, &tombstone); err != nil || tombstone.Phase != removalPhasePurged {
		t.Fatalf("completed tombstone=%#v err=%v", tombstone, err)
	}
}

func TestRemoveContainedRevalidatesSourceImmediatelyBeforeRename(t *testing.T) {
	fixture := newRemovalFixture(t)
	changed := []byte("changed after proof\n")
	options := applyRemovalOptions()
	options.BeforeRename = func() error {
		return os.WriteFile(fixture.contained.RolloutPath, changed, 0o644)
	}
	if _, err := RemoveContained(context.Background(), fixture.home, fixture.store, fixture.contained, fixture.container, options); err == nil {
		t.Fatal("source mutation after proof was accepted")
	}
	got, err := os.ReadFile(fixture.contained.RolloutPath)
	if err != nil || !bytes.Equal(got, changed) {
		t.Fatalf("changed source was not preserved: got=%q err=%v", got, err)
	}
	assertRemovalState(t, fixture, true)
	if _, err := os.Lstat(fixture.contained.RolloutPath + ".codexfold-remove-pending"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mutation rejection left a pending path: %v", err)
	}
	if _, err := os.Lstat(TombstonePath(fixture.store, fixture.contained.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mutation rejection left a tombstone: %v", err)
	}
}

func TestRemoveContainedRequiresWriterProbeAndRejectsActiveWriter(t *testing.T) {
	fixture := newRemovalFixture(t)
	if _, err := RemoveContained(context.Background(), fixture.home, fixture.store, fixture.contained, fixture.container, Options{Apply: true}); err == nil {
		t.Fatal("apply accepted a missing writer probe")
	}
	options := applyRemovalOptions()
	options.WriterActive = func(context.Context, codex.Session) (bool, error) { return true, nil }
	if _, err := RemoveContained(context.Background(), fixture.home, fixture.store, fixture.contained, fixture.container, options); err == nil {
		t.Fatal("apply accepted an active writer")
	}
	assertRemovalState(t, fixture, true)
}

func TestRecoverContainedFinalizesCommitInterruptedBeforePurge(t *testing.T) {
	fixture := newRemovalFixture(t)
	interrupted := errors.New("simulated process stop after commit")
	options := applyRemovalOptions()
	options.AfterCommit = func() error { return interrupted }
	result, err := RemoveContained(context.Background(), fixture.home, fixture.store, fixture.contained, fixture.container, options)
	if !errors.Is(err, interrupted) || !result.Removed {
		t.Fatalf("interrupted removal result=%#v err=%v", result, err)
	}
	pending := fixture.contained.RolloutPath + ".codexfold-remove-pending"
	purge := pending + ".purge"
	if _, err := os.Lstat(pending); err != nil {
		t.Fatalf("committed removal did not retain its pending source: %v", err)
	}
	global, err := os.ReadFile(filepath.Join(fixture.home, ".codex-global-state.json"))
	if err != nil || !bytes.Contains(global, []byte(fixture.contained.ID)) {
		t.Fatalf("test did not stop before global cleanup: err=%v global=%s", err, global)
	}

	recovered, err := RecoverContained(context.Background(), fixture.home, fixture.store, fixture.contained.ID, applyRemovalOptions())
	if err != nil {
		t.Fatalf("RecoverContained returned error: %v", err)
	}
	if !recovered.Finalized || recovered.RolledBack {
		t.Fatalf("unexpected recovery result: %#v", recovered)
	}
	assertRemovalState(t, fixture, false)
	if _, err := os.Lstat(pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery did not purge exact pending source: %v", err)
	}
	if _, err := os.Lstat(purge); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery left purge staging behind: %v", err)
	}
	data, err := os.ReadFile(TombstonePath(fixture.store, fixture.contained.ID))
	if err != nil {
		t.Fatalf("read completed tombstone: %v", err)
	}
	var tombstone Tombstone
	if err := json.Unmarshal(data, &tombstone); err != nil || tombstone.Version != 2 || tombstone.Phase != removalPhasePurged || tombstone.RemovedAt == "" {
		t.Fatalf("completed tombstone=%#v err=%v", tombstone, err)
	}
}

func TestRecoverContainedFinalizesInterruptedPurgeStaging(t *testing.T) {
	fixture := newRemovalFixture(t)
	interrupted := errors.New("simulated process stop after purge staging")
	options := applyRemovalOptions()
	options.AfterPurgeStage = func() error { return interrupted }
	result, err := RemoveContained(context.Background(), fixture.home, fixture.store, fixture.contained, fixture.container, options)
	if !errors.Is(err, interrupted) || !result.Removed {
		t.Fatalf("interrupted removal result=%#v err=%v", result, err)
	}
	pending := fixture.contained.RolloutPath + ".codexfold-remove-pending"
	purge := pending + ".purge"
	if _, err := os.Lstat(pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("purge staging left the pending name: %v", err)
	}
	if _, err := os.Lstat(purge); err != nil {
		t.Fatalf("purge staging did not retain the exact source: %v", err)
	}

	recovered, err := RecoverContained(context.Background(), fixture.home, fixture.store, fixture.contained.ID, applyRemovalOptions())
	if err != nil {
		t.Fatalf("RecoverContained returned error: %v", err)
	}
	if !recovered.Finalized || recovered.RolledBack {
		t.Fatalf("unexpected recovery result: %#v", recovered)
	}
	assertRemovalState(t, fixture, false)
	if _, err := os.Lstat(purge); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery left purge staging behind: %v", err)
	}
}

func TestRecoverContainedPreservesChangedPurgeStaging(t *testing.T) {
	fixture := newRemovalFixture(t)
	options := applyRemovalOptions()
	options.AfterPurgeStage = func() error { return errors.New("stop after staging") }
	if _, err := RemoveContained(context.Background(), fixture.home, fixture.store, fixture.contained, fixture.container, options); err == nil {
		t.Fatal("test did not stop after purge staging")
	}
	purge := fixture.contained.RolloutPath + ".codexfold-remove-pending.purge"
	changed := []byte("replacement must be preserved\n")
	if err := os.WriteFile(purge, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverContained(context.Background(), fixture.home, fixture.store, fixture.contained.ID, applyRemovalOptions()); err == nil {
		t.Fatal("recovery accepted changed purge staging")
	}
	got, err := os.ReadFile(purge)
	if err != nil || !bytes.Equal(got, changed) {
		t.Fatalf("changed purge staging was not preserved: got=%q err=%v", got, err)
	}
}

func TestRemoveContainedNeverOverwritesExistingPurgeStaging(t *testing.T) {
	fixture := newRemovalFixture(t)
	pending := fixture.contained.RolloutPath + ".codexfold-remove-pending"
	purge := pending + ".purge"
	foreign := []byte("unrelated existing file\n")
	options := applyRemovalOptions()
	options.AfterCommit = func() error {
		return os.WriteFile(purge, foreign, 0o600)
	}
	result, err := RemoveContained(context.Background(), fixture.home, fixture.store, fixture.contained, fixture.container, options)
	if err == nil || !result.Removed {
		t.Fatalf("existing purge staging result=%#v err=%v", result, err)
	}
	got, readErr := os.ReadFile(purge)
	if readErr != nil || !bytes.Equal(got, foreign) {
		t.Fatalf("existing purge staging was overwritten: got=%q err=%v", got, readErr)
	}
	if _, statErr := os.Lstat(pending); statErr != nil {
		t.Fatalf("proved pending source was not preserved: %v", statErr)
	}
}

func TestRecoverContainedRollsBackIsolatedSourceWhenDatabaseStillContainsThread(t *testing.T) {
	fixture := newRemovalFixture(t)
	manifest, err := fold.LoadManifest(fixture.store, fixture.contained.ID)
	if err != nil {
		t.Fatal(err)
	}
	pending := fixture.contained.RolloutPath + ".codexfold-remove-pending"
	purge := pending + ".purge"
	if err := os.Rename(fixture.contained.RolloutPath, pending); err != nil {
		t.Fatal(err)
	}
	tombstone := Tombstone{
		Version: 2, Kind: "contained-session-removal-v2", Phase: removalPhaseIsolated,
		PreparedAt:         time.Now().UTC().Format(time.RFC3339Nano),
		ContainedSessionID: fixture.contained.ID, ContainerSessionID: fixture.container.ID,
		OriginalRolloutPath: fixture.contained.RolloutPath, PendingRolloutPath: pending, PurgeRolloutPath: purge,
		SourceBytes: manifest.Source.Bytes, SourceSHA256: manifest.Source.SHA256,
		RecoveryManifestPath: fold.ManifestPath(fixture.store, fixture.contained.ID),
	}
	if err := writeJSONAtomically(TombstonePath(fixture.store, fixture.contained.ID), tombstone, 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := RecoverContained(context.Background(), fixture.home, fixture.store, fixture.contained.ID, applyRemovalOptions())
	if err != nil {
		t.Fatalf("RecoverContained returned error: %v", err)
	}
	if !recovered.RolledBack || recovered.Finalized {
		t.Fatalf("unexpected recovery result: %#v", recovered)
	}
	assertRemovalState(t, fixture, true)
	if _, err := os.Lstat(pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback left pending source: %v", err)
	}
	if _, err := os.Lstat(TombstonePath(fixture.store, fixture.contained.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback left tombstone: %v", err)
	}
}

func TestReplaceBytesIfUnchangedRejectsConcurrentStateChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("current"), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	if err := replaceBytesIfUnchanged(path, []byte("stale"), []byte("replacement"), 0o600); err == nil {
		t.Fatalf("concurrent state change should be rejected")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "current" {
		t.Fatalf("state changed despite rejection: got=%q err=%v", got, err)
	}
}

func TestCleanGlobalStatePreservesLargeJSONNumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	source := []byte(`{"remove":"contained","large":9007199254740993}`)
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	_, cleaned, _, _, err := cleanGlobalState(path, "contained")
	if err != nil {
		t.Fatalf("cleanGlobalState returned error: %v", err)
	}
	if !bytes.Contains(cleaned, []byte("9007199254740993")) {
		t.Fatalf("large JSON number changed during cleanup: %s", cleaned)
	}
}

func applyRemovalOptions() Options {
	return Options{
		Apply: true,
		WriterActive: func(context.Context, codex.Session) (bool, error) {
			return false, nil
		},
	}
}

type removalFixture struct {
	home      string
	store     string
	contained codex.Session
	container codex.Session
}

func newRemovalFixture(t *testing.T) removalFixture {
	t.Helper()
	home := t.TempDir()
	store := filepath.Join(home, "fold-store")
	containedPath := filepath.Join(home, "archived-contained.jsonl")
	containerPath := filepath.Join(home, "container.jsonl")
	containedSource := []byte("{\"type\":\"session_meta\",\"id\":\"contained\"}\n{\"value\":1}\n{\"value\":2}\n")
	containerSource := []byte("{\"type\":\"session_meta\",\"id\":\"container\"}\n{\"before\":true}\n{\"value\":1}\n{\"value\":2}\n{\"after\":true}\n")
	if err := os.WriteFile(containedPath, containedSource, 0o644); err != nil {
		t.Fatalf("write contained rollout: %v", err)
	}
	if err := os.WriteFile(containerPath, containerSource, 0o644); err != nil {
		t.Fatalf("write container rollout: %v", err)
	}
	contained := codex.Session{ID: "contained", Title: "Contained", CWD: "/workspace", RolloutPath: containedPath, Archived: true}
	container := codex.Session{ID: "container", Title: "Container", CWD: "/workspace", RolloutPath: containerPath}
	if _, err := fold.Fold(context.Background(), fold.Session{
		ID: contained.ID, Title: contained.Title, CWD: contained.CWD,
		RolloutPath: contained.RolloutPath, Archived: contained.Archived,
	}, fold.FoldOptions{StoreDir: store, Apply: true, FieldThreshold: 4}); err != nil {
		t.Fatalf("fold contained fixture: %v", err)
	}

	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	_, err = db.Exec(`
		create table threads (id text primary key, rollout_path text not null, archived integer not null);
		create table thread_dynamic_tools (thread_id text not null, position integer not null);
		create table thread_spawn_edges (parent_thread_id text not null, child_thread_id text not null primary key, status text not null);
		create table agent_job_items (job_id text not null, item_id text not null, assigned_thread_id text);
		insert into threads values ('contained', ?, 1), ('container', ?, 0);
		insert into thread_dynamic_tools values ('contained', 0);
		insert into thread_spawn_edges values ('container', 'contained', 'closed');
		insert into agent_job_items values ('job', 'item', 'contained');
	`, containedPath, containerPath)
	if err != nil {
		_ = db.Close()
		t.Fatalf("create state db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close state db: %v", err)
	}
	global := map[string]any{
		"pinned-thread-ids": []string{"contained", "other"},
		"thread-titles":     map[string]any{"contained": "Contained", "container": "Container"},
		"queued-follow-ups": map[string]any{"contained": map[string]any{"threadId": "contained"}},
		"unrelated":         map[string]any{"keep": true},
	}
	globalData, err := json.Marshal(global)
	if err != nil {
		t.Fatalf("encode global state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex-global-state.json"), globalData, 0o600); err != nil {
		t.Fatalf("write global state: %v", err)
	}
	return removalFixture{home: home, store: store, contained: contained, container: container}
}

func assertRemovalState(t *testing.T, fixture removalFixture, shouldExist bool) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(fixture.home, "state_5.sqlite"))
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer func() { _ = db.Close() }()
	var threadCount, toolCount, edgeCount, assignedCount int
	if err := db.QueryRow(`select count(*) from threads where id = ?`, fixture.contained.ID).Scan(&threadCount); err != nil {
		t.Fatalf("query thread: %v", err)
	}
	if err := db.QueryRow(`select count(*) from thread_dynamic_tools where thread_id = ?`, fixture.contained.ID).Scan(&toolCount); err != nil {
		t.Fatalf("query tools: %v", err)
	}
	if err := db.QueryRow(`select count(*) from thread_spawn_edges where parent_thread_id = ? or child_thread_id = ?`, fixture.contained.ID, fixture.contained.ID).Scan(&edgeCount); err != nil {
		t.Fatalf("query edges: %v", err)
	}
	if err := db.QueryRow(`select count(*) from agent_job_items where assigned_thread_id = ?`, fixture.contained.ID).Scan(&assignedCount); err != nil {
		t.Fatalf("query assignments: %v", err)
	}
	want := 0
	if shouldExist {
		want = 1
	}
	if threadCount != want || toolCount != want || edgeCount != want || assignedCount != want {
		t.Fatalf("unexpected database state: thread=%d tool=%d edge=%d assigned=%d want=%d", threadCount, toolCount, edgeCount, assignedCount, want)
	}
	_, statErr := os.Stat(fixture.contained.RolloutPath)
	if shouldExist && statErr != nil {
		t.Fatalf("contained rollout missing: %v", statErr)
	}
	if !shouldExist && !os.IsNotExist(statErr) {
		t.Fatalf("contained rollout still exists: %v", statErr)
	}
	globalData, err := os.ReadFile(filepath.Join(fixture.home, ".codex-global-state.json"))
	if err != nil {
		t.Fatalf("read global state: %v", err)
	}
	containsID := bytes.Contains(globalData, []byte(fixture.contained.ID))
	if containsID != shouldExist {
		t.Fatalf("global state reference presence = %t, want %t: %s", containsID, shouldExist, globalData)
	}
}

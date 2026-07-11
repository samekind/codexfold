package prune

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jstar0/codexfold/internal/codex"
	"github.com/jstar0/codexfold/internal/fold"
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
	result, err := RemoveContained(context.Background(), fixture.home, fixture.store, fixture.contained, fixture.container, Options{Apply: true})
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

	if _, err := RemoveContained(context.Background(), fixture.home, fixture.store, fixture.contained, fixture.container, Options{Apply: true}); err == nil {
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
	if _, err := fold.Fold(context.Background(), contained, fold.FoldOptions{StoreDir: store, Apply: true, FieldThreshold: 4}); err != nil {
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

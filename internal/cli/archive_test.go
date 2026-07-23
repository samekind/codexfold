package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	archivepkg "github.com/samekind/codexfold/internal/archive"
	"github.com/samekind/codexfold/internal/codex"
	_ "modernc.org/sqlite"
)

func TestArchiveCommandIsDryRunFirstAndMatchesOfficialApply(t *testing.T) {
	fixture := archiveCLIFixture(t)
	allowArchiveWriterProbe(t, nil)

	var output bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"archive", fixture.sessionID, "--codex-home", fixture.home, "--store", fixture.store, "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("archive dry-run: %v", err)
	}
	var dry archivepkg.Result
	if err := json.Unmarshal(output.Bytes(), &dry); err != nil || !dry.DryRun || dry.Archived {
		t.Fatalf("archive dry-run = %#v err=%v output=%s", dry, err, output.String())
	}
	if data, err := os.ReadFile(fixture.sourcePath); err != nil || string(data) != string(fixture.source) {
		t.Fatalf("dry-run changed source: %q err=%v", data, err)
	}

	output.Reset()
	root = NewRootCommand()
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"archive", fixture.sessionID, "--codex-home", fixture.home, "--store", fixture.store, "--apply", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("archive apply: %v", err)
	}
	var applied archivepkg.Result
	if err := json.Unmarshal(output.Bytes(), &applied); err != nil || applied.DryRun || !applied.Archived {
		t.Fatalf("archive apply = %#v err=%v output=%s", applied, err, output.String())
	}
	if data, err := os.ReadFile(applied.TargetPath); err != nil || string(data) != string(fixture.source) {
		t.Fatalf("archived source = %q err=%v", data, err)
	}
	var rolloutPath string
	var archived int
	if err := fixture.db.QueryRow(`select rollout_path, archived from threads where id = ?`, fixture.sessionID).Scan(&rolloutPath, &archived); err != nil {
		t.Fatal(err)
	}
	if rolloutPath != applied.TargetPath || archived != 1 {
		t.Fatalf("archive route = %s archived=%d", rolloutPath, archived)
	}
}

func TestArchiveCommandFailsClosedWhenWriterProbeFailsOrReportsWriter(t *testing.T) {
	t.Run("probe failure", func(t *testing.T) {
		fixture := archiveCLIFixture(t)
		allowArchiveWriterProbe(t, errors.New("lsof unavailable"))
		root := NewRootCommand()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{"archive", fixture.sessionID, "--codex-home", fixture.home, "--store", fixture.store, "--apply"})
		if err := root.Execute(); err == nil {
			t.Fatal("archive accepted a failed native writer probe")
		}
		assertArchiveCLIActive(t, fixture)
	})

	t.Run("active writer", func(t *testing.T) {
		fixture := archiveCLIFixture(t)
		previous := enrollmentWriterProbe
		enrollmentWriterProbe = func(context.Context, []codex.Session) (map[string]bool, error) {
			return map[string]bool{fixture.sessionID: true}, nil
		}
		t.Cleanup(func() { enrollmentWriterProbe = previous })
		root := NewRootCommand()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{"archive", fixture.sessionID, "--codex-home", fixture.home, "--store", fixture.store, "--apply"})
		if err := root.Execute(); err == nil {
			t.Fatal("archive accepted an active native writer")
		}
		assertArchiveCLIActive(t, fixture)
	})
}

func TestArchiveRecoverCommandRequiresApplyAndRestoresInterruptedRename(t *testing.T) {
	fixture := archiveCLIFixture(t)
	allowArchiveWriterProbe(t, nil)
	target := filepath.Join(fixture.home, "archived_sessions", filepath.Base(fixture.sourcePath))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(fixture.sourcePath, target); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(fixture.source)
	journal := map[string]any{
		"version": 1, "session_id": fixture.sessionID,
		"source_path": fixture.sourcePath, "target_path": target,
		"bytes": len(fixture.source), "sha256": hex.EncodeToString(digest[:]), "phase": "renamed",
	}
	journalData, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	journalPath := archivepkg.JournalPath(fixture.store, fixture.sessionID)
	if err := os.MkdirAll(filepath.Dir(journalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalPath, append(journalData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"archive", "recover", fixture.sessionID, "--codex-home", fixture.home, "--store", fixture.store})
	if err := root.Execute(); err == nil {
		t.Fatal("archive recovery ran without --apply")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("recovery preview changed target: %v", err)
	}

	var output bytes.Buffer
	root = NewRootCommand()
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"archive", "recover", fixture.sessionID, "--codex-home", fixture.home, "--store", fixture.store, "--apply", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("archive recover apply: %v", err)
	}
	var recovered archivepkg.RecoveryResult
	if err := json.Unmarshal(output.Bytes(), &recovered); err != nil || !recovered.RolledBack || recovered.Finalized {
		t.Fatalf("archive recover = %#v err=%v output=%s", recovered, err, output.String())
	}
	assertArchiveCLIActive(t, fixture)
}

type archiveCLIState struct {
	home       string
	store      string
	db         *sql.DB
	sessionID  string
	sourcePath string
	source     []byte
}

func archiveCLIFixture(t *testing.T) archiveCLIState {
	t.Helper()
	home := t.TempDir()
	store := filepath.Join(home, "fold-store")
	sourcePath := filepath.Join(home, "sessions", "2026", "07", "16", "rollout-session.jsonl")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
		t.Fatal(err)
	}
	source := []byte("{\"type\":\"session_meta\"}\n{\"value\":1}\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		create table threads (
			id text primary key, title text, cwd text, rollout_path text,
			model_provider text, model text, updated_at integer, updated_at_ms integer,
			archived integer, archived_at integer, git_branch text
		);
		insert into threads values ('session', 'Session', '/workspace', ?, 'provider', 'model', 100, 200, 0, null, 'main');
	`, sourcePath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return archiveCLIState{home: home, store: store, db: db, sessionID: "session", sourcePath: sourcePath, source: source}
}

func allowArchiveWriterProbe(t *testing.T, probeErr error) {
	t.Helper()
	previous := enrollmentWriterProbe
	enrollmentWriterProbe = func(context.Context, []codex.Session) (map[string]bool, error) {
		return map[string]bool{}, probeErr
	}
	t.Cleanup(func() { enrollmentWriterProbe = previous })
}

func assertArchiveCLIActive(t *testing.T, fixture archiveCLIState) {
	t.Helper()
	data, err := os.ReadFile(fixture.sourcePath)
	if err != nil || string(data) != string(fixture.source) {
		t.Fatalf("active source = %q err=%v", data, err)
	}
	var rolloutPath string
	var archived int
	if err := fixture.db.QueryRow(`select rollout_path, archived from threads where id = ?`, fixture.sessionID).Scan(&rolloutPath, &archived); err != nil {
		t.Fatal(err)
	}
	if rolloutPath != fixture.sourcePath || archived != 0 {
		t.Fatalf("active route = %s archived=%d", rolloutPath, archived)
	}
}

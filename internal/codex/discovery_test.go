package codex

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func BenchmarkSessionMetadataSnapshot(b *testing.B) {
	root := b.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(root, "state_5.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`create table threads (id text primary key, title text, cwd text, rollout_path text, model_provider text, model text, updated_at integer, archived integer, git_branch text)`); err != nil {
		b.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		if _, err := tx.Exec(`insert into threads values (?, 'title', '/workspace', '/rollout.jsonl', 'provider', 'model', ?, 0, '')`, fmt.Sprint(i), i); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.Run("all-3000", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := LoadSessions(root); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("managed-4", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := LoadSessionsByID(root, []string{"1", "2", "3", "4"}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestLoadSessionsReadsCodexStateDatabase(t *testing.T) {
	home := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite fixture: %v", err)
	}
	_, err = db.Exec(`
		create table threads (
			id text primary key,
			title text,
			cwd text,
			rollout_path text,
			model_provider text,
			model text,
			updated_at integer,
			archived integer,
			git_branch text
		);
		insert into threads values
			('active', 'Active session', '/workspace/a', '/rollouts/active.jsonl', 'provider', 'model-a', 200, 0, 'main'),
			('archived', 'Archived session', '/workspace/b', '/rollouts/archived.jsonl', 'provider', null, 100, 1, null);
	`)
	if err != nil {
		t.Fatalf("create sqlite fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close sqlite fixture: %v", err)
	}

	sessions, err := LoadSessions(home)
	if err != nil {
		t.Fatalf("LoadSessions returned error: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("session count = %d, want 2", len(sessions))
	}
	if sessions[0].ID != "active" || sessions[0].Archived || sessions[0].Model != "model-a" {
		t.Fatalf("unexpected active session: %#v", sessions[0])
	}
	if sessions[1].ID != "archived" || !sessions[1].Archived || sessions[1].Model != "" {
		t.Fatalf("unexpected archived session: %#v", sessions[1])
	}

	active, err := LoadSession(home, "active")
	if err != nil {
		t.Fatalf("LoadSession returned error: %v", err)
	}
	if active.ID != "active" || active.Archived || active.Model != "model-a" || active.GitBranch != "main" {
		t.Fatalf("unexpected single active session: %#v", active)
	}
	if _, err := LoadSession(home, "missing"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("LoadSession missing error = %v, want ErrSessionNotFound", err)
	}
	selected, err := LoadSessionsByID(home, []string{"archived", "missing", "' OR 1=1 --"})
	if err != nil || len(selected) != 1 || selected[0] != sessions[1] {
		t.Fatalf("selected snapshot = %#v, %v", selected, err)
	}
	writer, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec("update threads set archived=0, rollout_path='/new.jsonl' where id='archived'"); err != nil {
		t.Fatal(err)
	}
	selected, err = LoadSessionsByID(home, []string{"archived"})
	if err != nil || len(selected) != 1 || selected[0].Archived || selected[0].RolloutPath != "/new.jsonl" {
		t.Fatalf("snapshot did not reflect route update: %#v, %v", selected, err)
	}
	empty, err := LoadSessionsByID(home, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty selection scanned sessions: %#v, %v", empty, err)
	}
}

func TestResolveHomeUsesExplicitPathBeforeEnvironment(t *testing.T) {
	t.Setenv("CODEX_HOME", "/env/codex")
	got, err := ResolveHome("/explicit/codex")
	if err != nil {
		t.Fatalf("ResolveHome returned error: %v", err)
	}
	if got != filepath.Clean("/explicit/codex") {
		t.Fatalf("resolved home = %q, want explicit path", got)
	}
}

func TestSQLiteReadOnlyDSNUsesWindowsFileURI(t *testing.T) {
	got := sqliteReadOnlyDSNForOS(`C:\Users\demo\Codex Data\state_5.sqlite`, "windows")
	want := "file:///C:/Users/demo/Codex%20Data/state_5.sqlite?mode=ro"
	if got != want {
		t.Fatalf("Windows SQLite DSN = %q, want %q", got, want)
	}
}

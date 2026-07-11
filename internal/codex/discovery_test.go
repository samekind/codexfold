package codex

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

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
}

func TestResolveHomeUsesExplicitPathBeforeEnvironment(t *testing.T) {
	t.Setenv("CODEX_HOME", "/env/codex")
	got, err := ResolveHome("/explicit/codex")
	if err != nil {
		t.Fatalf("ResolveHome returned error: %v", err)
	}
	if got != "/explicit/codex" {
		t.Fatalf("resolved home = %q, want explicit path", got)
	}
}

package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jstar0/codexfold/internal/scan"
	_ "modernc.org/sqlite"
)

func TestRootExposesScanCommand(t *testing.T) {
	root := NewRootCommand()
	if _, _, err := root.Find([]string{"scan"}); err != nil {
		t.Fatalf("scan command should be exposed: %v", err)
	}
}

func TestRootUsesExplicitBuildVersion(t *testing.T) {
	previous := Version
	Version = "v-test"
	t.Cleanup(func() { Version = previous })
	if got := NewRootCommand().Version; got != "v-test" {
		t.Fatalf("root version = %q, want v-test", got)
	}
}

func TestScanCommandRunsAgainstExplicitCodexHome(t *testing.T) {
	home := t.TempDir()
	rolloutPath := filepath.Join(home, "rollout.jsonl")
	if err := os.WriteFile(rolloutPath, []byte("{\"value\":\"repeated-large-value\"}\n{\"value\":\"repeated-large-value\"}\n"), 0o644); err != nil {
		t.Fatalf("write rollout fixture: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatalf("open state fixture: %v", err)
	}
	_, err = db.Exec(`
		create table threads (
			id text primary key, title text, cwd text, rollout_path text,
			model_provider text, model text, updated_at integer,
			archived integer, git_branch text
		);
		insert into threads values ('fixture', 'Fixture', '/workspace', ?, 'provider', 'model', 1, 0, '');
	`, rolloutPath)
	if err != nil {
		t.Fatalf("create state fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close state fixture: %v", err)
	}

	var stdout bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"scan", "--all", "--codex-home", home,
		"--layers", "field", "--min-field-bytes", "8", "--json",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute scan command: %v", err)
	}
	var result scan.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode scan output: %v\n%s", err, stdout.String())
	}
	if result.SessionCount != 1 || len(result.Layers) != 1 || result.Layers[0].DuplicateOccurrences != 1 {
		t.Fatalf("unexpected scan result: %#v", result)
	}
}

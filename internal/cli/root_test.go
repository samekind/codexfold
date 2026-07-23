package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/samekind/codexfold/internal/family"
	"github.com/samekind/codexfold/internal/scan"
	_ "modernc.org/sqlite"
)

func TestRootExposesScanCommand(t *testing.T) {
	root := NewRootCommand()
	for _, name := range []string{"scan", "fork-family", "archive", "contains", "remove-contained", "fold", "unfold", "materialize", "doctor", "gc"} {
		if _, _, err := root.Find([]string{name}); err != nil {
			t.Fatalf("%s command should be exposed: %v", name, err)
		}
	}
}

func TestForkFamilyShowAndCompareUseExplicitSessions(t *testing.T) {
	home := t.TempDir()
	leftPath := filepath.Join(home, "left.jsonl")
	rightPath := filepath.Join(home, "right.jsonl")
	if err := os.WriteFile(leftPath, []byte("{\"type\":\"session_meta\",\"id\":\"left\"}\n{\"v\":1}\n{\"left\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rightPath, []byte("{\"type\":\"session_meta\",\"id\":\"right\"}\n{\"v\":1}\n{\"right\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		create table threads (
			id text primary key, title text, cwd text, rollout_path text,
			model_provider text, model text, updated_at integer,
			archived integer, git_branch text
		);
		create table thread_spawn_edges (
			parent_thread_id text not null,
			child_thread_id text not null primary key,
			status text not null
		);
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into threads values ('left', 'Left', '/workspace', ?, 'provider', 'model', 2, 0, 'main')`, leftPath); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into threads values ('right', 'Right', '/workspace', ?, 'provider', 'model', 1, 1, 'main')`, rightPath); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into thread_spawn_edges values ('left', 'right', 'closed')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	root := NewRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fork-family", "show", "left", "--codex-home", home, "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("fork-family show: %v", err)
	}
	var report family.Report
	if err := json.Unmarshal(output.Bytes(), &report); err != nil || len(report.Members) != 2 || len(report.Edges) != 1 {
		t.Fatalf("family show = %#v err=%v output=%s", report, err, output.String())
	}

	output.Reset()
	root = NewRootCommand()
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"fork-family", "compare", "left", "right", "--codex-home", home, "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("fork-family compare: %v", err)
	}
	var comparison family.Comparison
	if err := json.Unmarshal(output.Bytes(), &comparison); err != nil || comparison.Relation != family.RelationIndependentTails || comparison.GraphRelation != family.GraphAncestor {
		t.Fatalf("family comparison = %#v err=%v output=%s", comparison, err, output.String())
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
	writeStateFixture(t, home, rolloutPath)

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

func TestFoldAndUnfoldCommandsRoundTripFixture(t *testing.T) {
	home := t.TempDir()
	storeDir := filepath.Join(home, "fold-store")
	rolloutPath := filepath.Join(home, "rollout.jsonl")
	restoredPath := filepath.Join(home, "restored.jsonl")
	source := []byte("{\"value\":\"repeated-large-value\"}\n{\"value\":\"repeated-large-value\"}\n")
	if err := os.WriteFile(rolloutPath, source, 0o644); err != nil {
		t.Fatalf("write rollout fixture: %v", err)
	}
	writeStateFixture(t, home, rolloutPath)

	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"fold", "fixture", "--codex-home", home, "--store", storeDir,
		"--field-threshold", "8", "--apply", "--json",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute fold command: %v", err)
	}

	root = NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"unfold", "fixture", "--store", storeDir, "--to", restoredPath, "--json",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute unfold command: %v", err)
	}
	restored, err := os.ReadFile(restoredPath)
	if err != nil {
		t.Fatalf("read restored fixture: %v", err)
	}
	if string(restored) != string(source) {
		t.Fatalf("restored fixture differs: %q", restored)
	}
}

func writeStateFixture(t *testing.T, home string, rolloutPath string) {
	t.Helper()
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
}

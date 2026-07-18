package codex

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestLoadSpawnEdgesReturnsCurrentGraphAndAllowsMissingTable(t *testing.T) {
	home := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		create table thread_spawn_edges (
			parent_thread_id text not null,
			child_thread_id text not null primary key,
			status text not null
		);
		insert into thread_spawn_edges values ('parent', 'child-b', 'closed');
		insert into thread_spawn_edges values ('parent', 'child-a', 'open');
	`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	edges, err := LoadSpawnEdges(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 || edges[0].ChildID != "child-a" || edges[1].Status != "closed" {
		t.Fatalf("spawn edges = %#v", edges)
	}

	emptyHome := t.TempDir()
	empty, err := sql.Open("sqlite", filepath.Join(emptyHome, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Exec(`create table threads (id text primary key);`); err != nil {
		t.Fatal(err)
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	edges, err = LoadSpawnEdges(emptyHome)
	if err != nil || len(edges) != 0 {
		t.Fatalf("missing edge table = %#v err=%v", edges, err)
	}
}

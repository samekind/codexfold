//go:build windows

package cli

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/enroll"
)

func TestEnrollmentNormalizesOnlyIdenticalWindowsAlias(t *testing.T) {
	home, _, _ := fsFixture(t, true)
	plain := filepath.Join(home, "sessions", "alias.jsonl")
	if err := os.MkdirAll(filepath.Dir(plain), 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("{\"type\":\"session_meta\"}\n")
	if err := os.WriteFile(plain, content, 0o600); err != nil {
		t.Fatal(err)
	}
	alias := `\\?\` + plain
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`update threads set rollout_path=? where id='session'`, alias); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(alias)
	if err != nil {
		t.Fatal(err)
	}
	d := enroll.Decision{SessionID: "session", RolloutPath: alias, Fingerprint: enroll.Fingerprint{Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano()}}
	changed := d
	changed.Fingerprint.Size++
	if err := normalizeEnrollmentAlias(context.Background(), home, &changed); err == nil {
		t.Fatal("changed source accepted")
	}
	var route string
	if err := db.QueryRow(`select rollout_path from threads where id='session'`).Scan(&route); err != nil || route != alias {
		t.Fatalf("failed check changed route: %s %v", route, err)
	}
	if err := normalizeEnrollmentAlias(context.Background(), home, &d); err != nil {
		t.Fatal(err)
	}
	sessions, err := codex.LoadSessions(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].RolloutPath != plain || d.RolloutPath != plain {
		t.Fatalf("route not normalized: %#v", sessions)
	}
	data, err := os.ReadFile(alias)
	if err != nil || string(data) != string(content) {
		t.Fatalf("alias bytes changed: %v", err)
	}
	if _, err := canonicalNativeRoute(home, filepath.Join(home, "native"), d.RolloutPath); err != nil {
		t.Fatal(err)
	}
}

package codex

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestRouteSessionOptimisticallyUpdatesExpectedPath(t *testing.T) {
	home, nativePath := routeFixture(t)
	virtualPath := filepath.Join(home, "mounted", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(virtualPath), 0o700); err != nil {
		t.Fatalf("create mount fixture: %v", err)
	}
	data := []byte("current virtual bytes\n")
	if err := os.WriteFile(virtualPath, data, 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	result, err := RouteSession(context.Background(), RouteOptions{CodexHome: home, SessionID: "session", ExpectedPath: nativePath, Target: RouteTarget{Path: virtualPath, Bytes: int64(len(data)), SHA256: routeDigest(data)}})
	if err != nil {
		t.Fatalf("RouteSession returned error: %v", err)
	}
	if result.PreviousPath != nativePath || result.CurrentPath != virtualPath {
		t.Fatalf("unexpected route result: %#v", result)
	}
	if got := queryRoute(t, home); got != virtualPath {
		t.Fatalf("database route = %q, want %q", got, virtualPath)
	}
}

func TestRouteSessionRejectsConcurrentOrStaleExpectedPath(t *testing.T) {
	home, nativePath := routeFixture(t)
	changedPath := filepath.Join(home, "changed.jsonl")
	if err := updateRoute(t, home, changedPath); err != nil {
		t.Fatalf("change route: %v", err)
	}
	target := filepath.Join(home, "target.jsonl")
	data := []byte("target")
	_ = os.WriteFile(target, data, 0o600)
	if _, err := RouteSession(context.Background(), RouteOptions{CodexHome: home, SessionID: "session", ExpectedPath: nativePath, Target: RouteTarget{Path: target, Bytes: int64(len(data)), SHA256: routeDigest(data)}}); err == nil {
		t.Fatal("RouteSession should reject a stale expected path")
	}
	if got := queryRoute(t, home); got != changedPath {
		t.Fatalf("rejected transaction changed route to %q", got)
	}
}

func TestRouteSessionRejectsStaleOrCorruptFallbackBytes(t *testing.T) {
	home, nativePath := routeFixture(t)
	target := filepath.Join(home, "fallback.jsonl")
	if err := os.WriteFile(target, []byte("old snapshot"), 0o600); err != nil {
		t.Fatalf("write fallback: %v", err)
	}
	current := []byte("latest bytes")
	if _, err := RouteSession(context.Background(), RouteOptions{CodexHome: home, SessionID: "session", ExpectedPath: nativePath, Target: RouteTarget{Path: target, Bytes: int64(len(current)), SHA256: routeDigest(current)}}); err == nil {
		t.Fatal("RouteSession should reject a target not equal to current-byte metadata")
	}
	if got := queryRoute(t, home); got != nativePath {
		t.Fatalf("corrupt fallback changed route to %q", got)
	}
}

func routeFixture(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	nativePath := filepath.Join(home, "native.jsonl")
	if err := os.WriteFile(nativePath, []byte("native\n"), 0o600); err != nil {
		t.Fatalf("write native: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	_, err = db.Exec(`create table threads (id text primary key, rollout_path text not null); insert into threads values ('session', ?)`, nativePath)
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("create state: %v", err)
	}
	return home, nativePath
}

func queryRoute(t *testing.T, home string) string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer db.Close()
	var path string
	if err := db.QueryRow(`select rollout_path from threads where id = 'session'`).Scan(&path); err != nil {
		t.Fatalf("query route: %v", err)
	}
	return path
}

func updateRoute(t *testing.T, home, path string) error {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`update threads set rollout_path = ? where id = 'session'`, path)
	return err
}

func routeDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

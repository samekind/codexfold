package sessionns

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestActivateAndDeactivatePreserveSessionTrees(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mount := filepath.Join(root, "mount")
	nativeRoot := filepath.Join(home, "fold-native")
	writeFixture(t, filepath.Join(home, "sessions", "2026", "07", "12", "active.jsonl"), "active\n")
	writeFixture(t, filepath.Join(home, "archived_sessions", "archived.jsonl"), "archived\n")
	createStateDatabase(t, home)
	for _, directory := range []string{"sessions", "archived_sessions"} {
		if err := os.MkdirAll(filepath.Join(mount, directory), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(nativeRoot, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	result, err := Activate(Options{Home: home, Mount: mount, NativeRoot: nativeRoot, MountProbe: healthyMountProbe})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Active || result.Recovered {
		t.Fatalf("activation result = %#v", result)
	}
	assertLink(t, filepath.Join(home, "sessions"), filepath.Join(mount, "sessions"))
	assertLink(t, filepath.Join(home, "archived_sessions"), filepath.Join(mount, "archived_sessions"))
	assertFile(t, filepath.Join(nativeRoot, "sessions", "2026", "07", "12", "active.jsonl"), "active\n")
	assertFile(t, filepath.Join(nativeRoot, "archived_sessions", "archived.jsonl"), "archived\n")

	status, err := Inspect(Options{Home: home, Mount: mount, NativeRoot: nativeRoot})
	if err != nil || !status.Active {
		t.Fatalf("active status = %#v err=%v", status, err)
	}
	result, err = Deactivate(Options{Home: home, Mount: mount, NativeRoot: nativeRoot})
	if err != nil {
		t.Fatal(err)
	}
	if result.Active {
		t.Fatalf("deactivation result = %#v", result)
	}
	assertFile(t, filepath.Join(home, "sessions", "2026", "07", "12", "active.jsonl"), "active\n")
	assertFile(t, filepath.Join(home, "archived_sessions", "archived.jsonl"), "archived\n")
	for _, directory := range []string{"sessions", "archived_sessions"} {
		info, err := os.Lstat(filepath.Join(home, directory))
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("restored %s = %#v err=%v", directory, info, err)
		}
	}
}

func TestActivateRejectsOrdinaryDirectoryThatLooksLikeMount(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mount := filepath.Join(root, "mount")
	nativeRoot := filepath.Join(home, "fold-native")
	writeFixture(t, filepath.Join(home, "sessions", "active.jsonl"), "active\n")
	writeFixture(t, filepath.Join(home, "archived_sessions", "archived.jsonl"), "archived\n")
	for _, directory := range []string{"sessions", "archived_sessions"} {
		if err := os.MkdirAll(filepath.Join(mount, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	_, err := Activate(Options{
		Home: home, Mount: mount, NativeRoot: nativeRoot,
		MountProbe: func(string) error { return os.ErrInvalid },
	})
	if err == nil {
		t.Fatal("activation must reject an ordinary directory even when it has canonical subdirectories")
	}
	assertFile(t, filepath.Join(home, "sessions", "active.jsonl"), "active\n")
	assertFile(t, filepath.Join(home, "archived_sessions", "archived.jsonl"), "archived\n")
}

func TestActiveNamespaceNormalizesDesktopMountAliasesInStateDatabase(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mount := filepath.Join(home, "fold-fs")
	nativeRoot := filepath.Join(home, "fold-native")
	activeRoute := filepath.Join(home, "sessions", "2026", "07", "13", "rollout-session.jsonl")
	writeFixture(t, activeRoute, "active\n")
	if err := os.MkdirAll(filepath.Join(home, "archived_sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, directory := range sessionDirectories {
		if err := os.MkdirAll(filepath.Join(mount, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`create table threads (id text primary key, rollout_path text not null); insert into threads values ('session', ?)`, activeRoute); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	options := Options{Home: home, Mount: mount, NativeRoot: nativeRoot, MountProbe: healthyMountProbe}
	if _, err := Activate(options); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	mountAlias := filepath.Join(mount, "sessions", "2026", "07", "13", "rollout-session.jsonl")
	if _, err := db.Exec(`update threads set rollout_path = ? where id = 'session'`, mountAlias); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	var normalized string
	if err := db.QueryRow(`select rollout_path from threads where id = 'session'`).Scan(&normalized); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if filepath.Clean(normalized) != filepath.Clean(activeRoute) {
		_ = db.Close()
		t.Fatalf("normalized route = %q, want %q", normalized, activeRoute)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Deactivate(options); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var triggers int
	if err := db.QueryRow(`select count(*) from sqlite_master where type = 'trigger' and name like 'codexfold_normalize_rollout_path_%'`).Scan(&triggers); err != nil {
		t.Fatal(err)
	}
	if triggers != 0 {
		t.Fatalf("route normalization triggers remained after deactivation: %d", triggers)
	}
}

func TestRouteGuardNormalizesMountAliasesWithUnicodePaths(t *testing.T) {
	home := filepath.Join(t.TempDir(), "用户", ".codex")
	mount := filepath.Join(home, "fold-fs")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	createStateDatabase(t, home)
	options := Options{Home: home, Mount: mount, NativeRoot: filepath.Join(home, "fold-native")}
	if err := installRouteGuard(options); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mountAlias := filepath.Join(mount, "archived_sessions", "rollout-session.jsonl")
	if _, err := database.Exec(`insert into threads values ('session', ?)`, mountAlias); err != nil {
		t.Fatal(err)
	}
	var normalized string
	if err := database.QueryRow(`select rollout_path from threads where id = 'session'`).Scan(&normalized); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "archived_sessions", "rollout-session.jsonl")
	if filepath.Clean(normalized) != filepath.Clean(want) {
		t.Fatalf("normalized Unicode route = %q, want %q", normalized, want)
	}
}

func TestRecoverRollsBackInterruptedActivation(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mount := filepath.Join(root, "mount")
	nativeRoot := filepath.Join(home, "fold-native")
	writeFixture(t, filepath.Join(home, "sessions", "active.jsonl"), "active\n")
	writeFixture(t, filepath.Join(home, "archived_sessions", "archived.jsonl"), "archived\n")
	if err := os.MkdirAll(nativeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(home, "sessions"), filepath.Join(nativeRoot, "sessions")); err != nil {
		t.Fatal(err)
	}
	options := Options{Home: home, Mount: mount, NativeRoot: nativeRoot}
	if err := writeJournal(options, journal{Version: 1, Action: actionActivate}); err != nil {
		t.Fatal(err)
	}

	result, err := Recover(options)
	if err != nil {
		t.Fatal(err)
	}
	if result.Active || !result.Recovered {
		t.Fatalf("recovery result = %#v", result)
	}
	assertFile(t, filepath.Join(home, "sessions", "active.jsonl"), "active\n")
	assertFile(t, filepath.Join(home, "archived_sessions", "archived.jsonl"), "archived\n")
	if _, err := os.Stat(journalPath(options)); !os.IsNotExist(err) {
		t.Fatalf("journal remained after recovery: %v", err)
	}
}

func writeFixture(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, path string, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("file %s = %q err=%v", path, got, err)
	}
}

func assertLink(t *testing.T, path string, want string) {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil || filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("link %s = %q err=%v", path, got, err)
	}
}

func healthyMountProbe(string) error { return nil }

func createStateDatabase(t *testing.T, home string) {
	t.Helper()
	database, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`create table threads (id text primary key, rollout_path text not null)`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
}

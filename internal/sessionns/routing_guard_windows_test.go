//go:build windows

package sessionns

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samekind/codexfold/internal/mountfs"
)

func TestWindowsRouteGuardNormalizesDriveAndUNCAliases(t *testing.T) {
	home := t.TempDir()
	options := Options{Home: home, Mount: `T:\`, NativeRoot: filepath.Join(home, "native")}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`create table threads (id text primary key, rollout_path text); insert into threads values ('thread','native')`); err != nil {
		t.Fatal(err)
	}
	if err := installRouteGuard(options); err != nil {
		t.Fatal(err)
	}
	unc := mountfs.WindowsUNCPath(home, options.Mount)
	for _, root := range []string{`t:\`, `\\?\T:\`, unc, `\\?\UNC\` + unc[2:]} {
		for _, namespace := range sessionDirectories {
			alias := filepath.Join(root, namespace, "2026", "rollout.jsonl")
			if _, err := db.Exec(`update threads set rollout_path=? where id='thread'`, alias); err != nil {
				t.Fatal(err)
			}
			var actual string
			if err := db.QueryRow(`select rollout_path from threads where id='thread'`).Scan(&actual); err != nil {
				t.Fatal(err)
			}
			expected := filepath.Join(home, namespace, "2026", "rollout.jsonl")
			if actual != expected {
				t.Fatalf("alias %q normalized to %q, wanted %q", alias, actual, expected)
			}
		}
	}
	foreign := `\\unrelated\share\sessions\rollout.jsonl`
	if _, err := db.Exec(`update threads set rollout_path=? where id='thread'`, foreign); err != nil {
		t.Fatal(err)
	}
	var actual string
	if err := db.QueryRow(`select rollout_path from threads where id='thread'`).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	if actual != foreign {
		t.Fatal("unrelated UNC route changed")
	}
}

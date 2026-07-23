package service

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDefinitionUpdatePromotesAndCommitsExistingDefinition(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "service.plist")
	if err := os.WriteFile(target, []byte("old-definition"), 0o600); err != nil {
		t.Fatal(err)
	}
	update, err := StageDefinitionUpdate(target, []byte("new-definition"))
	if err != nil {
		t.Fatal(err)
	}
	if err := update.Promote(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "new-definition" {
		t.Fatalf("promoted definition=%q err=%v", data, err)
	}
	if err := update.Commit(); err != nil {
		t.Fatal(err)
	}
	assertNoDefinitionUpdateArtifacts(t, root)
}

func TestDefinitionUpdateRollsBackExistingAndNewDefinitions(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		name string
		old  []byte
	}{
		{name: "existing", old: []byte("old-definition")},
		{name: "new"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(root, test.name)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "service.plist")
			if test.old != nil {
				if err := os.WriteFile(target, test.old, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			update, err := StageDefinitionUpdate(target, []byte("new-definition"))
			if err != nil {
				t.Fatal(err)
			}
			if err := update.Promote(); err != nil {
				t.Fatal(err)
			}
			if err := update.Rollback(); err != nil {
				t.Fatal(err)
			}
			if test.old == nil {
				if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("new definition remained after rollback: %v", err)
				}
			} else if data, err := os.ReadFile(target); err != nil || string(data) != string(test.old) {
				t.Fatalf("rolled back definition=%q err=%v", data, err)
			}
			if err := update.Commit(); err != nil {
				t.Fatal(err)
			}
			assertNoDefinitionUpdateArtifacts(t, dir)
		})
	}
}

func assertNoDefinitionUpdateArtifacts(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Name()) >= len(".codexfold-definition-") && entry.Name()[:len(".codexfold-definition-")] == ".codexfold-definition-" {
			t.Fatalf("definition update artifact remained: %s", entry.Name())
		}
	}
}

package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jstar0/codexfold/internal/buildid"
)

func TestBinaryUpdatePromotesAndCommitsAtomically(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "codexfold")
	candidate := filepath.Join(root, "candidate")
	if err := os.WriteFile(target, []byte("old-build"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte("new-build"), 0o700); err != nil {
		t.Fatal(err)
	}
	update, err := StageBinaryUpdate(candidate, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := update.Promote(); err != nil {
		t.Fatal(err)
	}
	if digest, err := buildid.FileSHA256(target); err != nil || digest != update.CandidateSHA256 {
		t.Fatalf("promoted digest=%s err=%v", digest, err)
	}
	if err := update.Commit(); err != nil {
		t.Fatal(err)
	}
	assertNoBinaryUpdateArtifacts(t, root)
}

func TestBinaryUpdateRollsBackPromotedCandidate(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "codexfold")
	candidate := filepath.Join(root, "candidate")
	if err := os.WriteFile(target, []byte("old-build"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte("new-build"), 0o700); err != nil {
		t.Fatal(err)
	}
	update, err := StageBinaryUpdate(candidate, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := update.Promote(); err != nil {
		t.Fatal(err)
	}
	if err := update.Rollback(); err != nil {
		t.Fatal(err)
	}
	if digest, err := buildid.FileSHA256(target); err != nil || digest != update.CurrentSHA256 {
		t.Fatalf("rolled back digest=%s err=%v", digest, err)
	}
	if err := update.Commit(); err != nil {
		t.Fatal(err)
	}
	assertNoBinaryUpdateArtifacts(t, root)
}

func assertNoBinaryUpdateArtifacts(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Name()) >= len(".codexfold-") && entry.Name()[:len(".codexfold-")] == ".codexfold-" {
			t.Fatalf("binary update artifact remained: %s", entry.Name())
		}
	}
}

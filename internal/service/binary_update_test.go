package service

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/samekind/codexfold/internal/buildid"
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

func TestPreserveBinaryForRecoveryKeepsExecutableBytes(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "installed")
	if err := os.WriteFile(source, []byte("signed-previous-build"), 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := PreserveBinaryForRecovery(source, filepath.Join(root, "Recovery"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "signed-previous-build" {
		t.Fatalf("preserved binary=%q err=%v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("preserved binary is not executable: info=%v err=%v", info, err)
	}
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

func TestBinaryUpdateInstallsAndCommitsNewTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "service", "codexfold")
	candidate := filepath.Join(root, "candidate")
	if err := os.WriteFile(candidate, []byte("first-build"), 0o755); err != nil {
		t.Fatal(err)
	}
	update, err := StageBinaryUpdate(candidate, target)
	if err != nil {
		t.Fatal(err)
	}
	if update.HadTarget || update.CurrentSHA256 != "" {
		t.Fatalf("first install update = %#v", update)
	}
	if err := update.Promote(); err != nil {
		t.Fatal(err)
	}
	if digest, err := buildid.FileSHA256(target); err != nil || digest != update.CandidateSHA256 {
		t.Fatalf("installed digest=%s err=%v", digest, err)
	}
	if err := update.Commit(); err != nil {
		t.Fatal(err)
	}
	assertNoBinaryUpdateArtifacts(t, filepath.Dir(target))
}

func TestBinaryUpdateRollsBackNewTargetWithoutRemovingForeignFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "service", "codexfold")
	candidate := filepath.Join(root, "candidate")
	if err := os.WriteFile(candidate, []byte("first-build"), 0o700); err != nil {
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
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rolled-back first install left target: %v", err)
	}
	if err := update.Commit(); err != nil {
		t.Fatal(err)
	}

	update, err = StageBinaryUpdate(candidate, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("foreign"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := update.Promote(); err == nil {
		t.Fatal("first install replaced a target that appeared after staging")
	}
	if err := update.Rollback(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "foreign" {
		t.Fatalf("rollback removed foreign target: data=%q err=%v", data, err)
	}
	if err := update.Commit(); err != nil {
		t.Fatal(err)
	}
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

package mountfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareMountPointRejectsOrdinaryFiles(t *testing.T) {
	mountPoint := filepath.Join(t.TempDir(), "mount")
	if err := os.MkdirAll(filepath.Join(mountPoint, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountPoint, "sessions", "stale.jsonl"), []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := prepareMountPoint(mountPoint); err == nil {
		t.Fatal("non-empty ordinary directory must not be accepted as a mount backing directory")
	}
}

func TestPrepareMountPointCreatesAndSealsMissingBackingDirectory(t *testing.T) {
	mountPoint := filepath.Join(t.TempDir(), "missing", "mount")
	if err := prepareMountPoint(mountPoint); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(mountPoint)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o500 {
		t.Fatalf("created mount backing mode=%#o directory=%t", info.Mode().Perm(), info.IsDir())
	}
}

func TestPrepareMountPointSealsEmptyBackingDirectory(t *testing.T) {
	mountPoint := filepath.Join(t.TempDir(), "mount")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := prepareMountPoint(mountPoint); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(mountPoint)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o200 != 0 {
		t.Fatalf("unmounted backing directory remained writable: mode=%#o", info.Mode().Perm())
	}
	if err := os.Mkdir(filepath.Join(mountPoint, "sessions"), 0o700); err == nil {
		t.Fatal("sealed unmounted backing directory accepted a namespace write")
	}
}

func TestPrepareMountPointRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	mountPoint := filepath.Join(root, "mount")
	if err := os.Symlink(target, mountPoint); err != nil {
		t.Fatal(err)
	}

	if err := prepareMountPoint(mountPoint); err == nil {
		t.Fatal("mount backing path must not be a symlink")
	}
}

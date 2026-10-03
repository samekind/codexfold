//go:build windows

package mountfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareWinFspMountPointLeavesAbsentDirectory(t *testing.T) {
	for _, exists := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "parent", "mount")
		if exists {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := prepareMountPoint(path); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("mount point must be absent before WinFsp starts: %v", err)
		}
		if err := os.WriteFile(filepath.Join(path, "unexpected.jsonl"), []byte("write"), 0o600); err == nil {
			t.Fatal("unmounted path accepted a write")
		}
		if info, err := os.Stat(filepath.Dir(path)); err != nil || !info.IsDir() {
			t.Fatalf("mount parent missing: %v", err)
		}
	}
}

func TestPrepareWinFspMountPointPreservesOrdinaryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mount")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareMountPoint(path); err == nil {
		t.Fatal("ordinary file accepted")
	}
	if contents, err := os.ReadFile(path); err != nil || string(contents) != "keep" {
		t.Fatalf("ordinary file changed: %q %v", contents, err)
	}
}

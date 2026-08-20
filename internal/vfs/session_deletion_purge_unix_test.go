//go:build !windows

package vfs

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSessionDeletionPurgePreservesNestedSpecialFile(t *testing.T) {
	root, _, tombstone := sessionDeletionFixture(t)
	stageSessionDeletionQuarantine(t, tombstone)
	special := filepath.Join(tombstone.RetiredSessionPath, "retained-native", "nested", "foreign-fifo")
	if err := os.MkdirAll(filepath.Dir(special), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(special, 0o600); err != nil {
		t.Skipf("named pipes unavailable: %v", err)
	}

	if completed, err := AdvanceSessionDeletion(root, tombstone); completed || err == nil {
		t.Fatalf("nested special-file purge completed=%t err=%v", completed, err)
	}
	if info, err := os.Lstat(special); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("nested special file was not preserved: info=%v err=%v", info, err)
	}
}

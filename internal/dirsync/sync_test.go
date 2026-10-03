package dirsync

import (
	"os"
	"testing"
)

func TestDirectoryAndClosedHandle(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := Sync(directory); err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Sync(directory); err == nil {
		t.Fatal("closed directory was accepted")
	}
}

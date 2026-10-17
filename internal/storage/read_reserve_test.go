package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadReserveIsBoundedAndDoesNotRemoveForeignMaterial(t *testing.T) {
	root := t.TempDir()
	PrepareReadReserve(root)
	path := filepath.Join(root, readReserveName)
	info, err := os.Stat(path)
	if err != nil || info.Size() != readReserveBytes {
		t.Fatalf("reserve: %v %v", info, err)
	}
	if releaseReadReserveFor(t.TempDir()) {
		t.Fatal("released reserve for unrelated store")
	}
	if !releaseReadReserveFor(filepath.Join(root, "fs", "sessions", "test", "leases")) {
		t.Fatal("could not release owned reserve for reader lease")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("reserve remains: %v", err)
	}
	foreign := make([]byte, readReserveBytes)
	if err := os.WriteFile(path, foreign, 0600); err != nil {
		t.Fatal(err)
	}
	if releaseReadReserveFor(filepath.Join(root, "packs", "test", "leases")) {
		t.Fatal("removed unproven material")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

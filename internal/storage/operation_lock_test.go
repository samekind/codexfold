package storage

import (
	"strings"
	"testing"
)

func TestOperationLockExcludesSecondMutation(t *testing.T) {
	store := t.TempDir()
	first, err := AcquireOperationLock(store, "objects")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireOperationLock(store, "objects"); err == nil || !strings.Contains(err.Error(), "already held") {
		t.Fatalf("second lock error = %v, want held rejection", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireOperationLock(store, "objects")
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

//go:build windows

package mountfs

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
)

func TestWindowsNativeRepairPreservesOpenReader(t *testing.T) {
	original := append(append([]byte(nil), repairFixtureRecords(0, 2)...), heapGarbage...)
	root, path := nativePreflightFixture(t, original)
	reader, err := openNativeBacking(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	repair, changed, err := repairNativeRollout(context.Background(), root, path, 0)
	if err != nil || !changed || repair.QuarantinePath == "" {
		t.Fatalf("repair: changed=%t err=%v report=%+v", changed, err, repair)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, repairFixtureRecords(0, 2)) {
		t.Fatalf("repaired content: %q %v", got, err)
	}
	old, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(old, original) {
		t.Fatalf("existing reader changed: %v", err)
	}
	manifest := readQuarantine(t, repair.QuarantinePath)
	if len(manifest.Ranges) != 1 {
		t.Fatalf("quarantine ranges: %+v", manifest.Ranges)
	}
}

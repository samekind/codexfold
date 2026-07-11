package scan

import (
	"crypto/sha256"
	"path/filepath"
	"testing"
)

func TestDedupIndexTracksExactDuplicateSavings(t *testing.T) {
	index, err := openDedupIndex(filepath.Join(t.TempDir(), "dedup.sqlite"))
	if err != nil {
		t.Fatalf("open dedup index: %v", err)
	}
	t.Cleanup(func() {
		if err := index.Close(); err != nil {
			t.Errorf("close dedup index: %v", err)
		}
	})

	repeated := sha256.Sum256([]byte("repeated"))
	unique := sha256.Sum256([]byte("unique"))
	for range 3 {
		if err := index.ObserveAt("record", repeated, 100, "/payload/output"); err != nil {
			t.Fatalf("observe repeated object: %v", err)
		}
	}
	if err := index.Observe("record", unique, 20); err != nil {
		t.Fatalf("observe unique object: %v", err)
	}

	stats, err := index.LayerStats("record")
	if err != nil {
		t.Fatalf("load layer stats: %v", err)
	}
	if stats.ObjectCount != 4 {
		t.Fatalf("object count = %d, want 4", stats.ObjectCount)
	}
	if stats.UniqueObjectCount != 2 {
		t.Fatalf("unique object count = %d, want 2", stats.UniqueObjectCount)
	}
	if stats.TotalBytes != 320 {
		t.Fatalf("total bytes = %d, want 320", stats.TotalBytes)
	}
	if stats.UniqueBytes != 120 {
		t.Fatalf("unique bytes = %d, want 120", stats.UniqueBytes)
	}
	if stats.DuplicateOccurrences != 2 {
		t.Fatalf("duplicate occurrences = %d, want 2", stats.DuplicateOccurrences)
	}
	if stats.DuplicateBytes != 200 {
		t.Fatalf("duplicate bytes = %d, want 200", stats.DuplicateBytes)
	}

	top, err := index.TopObjects("record", 1)
	if err != nil {
		t.Fatalf("load top duplicate objects: %v", err)
	}
	if len(top) != 1 || top[0].Occurrences != 3 || top[0].DuplicateBytes != 200 || top[0].SamplePath != "/payload/output" {
		t.Fatalf("top duplicate objects = %#v, want repeated object with 3 occurrences and 200 duplicate bytes", top)
	}
}

package scan

import (
	"context"
	"crypto/sha256"
	"path/filepath"
	"testing"
)

func TestDuplicateRecordIndexAggregatesOccurrencesAcrossFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.sqlite")
	index, err := openDedupIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	repeated := sha256.Sum256([]byte("repeated record"))
	unique := sha256.Sum256([]byte("unique record"))
	for _, file := range []string{"first.jsonl", "second.jsonl"} {
		if err := index.BeginFile(file); err != nil {
			t.Fatal(err)
		}
		if err := index.Observe(LayerRecord, repeated, 15); err != nil {
			t.Fatal(err)
		}
		if file == "first.jsonl" {
			if err := index.Observe(LayerRecord, unique, 13); err != nil {
				t.Fatal(err)
			}
		}
		if err := index.CommitFile(); err != nil {
			t.Fatal(err)
		}
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	lookup, err := OpenDuplicateRecordIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lookup.Close()
	duplicate, err := lookup.IsDuplicateRecord(context.Background(), repeated, 15)
	if err != nil || !duplicate {
		t.Fatalf("repeated record lookup = %t, %v", duplicate, err)
	}
	duplicate, err = lookup.IsDuplicateRecord(context.Background(), unique, 13)
	if err != nil || duplicate {
		t.Fatalf("unique record lookup = %t, %v", duplicate, err)
	}
}

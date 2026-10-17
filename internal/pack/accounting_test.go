package pack

import (
	"bytes"
	"context"
	"testing"
)

func TestPublishedLogicalBytesExcludeUnpublishedWork(t *testing.T) {
	store := t.TempDir()
	first := bytes.Repeat([]byte("already-published-session"), 10000)
	writeManifest(t, store, "first", putObjects(t, store, first))
	result, err := Build(context.Background(), store, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second := bytes.Repeat([]byte("still-being-folded"), 10000)
	writeManifest(t, store, "second", putObjects(t, store, second))
	logical, err := PublishedLogicalBytes(context.Background(), store, result.Generation)
	if err != nil || logical != int64(len(first)) {
		t.Fatalf("published basis = %d, %v", logical, err)
	}
	result, err = Build(context.Background(), store, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	logical, err = PublishedLogicalBytes(context.Background(), store, result.Generation)
	if err != nil || logical != int64(len(first)+len(second)) {
		t.Fatalf("next publication basis = %d, %v", logical, err)
	}
}

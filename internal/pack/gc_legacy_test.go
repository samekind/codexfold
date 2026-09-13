package pack

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/samekind/codexfold/internal/storage"
)

func TestLegacyPackWithoutPublicationIsRetainedWithoutBlockingGC(t *testing.T) {
	store := t.TempDir()
	directory := filepath.Join(store, "packs", "gen-123")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	content := filepath.Join(directory, "pack-000001.pack")
	if err := os.WriteFile(content, []byte("legacy bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	guard, err := AuthorizeGenerationRemoval(context.Background(), store, storage.GCCandidate{
		Kind: storage.CandidatePackGeneration, Path: directory,
	})
	if err != nil || guard != nil {
		t.Fatalf("unproved legacy pack must be deferred: guard=%v err=%v", guard, err)
	}
	if data, err := os.ReadFile(content); err != nil || string(data) != "legacy bytes" {
		t.Fatalf("legacy bytes changed: %q %v", data, err)
	}
}

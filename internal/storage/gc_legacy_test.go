package storage

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCollectRetainsUnprovedLegacyPackAndContinuesOtherCandidates(t *testing.T) {
	store := t.TempDir()
	writeBytesFile(t, filepath.Join(store, "packs", "CURRENT"), []byte("gen-3\n"))
	for i, generation := range []string{"gen-0", "gen-1", "gen-2", "gen-3"} {
		dir := filepath.Join(store, "packs", generation)
		writeSizedFile(t, filepath.Join(dir, "pack-000001.pack"), 4096)
		writeJSONFile(t, filepath.Join(dir, "index.json"), map[string]any{"generation": generation})
		setModTime(t, dir, time.Unix(2_000_000, 0).Add(time.Duration(i)*time.Minute))
	}
	var authorized []string
	result, err := Collect(context.Background(), GCOptions{
		StoreDir: store, Apply: true, KeepPackGenerations: 2,
		AuthorizePackGenerationRemoval: func(_ context.Context, candidate GCCandidate) (PackGenerationRemovalGuard, error) {
			generation := filepath.Base(candidate.Path)
			authorized = append(authorized, generation)
			if generation == "gen-0" {
				return nil, nil
			}
			return testPackRemovalGuard{}, nil
		},
	})
	if err != nil {
		t.Fatalf("unproved legacy pack aborted collection: %v", err)
	}
	if !reflect.DeepEqual(authorized, []string{"gen-0", "gen-1"}) {
		t.Fatalf("authorizer calls=%v", authorized)
	}
	if result.RemovedCount != 1 {
		t.Fatalf("removed=%d, want 1; result=%#v", result.RemovedCount, result)
	}
	for _, generation := range []string{"gen-0", "gen-2", "gen-3"} {
		if _, err := os.Stat(filepath.Join(store, "packs", generation, "pack-000001.pack")); err != nil {
			t.Fatalf("retained generation %s missing: %v", generation, err)
		}
	}
	if _, err := os.Stat(filepath.Join(store, "packs", "gen-1")); !os.IsNotExist(err) {
		t.Fatalf("proved candidate not removed: %v", err)
	}
}

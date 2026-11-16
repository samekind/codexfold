package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/fold"
)

func TestEnrollmentDryRunPricerMeasuresAndCaches(t *testing.T) {
	store := t.TempDir()
	content := bytes.Repeat([]byte("{\"type\":\"response_item\",\"text\":\"a long shared prefix that every fork repeats\"}\n"), 20000)
	foldFixtureManifest(t, store, "original", "", content)
	fork := filepath.Join(t.TempDir(), "rollout-fork.jsonl")
	if err := os.WriteFile(fork, content, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(fork)
	if err != nil {
		t.Fatal(err)
	}
	price := enrollmentDryRunPricer(store)
	session := codex.Session{ID: "fork", RolloutPath: fork, Archived: true}
	newBytes, err := price(context.Background(), session, info)
	if err != nil {
		t.Fatal(err)
	}
	if newBytes != 0 {
		t.Fatalf("fork of stored content priced at %d new bytes", newBytes)
	}
	if _, err := os.Stat(fold.ManifestPath(store, "fork")); !os.IsNotExist(err) {
		t.Fatalf("pricing wrote a manifest: %v", err)
	}
	// A cached price is reused for the same file identity.
	prices := map[string]enrollmentPrice{}
	data, err := os.ReadFile(enrollmentPricePath(store))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &prices); err != nil {
		t.Fatal(err)
	}
	cached := prices["fork"]
	cached.NewBytes = 12345
	prices["fork"] = cached
	data, _ = json.Marshal(prices)
	if err := os.WriteFile(enrollmentPricePath(store), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if again, err := price(context.Background(), session, info); err != nil || again != 12345 {
		t.Fatalf("cached price = %d, %v", again, err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(fork, later, later); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(fork)
	if again, err := price(context.Background(), session, info); err != nil || again != 0 {
		t.Fatalf("changed file price = %d, %v", again, err)
	}
}

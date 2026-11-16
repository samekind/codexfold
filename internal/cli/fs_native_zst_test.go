package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/enroll"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/pack"
)

func foldFixtureManifest(t *testing.T, store string, id string, manifestPath string, content []byte) {
	t.Helper()
	source := filepath.Join(t.TempDir(), id+".jsonl")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	options := fold.FoldOptions{StoreDir: store, Apply: true}
	if manifestPath != "" {
		if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
			t.Fatal(err)
		}
		options.ManifestPathOverride = manifestPath
	}
	if _, err := fold.Fold(context.Background(), fold.Session{ID: id, RolloutPath: source, Archived: true}, options); err != nil {
		t.Fatal(err)
	}
}

func TestPackPendingSeesNestedAndDroppedManifests(t *testing.T) {
	store := t.TempDir()
	foldFixtureManifest(t, store, "session-a", "", bytes.Repeat([]byte("{\"a\":\"one\"}\n"), 400))
	foldFixtureManifest(t, store, "session-b", "", bytes.Repeat([]byte("{\"b\":\"two\"}\n"), 400))
	if _, err := pack.Build(context.Background(), store, pack.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	for _, id := range []string{"session-a", "session-b"} {
		if err := os.Chtimes(fold.ManifestPath(store, id), past, past); err != nil {
			t.Fatal(err)
		}
	}
	if pending, err := enrollmentHasManifestsNewerThanPack(store); err != nil || pending {
		t.Fatalf("fresh pack pending=%t err=%v", pending, err)
	}

	nested := filepath.Join(store, "manifests", "native-zst", "zst-000000000000000000000000-1.json")
	foldFixtureManifest(t, store, "zst-000000000000000000000000-1", nested, bytes.Repeat([]byte("{\"c\":\"three\"}\n"), 400))
	if pending, err := enrollmentHasManifestsNewerThanPack(store); err != nil || !pending {
		t.Fatalf("nested manifest pending=%t err=%v", pending, err)
	}
	if _, err := pack.Build(context.Background(), store, pack.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(nested, past, past); err != nil {
		t.Fatal(err)
	}
	if pending, err := enrollmentHasManifestsNewerThanPack(store); err != nil || pending {
		t.Fatalf("after rebuild pending=%t err=%v", pending, err)
	}

	// A deleted session leaves its objects in the pack until a compaction.
	if err := os.Remove(fold.ManifestPath(store, "session-b")); err != nil {
		t.Fatal(err)
	}
	if pending, err := enrollmentHasManifestsNewerThanPack(store); err != nil || !pending {
		t.Fatalf("dropped manifest pending=%t err=%v", pending, err)
	}
}

func TestNativeZstRestoreRefusesWhileFoldingIsEnabled(t *testing.T) {
	home := t.TempDir()
	store := filepath.Join(home, "fold-store")
	if err := enroll.SaveControl(enroll.ControlPath(store), enroll.Control{Enabled: true, Interval: 30 * time.Minute, StableFor: time.Hour}); err != nil {
		t.Fatal(err)
	}
	command := newFSNativeZstRestoreCommand()
	command.SetArgs([]string{"--codex-home", home, "--all", "--apply"})
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "automatic folding is enabled") {
		t.Fatalf("restore with folding enabled = %v", err)
	}
	if err := enroll.SaveControl(enroll.ControlPath(store), enroll.Control{Enabled: false, Interval: 30 * time.Minute, StableFor: time.Hour}); err != nil {
		t.Fatal(err)
	}
	command = newFSNativeZstRestoreCommand()
	command.SetArgs([]string{"--codex-home", home, "--all", "--apply"})
	command.SetOut(io.Discard)
	if err := command.Execute(); err != nil {
		t.Fatalf("restore with folding disabled = %v", err)
	}
}

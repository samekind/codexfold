package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/enroll"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/vfs"
)

func namespaceReplacementFixture(t *testing.T) (*mountfs.Filesystem, string, string, []byte) {
	t.Helper()
	store, native := t.TempDir(), t.TempDir()
	route := "/sessions/rollout.jsonl"
	source := filepath.Join(t.TempDir(), "original")
	if err := os.WriteFile(source, []byte("{\"original\":true}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := fold.Fold(context.Background(), fold.Session{ID: "logical", RolloutPath: source}, fold.FoldOptions{StoreDir: store, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pack.Build(context.Background(), store, pack.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	manifest, err := fold.LoadManifest(store, "logical")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := retainCanonicalSnapshot(context.Background(), store, "logical", vfs.NativeFile{Path: source, Bytes: result.SourceBytes, SHA256: result.SourceSHA256}, nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{Root: store, ManifestPath: fold.ManifestPath(store, "logical"), Manifest: manifest, Reader: mountfs.NamespaceReader(store, nil), NativeSnapshot: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	filesystem := mountfs.NewCanonical()
	filesystem.SetNativeRoot(native)
	if err := filesystem.SetManagedStore(store); err != nil {
		t.Fatal(err)
	}
	if err := filesystem.AddSessionAt("logical", route, session); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { filesystem.CloseSessions() })
	if err := os.MkdirAll(filepath.Join(native, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	want := []byte("{\"replacement\":true}\n")
	if err := os.WriteFile(filepath.Join(native, "sessions", ".rebind"), want, 0600); err != nil {
		t.Fatal(err)
	}
	if errno := filesystem.Rename(route, route+".bak"); errno != 0 {
		t.Fatal(errno)
	}
	if errno := filesystem.Rename("/sessions/.rebind", route); errno != 0 {
		t.Fatal(errno)
	}
	return filesystem, store, native, want
}

func TestNamespaceReplacementUsesRetainedSnapshotAndLooseReader(t *testing.T) {
	filesystem, store, native, want := namespaceReplacementFixture(t)
	if err := filesystem.FoldNativeReplacements(context.Background(), func(ctx context.Context, source, id string) (*vfs.Session, io.Closer, error) {
		return prepareNamespaceReplacement(ctx, store, source, id)
	}); err != nil {
		t.Fatal(err)
	}
	routes, err := mountfs.ManagedNamespaceRoutes(store)
	if err != nil {
		t.Fatal(err)
	}
	var current string
	for id, route := range routes {
		if route == "/sessions/rollout.jsonl" {
			current = id
		}
	}
	if current == "" || current == "logical" {
		t.Fatalf("replacement identity: %s", current)
	}
	state, err := managedState(store, current)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := recoverInterruptedCanonicalMigration(t.TempDir(), store, native, state); err != nil || changed {
		t.Fatalf("namespace inode was mistaken for a SQLite migration: %t %v", changed, err)
	}
	if state.NativeSnapshot.Path == filepath.Join(native, "sessions", "rollout.jsonl") {
		t.Fatal("cutover reused mutable native source as proof")
	}
	if got, err := os.ReadFile(state.NativeSnapshot.Path); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("retained snapshot mismatch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(native, "sessions", "rollout.jsonl")); !os.IsNotExist(err) {
		t.Fatal("native source was not removed")
	}
	reopened, resolver, err := openManagedNamespaceSession(context.Background(), store, state)
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	reader, err := reopened.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got := make([]byte, len(want))
	if n, err := reader.ReadAt(context.Background(), got, 0); err != nil || n != len(want) || !bytes.Equal(got, want) {
		t.Fatalf("reopened replacement: n=%d err=%v", n, err)
	}

	managed := make(map[string]struct{})
	home, mount := t.TempDir(), t.TempDir()
	sessions := []codex.Session{{ID: "logical", RolloutPath: filepath.Join(home, "sessions", "rollout.jsonl")}}
	if err := addNamespaceManagedSessions(store, home, mount, sessions, managed); err != nil {
		t.Fatal(err)
	}
	if _, ok := managed["logical"]; !ok {
		t.Fatal("replacement lost logical enrollment identity")
	}
}

func TestNamespaceReplacementHonorsDisabledPolicy(t *testing.T) {
	filesystem, store, native, want := namespaceReplacementFixture(t)
	if err := enroll.SaveControl(enroll.ControlPath(store), enroll.Control{Enabled: false, Interval: time.Minute, StableFor: time.Hour}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var diagnostics bytes.Buffer
	done := startNamespaceRetirement(ctx, filesystem, store, func() bool { return resolveEnrollmentControl(enrollmentFlags{storeDir: store}, time.Minute).Enabled }, &diagnostics)
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done
	if got, err := os.ReadFile(filepath.Join(native, "sessions", "rollout.jsonl")); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("disabled policy folded/replaced native source: %v", err)
	}
	if diagnostics.Len() != 0 {
		t.Fatal(diagnostics.String())
	}
}

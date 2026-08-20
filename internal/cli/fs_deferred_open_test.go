package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/vfs"
)

func TestOpenManagedSessionDeferredReadsExactStateAndRejectsWriter(t *testing.T) {
	store, state, want := deferredManagedSessionFixture(t)
	directory := filepath.Join(store, "fs", "sessions", state.SessionID)
	leasePath := filepath.Join(directory, "writer.lease")
	if err := os.WriteFile(leasePath, []byte("stale-owned-loader-lease\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotDeferredManagedSession(t, directory)

	managed, resolver, err := openManagedSessionDeferred(context.Background(), store, state)
	if err != nil {
		t.Fatalf("openManagedSessionDeferred: %v", err)
	}
	defer resolver.Close()
	handle, err := managed.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, handle.Size())
	n, readErr := handle.ReadAt(context.Background(), got, 0)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		t.Fatal(readErr)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:n], want) {
		t.Fatalf("deferred managed bytes = %q, want %q", got[:n], want)
	}
	if _, err := managed.OpenWriter(); !errors.Is(err, vfs.ErrSessionRecoveryDeferred) {
		t.Fatalf("OpenWriter error = %v, want ErrSessionRecoveryDeferred", err)
	}
	after := snapshotDeferredManagedSession(t, directory)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("owned loader changed managed session artifacts:\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestOpenManagedSessionDeferredClosesResolverAfterStateFailure(t *testing.T) {
	store, state, _ := deferredManagedSessionFixture(t)
	directory := filepath.Join(store, "fs", "sessions", state.SessionID)
	statePath := filepath.Join(directory, "state.json")
	if err := os.WriteFile(statePath, []byte("broken-state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotDeferredManagedSession(t, directory)
	generation, err := pack.CurrentGeneration(store)
	if err != nil {
		t.Fatal(err)
	}
	packLeaseDirectory := filepath.Join(store, "packs", generation, "leases")
	assertNoPackResolverLeases(t, packLeaseDirectory)

	managed, resolver, err := openManagedSessionDeferred(context.Background(), store, state)
	if !errors.Is(err, vfs.ErrSessionRecoveryDeferred) {
		t.Fatalf("openManagedSessionDeferred error = %v, want ErrSessionRecoveryDeferred", err)
	}
	if managed != nil || resolver != nil {
		t.Fatalf("failed deferred open returned resources: managed=%v resolver=%v", managed, resolver)
	}
	after := snapshotDeferredManagedSession(t, directory)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("failed owned loader repaired managed state:\nbefore=%#v\nafter=%#v", before, after)
	}
	assertNoPackResolverLeases(t, packLeaseDirectory)
}

func deferredManagedSessionFixture(t *testing.T) (string, vfs.SessionState, []byte) {
	t.Helper()
	_, store, nativePath := fsFixture(t, true)
	manifest, err := fold.LoadManifest(store, "session")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := pack.Open(store, pack.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	managed, openErr := vfs.OpenSession(context.Background(), vfs.SessionOptions{
		Root: store, ManifestPath: fold.ManifestPath(store, manifest.Session.ID), Manifest: manifest, Reader: resolver,
		NativeSnapshot: vfs.NativeFile{Path: nativePath, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256},
	})
	closeErr := resolver.Close()
	if err := errors.Join(openErr, closeErr); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	return store, managed.State(), want
}

type deferredManagedEntry struct {
	Mode fs.FileMode
	Data []byte
	Link string
}

func snapshotDeferredManagedSession(t *testing.T, directory string) map[string]deferredManagedEntry {
	t.Helper()
	snapshot := make(map[string]deferredManagedEntry)
	if err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		if relative == "leases" || strings.HasPrefix(relative, "leases"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		record := deferredManagedEntry{Mode: info.Mode()}
		switch {
		case info.Mode().IsRegular():
			record.Data, err = os.ReadFile(path)
		case info.Mode()&os.ModeSymlink != 0:
			record.Link, err = os.Readlink(path)
		}
		if err != nil {
			return err
		}
		snapshot[relative] = record
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertNoPackResolverLeases(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".lease-resolver-") {
			t.Fatalf("resolver lease remained after deferred open failure: %s", filepath.Join(directory, entry.Name()))
		}
	}
}

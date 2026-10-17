package pack

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
)

func TestRetireLooseYieldsToForegroundAndCanResume(t *testing.T) {
	root := t.TempDir()
	source := bytes.Repeat([]byte("foreground retirement bytes\n"), 1000)
	refs := putObjects(t, root, source)
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	state := persistManagedManifestFixture(t, root, "session", source)
	manifest, err := fold.LoadManifest(root, "session")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	session, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{Root: root, ManifestPath: state.ManifestPath, Manifest: manifest, Reader: resolver, NativeSnapshot: state.NativeSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	background := make(chan error, 1)
	go func() {
		_, err := RetireLoose(context.Background(), root, RetireLooseOptions{Apply: true, BeforeRemove: func(string) error {
			close(ready)
			// Keep the verified deletion boundary open until the foreground has
			// actually requested priority, then allow the watcher to cancel it.
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				entries, _ := os.ReadDir(root + "/fs/maintenance")
				for _, entry := range entries {
					if len(entry.Name()) > 6 && entry.Name()[len(entry.Name())-6:] == ".yield" {
						time.Sleep(30 * time.Millisecond)
						return nil
					}
				}
				time.Sleep(time.Millisecond)
			}
			return errors.New("foreground did not request maintenance yielding")
		}})
		background <- err
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("retirement did not reach its deletion boundary")
	}
	writer, err := session.OpenForegroundWriter()
	if err != nil {
		t.Fatalf("foreground writer did not preempt real retirement: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-background; !errors.Is(err, storage.ErrManagedSessionBusy) {
		t.Fatalf("yield did not defer retirement: %v", err)
	}
	if _, err := os.Stat(fold.NewObjectStore(root).ObjectPath(refs[0].SHA256)); err != nil {
		t.Fatalf("cancelled retirement removed its uncommitted candidate: %v", err)
	}
	result, err := RetireLoose(context.Background(), root, RetireLooseOptions{Apply: true})
	if err != nil || result.RetiredCount != 1 {
		t.Fatalf("yielded cleanup could not resume: result=%+v err=%v", result, err)
	}
}

func TestPackGCYieldsToForegroundAndRestoresStagedGeneration(t *testing.T) {
	root := t.TempDir()
	source := []byte("pack GC foreground bytes\n")
	refs := putObjects(t, root, source)
	writeManifest(t, root, "session", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	state := persistManagedManifestFixture(t, root, "session", source)
	second, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := fold.LoadManifest(root, "session")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	session, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{Root: root, ManifestPath: state.ManifestPath, Manifest: manifest, Reader: resolver, NativeSnapshot: state.NativeSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	background := make(chan error, 1)
	go func() {
		_, err := storage.Collect(context.Background(), storage.GCOptions{
			StoreDir: root, Apply: true, KeepPackGenerations: 1,
			AuthorizePackGenerationRemoval: func(ctx context.Context, candidate storage.GCCandidate) (storage.PackGenerationRemovalGuard, error) {
				return AuthorizeGenerationRemoval(ctx, root, candidate)
			},
			BeforePackStage: func(storage.GCCandidate) error {
				close(ready)
				deadline := time.Now().Add(time.Second)
				for time.Now().Before(deadline) {
					entries, _ := os.ReadDir(filepath.Join(root, "fs", "maintenance"))
					for _, entry := range entries {
						if strings.HasSuffix(entry.Name(), ".yield") {
							time.Sleep(30 * time.Millisecond)
							return nil
						}
					}
					time.Sleep(time.Millisecond)
				}
				return errors.New("foreground did not request GC yielding")
			},
		})
		background <- err
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("pack cleanup did not reach its staging boundary")
	}
	writer, err := session.OpenForegroundWriter()
	if err != nil {
		t.Fatalf("foreground could not preempt old-pack cleanup: %v", err)
	}
	defer writer.Close()
	if err := <-background; !errors.Is(err, storage.ErrManagedSessionBusy) {
		t.Fatalf("pack cleanup yielding classification: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "packs", first.Generation)); err != nil {
		t.Fatalf("yielded cleanup did not restore staged generation: %v", err)
	}
	if current, err := CurrentGeneration(root); err != nil || current != second.Generation {
		t.Fatalf("foreground interruption changed CURRENT: %q %v", current, err)
	}
}

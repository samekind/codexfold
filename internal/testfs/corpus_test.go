package testfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jstar0/codexfold/internal/fold"
	"github.com/jstar0/codexfold/internal/fsctl"
	"github.com/jstar0/codexfold/internal/pack"
	"github.com/jstar0/codexfold/internal/vfs"
)

func TestGenerateIsDeterministicAndContainsForkAndNonPrefixDuplication(t *testing.T) {
	first, err := Generate(filepath.Join(t.TempDir(), "first"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Generate(filepath.Join(t.TempDir(), "second"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Sessions) != len(second.Sessions) || len(first.Sessions) < 4 {
		t.Fatalf("unexpected corpora: %#v %#v", first, second)
	}
	for index := range first.Sessions {
		if first.Sessions[index].ID != second.Sessions[index].ID || first.Sessions[index].SHA256 != second.Sessions[index].SHA256 || first.Sessions[index].Bytes != second.Sessions[index].Bytes {
			t.Fatalf("corpus is not deterministic at %d: %#v %#v", index, first.Sessions[index], second.Sessions[index])
		}
	}
	if first.Sessions[0].SHA256 == first.Sessions[1].SHA256 || first.Sessions[2].Bytes <= first.Sessions[0].Bytes {
		t.Fatalf("fork and reordered fixtures are not distinct: %#v", first.Sessions)
	}
}

func TestPackedCorpusShadowRandomReadsAndWritableSessionStress(t *testing.T) {
	root := t.TempDir()
	corpus, err := Generate(filepath.Join(root, "corpus"), Options{LargeFieldBytes: 768 << 10, RepeatedRecords: 128})
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(root, "store")
	for _, fixture := range corpus.Sessions {
		_, err := fold.Fold(context.Background(), fold.Session{ID: fixture.ID, RolloutPath: fixture.Path, Archived: true}, fold.FoldOptions{StoreDir: store, Apply: true, FieldThreshold: 32})
		if err != nil {
			t.Fatalf("fold %s: %v", fixture.ID, err)
		}
	}
	if _, err := pack.Build(context.Background(), store, pack.BuildOptions{}); err != nil {
		t.Fatalf("pack build: %v", err)
	}
	resolver, err := pack.Open(store, pack.OpenOptions{CacheBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	for _, fixture := range corpus.Sessions {
		manifest, err := fold.LoadManifest(store, fixture.ID)
		if err != nil {
			t.Fatal(err)
		}
		view, err := vfs.NewView(manifest, resolver)
		if err != nil {
			t.Fatal(err)
		}
		shadow, err := fsctl.Shadow(context.Background(), fixture.Path, view, fsctl.ShadowOptions{BlockBytes: 64 << 10, RandomReads: 10000, Seed: 42})
		if err != nil || !shadow.Verified {
			t.Fatalf("shadow %s: %#v err=%v", fixture.ID, shadow, err)
		}
	}
	fixture := corpus.Sessions[0]
	manifest, _ := fold.LoadManifest(store, fixture.ID)
	managed, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{Root: store, ManifestPath: fold.ManifestPath(store, fixture.ID), Manifest: manifest, Reader: resolver, NativeSnapshot: vfs.NativeFile{Path: fixture.Path, Bytes: fixture.Bytes, SHA256: fixture.SHA256}})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := managed.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for index := 0; index < 100000; index++ {
		if _, err := writer.Append(context.Background(), []byte("x")); err != nil {
			t.Fatalf("append %d: %v", index, err)
		}
	}
	if err := writer.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if testing.Verbose() {
		t.Logf("100000 appends: %s", time.Since(start))
	}
	current, err := managed.MaterializeCurrent(context.Background(), filepath.Join(root, "current.jsonl"), false)
	if err != nil || current.Bytes != fixture.Bytes+100000 {
		t.Fatalf("append result: %#v err=%v", current, err)
	}
	verifyConcurrentReadAndWrite(t, managed)
	writer, err = managed.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteAt(context.Background(), []byte("PATCH"), 7); err != nil {
		t.Fatal(err)
	}
	if err := writer.Truncate(context.Background(), current.Bytes/2); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	info, err := managed.VisibleInfo()
	if err != nil || info.Size != current.Bytes/2 {
		t.Fatalf("COW/truncate result: %#v err=%v", info, err)
	}
}

func TestGenerateRolloutWritesExactRequestedBytes(t *testing.T) {
	fixture, err := GenerateRollout(filepath.Join(t.TempDir(), "large.jsonl"), (17<<20)+37)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(fixture.Path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != fixture.Bytes || hex.EncodeToString(digest[:]) != fixture.SHA256 {
		t.Fatalf("generated rollout metadata differs: %#v", fixture)
	}
}

func TestFaultHookFiresExactlyOnceAtRequestedPhase(t *testing.T) {
	faults := NewFaults("state-publish")
	if err := faults.Hook("prepare"); err != nil {
		t.Fatal(err)
	}
	if err := faults.Hook("state-publish"); err == nil || !faults.Fired() {
		t.Fatalf("fault did not fire: %v", err)
	}
	if err := faults.Hook("state-publish"); err != nil {
		t.Fatalf("fault fired twice: %v", err)
	}
}

func verifyConcurrentReadAndWrite(t *testing.T, managed *vfs.Session) {
	t.Helper()
	writer, err := managed.OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 5)
	for worker := 0; worker < 5; worker++ {
		wait.Add(1)
		go func(seed int64) {
			defer wait.Done()
			random := rand.New(rand.NewSource(seed))
			for index := 0; index < 1000; index++ {
				reader, err := managed.OpenReader()
				if err != nil {
					errorsSeen <- err
					return
				}
				if reader.Size() > 0 {
					offset := random.Int63n(reader.Size())
					buffer := make([]byte, 1)
					if _, err := reader.ReadAt(context.Background(), buffer, offset); err != nil && !errors.Is(err, io.EOF) {
						_ = reader.Close()
						errorsSeen <- err
						return
					}
				}
				_ = reader.Close()
			}
		}(int64(worker + 1))
	}
	for index := 0; index < 1000; index++ {
		if _, err := writer.Append(context.Background(), []byte("y")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatal(err)
	}
}

package pack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/samekind/codexfold/internal/storage"
)

func TestSequentialObjectStreamKeepsOnlyCurrentDecodedBlock(t *testing.T) {
	root := t.TempDir()
	source := bytes.Repeat([]byte("bounded-stream"), 200000)
	refs := putObjects(t, root, source)
	writeManifest(t, root, "sample", refs)
	if _, err := Build(context.Background(), root, BuildOptions{BlockBytes: 256 << 10}); err != nil {
		t.Fatal(err)
	}
	resolver, err := Open(root, OpenOptions{CacheBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	reader, err := resolver.OpenObject(context.Background(), refs[0])
	if err != nil {
		t.Fatal(err)
	}
	stream := reader.(*objectStream)
	prefix := make([]byte, 2)
	if _, err := reader.Read(prefix[:1]); err != nil {
		t.Fatal(err)
	}
	first := &stream.blockData[0]
	if _, err := reader.Read(prefix[1:]); err != nil {
		t.Fatal(err)
	}
	if first != &stream.blockData[0] {
		t.Fatal("small sequential reads decoded the same block again")
	}
	if len(stream.blockData) > 256<<10 {
		t.Fatal("stream cache exceeded one block")
	}
	rest, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(append(prefix, rest...), source) {
		t.Fatalf("stream bytes differ: %v", err)
	}
	reader.Close()
	if stream.blockData != nil {
		t.Fatal("closed stream retained block")
	}
	if _, err := reader.Read(prefix); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed read: %v", err)
	}
}

func TestIncrementalBuildSharesImmutablePayloadAndSurvivesOldGenerationGC(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, bytes.Repeat([]byte("shared-payload"), 100000))
	writeManifest(t, root, "first", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RetireLoose(context.Background(), root, RetireLooseOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	second, err := Build(context.Background(), root, BuildOptions{Incremental: true})
	if err != nil {
		t.Fatal(err)
	}
	oldFiles, err := filepath.Glob(filepath.Join(root, "packs", first.Generation, "*.pack"))
	if err != nil || len(oldFiles) != 1 {
		t.Fatal("missing initial payload")
	}
	newFiles, err := filepath.Glob(filepath.Join(root, "packs", second.Generation, "*.pack"))
	if err != nil || len(newFiles) != 1 {
		t.Fatal("missing incremental payload")
	}
	a, err := os.Stat(oldFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(newFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Fatal("incremental build duplicated payload")
	}
	gc, err := storage.Collect(context.Background(), storage.GCOptions{StoreDir: root, Apply: true, KeepPackGenerations: 1, AuthorizePackGenerationRemoval: func(ctx context.Context, c storage.GCCandidate) (storage.PackGenerationRemovalGuard, error) {
		return AuthorizeGenerationRemoval(ctx, root, c)
	}})
	if err != nil || gc.RemovedCount != 1 {
		t.Fatalf("old generation GC: %+v %v", gc, err)
	}
	if _, err := os.Stat(filepath.Join(root, "packs", first.Generation)); !os.IsNotExist(err) {
		t.Fatal("old generation survived")
	}
	report, err := Doctor(context.Background(), root)
	if err != nil || report.IssueCount != 0 {
		t.Fatalf("shared payload lost during GC: %+v %v", report, err)
	}
}

func TestIncrementalBuildCompactsWhenObjectsAreDeleted(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, bytes.Repeat([]byte("first"), 100000), bytes.Repeat([]byte("second"), 100000))
	writeManifest(t, root, "first", refs[:1])
	writeManifest(t, root, "second", refs[1:])
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RetireLoose(context.Background(), root, RetireLooseOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "manifests", "first.json")); err != nil {
		t.Fatal(err)
	}
	second, err := Build(context.Background(), root, BuildOptions{Incremental: true})
	if err != nil {
		t.Fatal(err)
	}
	oldFiles, _ := filepath.Glob(filepath.Join(root, "packs", first.Generation, "*.pack"))
	newFiles, _ := filepath.Glob(filepath.Join(root, "packs", second.Generation, "*.pack"))
	a, err := os.Stat(oldFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(newFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(a, b) {
		t.Fatal("deleted object's storage was retained by incremental linking")
	}
	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	if resolver.HasObject(refs[0]) || !resolver.HasObject(refs[1]) {
		t.Fatal("compacted object set is incorrect")
	}
}

type cappedBuildChecker struct {
	limit int64
	seen  int64
}

func (c *cappedBuildChecker) Check(_ context.Context, projection storage.Projection) (storage.Assessment, error) {
	c.seen = projection.TemporaryBytes
	if c.seen > c.limit {
		return storage.Assessment{}, storage.ErrBudgetExceeded
	}
	return storage.Assessment{}, nil
}

func TestCompactionBudgetCountsCopiedEncodedBlocksInsteadOfRawCorpus(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, bytes.Repeat([]byte{0}, 70<<20), []byte("obsolete"))
	writeManifest(t, root, "retained", refs[:1])
	writeManifest(t, root, "deleted", refs[1:])
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := RetireLoose(context.Background(), root, RetireLooseOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "manifests", "deleted.json")); err != nil {
		t.Fatal(err)
	}
	checker := &cappedBuildChecker{limit: 80 << 20}
	result, err := Build(context.Background(), root, BuildOptions{Incremental: true, Budget: checker})
	if err != nil {
		t.Fatalf("compaction should fit its encoded-byte budget: %v (projection=%d)", err, checker.seen)
	}
	if checker.seen == 0 || checker.seen > checker.limit || result.ReusedEncodedObjects == 0 {
		t.Fatalf("compaction projection=%d result=%+v", checker.seen, result)
	}
	if report, err := Doctor(context.Background(), root); err != nil || report.IssueCount != 0 {
		t.Fatalf("compacted pack verification: %+v %v", report, err)
	}
}

func TestFullPackFilesDoNotForceCompactionEveryCycle(t *testing.T) {
	root := t.TempDir()
	resolver := &Resolver{packs: make(map[string]*os.File)}
	for i := 0; i < 17; i++ {
		name := fmt.Sprintf("pack-%02d", i)
		file, err := os.Create(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		if err := file.Truncate(100); err != nil {
			t.Fatal(err)
		}
		resolver.packs[name] = file
	}
	limit, err := compactPackFileLimit(resolver, 100)
	if err != nil || limit <= len(resolver.packs) {
		t.Fatalf("full pack limit=%d err=%v", limit, err)
	}
	for _, file := range resolver.packs {
		if err := file.Truncate(10); err != nil {
			t.Fatal(err)
		}
	}
	limit, err = compactPackFileLimit(resolver, 100)
	if err != nil || limit != 16 {
		t.Fatalf("fragmented pack limit=%d err=%v", limit, err)
	}
}

func TestBuildReusesEncodedObjectsButRejectsCorruptSourcePack(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, bytes.Repeat([]byte("compressed-reuse"), 100000))
	writeManifest(t, root, "sample", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := RetireLoose(context.Background(), root, RetireLooseOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ReusedEncodedObjects != 1 {
		t.Fatalf("reused=%d", result.ReusedEncodedObjects)
	}
	report, err := Doctor(context.Background(), root)
	if err != nil || report.IssueCount != 0 {
		t.Fatalf("new pack proof failed: %v %+v", err, report)
	}
	resolver, err := Open(root, OpenOptions{CacheBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	object, _, err := resolver.lookupObject(refs[0].SHA256)
	resolver.Close()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "packs", result.Generation, object.Blocks[0].Pack)
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte{0}
	if _, err := file.ReadAt(data, object.Blocks[0].PackOffset); err != nil {
		t.Fatal(err)
	}
	data[0] ^= 255
	if _, err := file.WriteAt(data, object.Blocks[0].PackOffset); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := Build(context.Background(), root, BuildOptions{}); err == nil {
		t.Fatal("published copied corrupt compressed data")
	}
	generation, err := CurrentGeneration(root)
	if err != nil || generation != result.Generation {
		t.Fatalf("failed build changed CURRENT: %s %v", generation, err)
	}
}

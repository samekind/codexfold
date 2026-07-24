package pack

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/storage"
)

func TestBuildAndResolverReadExactRandomRanges(t *testing.T) {
	root := t.TempDir()
	large := bytes.Repeat([]byte("large-object-block-"), 50000)
	refs := putObjects(t, root, []byte("shared-small-object"), large)
	writeManifest(t, root, "first", []fold.ObjectRef{refs[0], refs[1], refs[0]})
	writeManifest(t, root, "fork", []fold.ObjectRef{refs[0], refs[1]})

	result, err := Build(context.Background(), root, BuildOptions{BlockBytes: 256 << 10, PackBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if result.ObjectCount != 2 || result.BlockCount < 3 || result.PackCount < 1 {
		t.Fatalf("unexpected build result: %#v", result)
	}
	current, err := CurrentGeneration(root)
	if err != nil || current != result.Generation {
		t.Fatalf("current generation = %q, %v; want %q", current, err, result.Generation)
	}
	loose := fold.NewObjectStore(root)
	for _, ref := range refs {
		if err := os.Remove(loose.ObjectPath(ref.SHA256)); err != nil {
			t.Fatalf("remove loose object after pack build: %v", err)
		}
	}

	resolver, err := Open(root, OpenOptions{CacheBytes: 512 << 10})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() { _ = resolver.Close() })
	if resolver.Generation() != result.Generation {
		t.Fatalf("resolver generation = %q, want %q", resolver.Generation(), result.Generation)
	}

	for _, test := range []struct {
		name string
		ref  fold.ObjectRef
		data []byte
		off  int64
		size int
	}{
		{name: "small", ref: refs[0], data: []byte("shared-small-object"), off: 2, size: 8},
		{name: "large-first", ref: refs[1], data: large, off: 17, size: 333},
		{name: "large-block-boundary", ref: refs[1], data: large, off: (256 << 10) - 31, size: 1000},
		{name: "large-tail", ref: refs[1], data: large, off: int64(len(large) - 101), size: 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			buffer := make([]byte, test.size)
			n, readErr := resolver.ReadAt(context.Background(), test.ref, buffer, test.off)
			end := int(test.off) + test.size
			if end > len(test.data) {
				end = len(test.data)
			}
			want := test.data[int(test.off):end]
			if !bytes.Equal(buffer[:n], want) {
				t.Fatalf("ReadAt bytes differ: got=%d want=%d", n, len(want))
			}
			if len(want) < test.size && !errors.Is(readErr, io.EOF) {
				t.Fatalf("ReadAt error = %v, want EOF", readErr)
			}
		})
	}
}

func TestResolverReusesDecoderAcrossDistinctBlocks(t *testing.T) {
	root := t.TempDir()
	value := bytes.Repeat([]byte("decoder-pool-block-data"), 20000)
	refs := putObjects(t, root, value)
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{BlockBytes: 64 << 10, PackBytes: 1 << 20}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	resolver, err := Open(root, OpenOptions{CacheBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = resolver.Close() })

	created := 0
	resolver.decoderFactory = func() (*zstd.Decoder, error) {
		created++
		return newPackDecoder()
	}
	for _, offset := range []int64{0, 70 << 10} {
		if _, err := resolver.ReadAt(context.Background(), refs[0], make([]byte, 1), offset); err != nil {
			t.Fatalf("ReadAt(%d): %v", offset, err)
		}
	}
	if created != 1 {
		t.Fatalf("decoder creations = %d, want 1", created)
	}
}

func TestBlockCacheDoesNotRetainOversizedCallerBacking(t *testing.T) {
	cache := newBlockCache(64)
	value := make([]byte, 16, 4096)
	copy(value, []byte("sixteen-byte-val"))
	cache.put("object:0", value)

	cached, ok := cache.get("object:0")
	if !ok {
		t.Fatal("cache rejected a value within its byte budget")
	}
	if len(cached) != len(value) || cap(cached) != len(cached) {
		t.Fatalf("cached slice len=%d cap=%d, want exact owned storage", len(cached), cap(cached))
	}
	value[0] = 'X'
	if cached[0] == value[0] {
		t.Fatal("cache retained the caller's oversized backing array")
	}
	if cache.used != int64(len(cached)) {
		t.Fatalf("cache used=%d, want %d", cache.used, len(cached))
	}
}

func TestBuildSelectsRawAndZstdBlocksAndResolverReadsBoth(t *testing.T) {
	root := t.TempDir()
	raw := make([]byte, 8<<10)
	if _, err := cryptorand.Read(raw); err != nil {
		t.Fatal(err)
	}
	compressible := bytes.Repeat([]byte("highly-compressible-value"), 400)
	refs := putObjects(t, root, raw, compressible)
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	index := loadCurrentIndex(t, root)
	encodings := make(map[string]string, len(index.Objects))
	for _, object := range index.Objects {
		if len(object.Blocks) != 1 {
			t.Fatalf("object %s blocks = %d, want 1", object.SHA256, len(object.Blocks))
		}
		encodings[object.SHA256] = object.Blocks[0].Encoding
	}
	if encodings[refs[0].SHA256] != EncodingRaw || encodings[refs[1].SHA256] != EncodingZstd {
		t.Fatalf("encodings = %#v", encodings)
	}

	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer resolver.Close()
	for index, want := range [][]byte{raw, compressible} {
		got := make([]byte, len(want))
		if n, err := resolver.ReadAt(context.Background(), refs[index], got, 0); n != len(want) || err != nil {
			t.Fatalf("ReadAt(%d) = %d, %v", index, n, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("ReadAt(%d) bytes differ", index)
		}
	}
}

func TestResolverOpensLegacyV1ZstdIndex(t *testing.T) {
	root := t.TempDir()
	value := bytes.Repeat([]byte("legacy-zstd-data"), 2000)
	refs := putObjects(t, root, value)
	writeManifest(t, root, "session", refs)
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	index := loadCurrentIndex(t, root)
	index.Version = legacyIndexVersion
	index.Kind = legacyIndexKind
	for objectIndex := range index.Objects {
		for blockIndex := range index.Objects[objectIndex].Blocks {
			block := &index.Objects[objectIndex].Blocks[blockIndex]
			if block.Encoding != EncodingZstd {
				t.Fatalf("legacy fixture block encoding = %q, want zstd", block.Encoding)
			}
			block.Encoding = ""
		}
	}
	encoded, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "packs", result.Generation, "index.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatalf("Open legacy v1: %v", err)
	}
	defer resolver.Close()
	got := make([]byte, len(value))
	if n, err := resolver.ReadAt(context.Background(), refs[0], got, 0); n != len(value) || err != nil || !bytes.Equal(got, value) {
		t.Fatalf("legacy ReadAt = %d, %v, equal=%t", n, err, bytes.Equal(got, value))
	}
}

func TestResolverSupportsOSCacheBypassOption(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("cache-bypass-object"))
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	resolver, err := Open(root, OpenOptions{BypassOSCache: true})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	buffer := make([]byte, refs[0].RawBytes)
	if _, err := resolver.ReadAt(context.Background(), refs[0], buffer, 0); err != nil {
		t.Fatal(err)
	}
	if string(buffer) != "cache-bypass-object" {
		t.Fatalf("unexpected bytes: %q", buffer)
	}
}

func TestResolverHoldsGenerationLeaseUntilClose(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("leased-generation"))
	writeManifest(t, root, "session", refs)
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaseDirectory := filepath.Join(root, "packs", result.Generation, "leases")
	active, err := storage.DirectoryHasActiveLease(leaseDirectory, false)
	if err != nil || !active {
		t.Fatalf("resolver generation lease: active=%t err=%v", active, err)
	}
	if err := resolver.Close(); err != nil {
		t.Fatal(err)
	}
	active, err = storage.DirectoryHasActiveLease(leaseDirectory, true)
	if err != nil || active {
		t.Fatalf("closed resolver generation lease: active=%t err=%v", active, err)
	}
}

func TestResolversShareGenerationResourcesUntilLastClose(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("shared-generation-resources"))
	writeManifest(t, root, "session", refs)
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(root, OpenOptions{})
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	if first.cache != second.cache || first.packs["pack-000001.pack"] != second.packs["pack-000001.pack"] {
		_ = first.Close()
		_ = second.Close()
		t.Fatal("resolvers for one generation did not share cache and pack descriptors")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	leaseDirectory := filepath.Join(root, "packs", result.Generation, "leases")
	active, err := storage.DirectoryHasActiveLease(leaseDirectory, false)
	if err != nil || !active {
		t.Fatalf("shared lease after first close: active=%t err=%v", active, err)
	}
	buffer := make([]byte, refs[0].RawBytes)
	if _, err := second.ReadAt(context.Background(), refs[0], buffer, 0); err != nil {
		t.Fatalf("second resolver after first close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	active, err = storage.DirectoryHasActiveLease(leaseDirectory, true)
	if err != nil || active {
		t.Fatalf("shared lease after final close: active=%t err=%v", active, err)
	}
}

func TestDoctorDoesNotPopulateActiveResolverCache(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, bytes.Repeat([]byte("doctor-cache-isolation"), 1000))
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	if resolver.cache.used != 0 {
		t.Fatalf("new resolver cache bytes = %d", resolver.cache.used)
	}
	report, err := Doctor(context.Background(), root)
	if err != nil || report.IssueCount != 0 || report.ManifestCount != 1 || report.VerifiedManifestCount != 1 {
		t.Fatalf("Doctor: report=%#v err=%v", report, err)
	}
	if resolver.cache.used != 0 {
		t.Fatalf("doctor populated active resolver cache with %d bytes", resolver.cache.used)
	}
}

func TestDoctorDetectsManifestOrderingCorruptionWithValidObjects(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("first-object"), []byte("second-object"))
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	manifest, err := fold.LoadManifest(root, "session")
	if err != nil {
		t.Fatal(err)
	}
	manifest.Parts[0], manifest.Parts[1] = manifest.Parts[1], manifest.Parts[0]
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fold.ManifestPath(root, "session"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Doctor(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if report.IssueCount == 0 || report.VerifiedManifestCount != 0 {
		t.Fatalf("manifest ordering corruption passed doctor: %#v", report)
	}
}

func TestOpenDoesNotRecreateGenerationMissingFromCurrent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "packs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "packs", "CURRENT"), []byte("gen-missing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, OpenOptions{}); err == nil {
		t.Fatal("resolver unexpectedly opened a missing generation")
	}
	if _, err := os.Lstat(filepath.Join(root, "packs", "gen-missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing pack generation was recreated: %v", err)
	}
}

func TestValidatePackedBlockBoundsUsesCachedPackSizes(t *testing.T) {
	objectDigest := strings.Repeat("a", 64)
	blockDigest := strings.Repeat("b", 64)
	index := Index{
		Objects: []Object{{
			SHA256: objectDigest,
			Blocks: []Block{
				{Pack: "pack-000001.bin", PackOffset: 0, StoredBytes: 10, SHA256: blockDigest},
				{Pack: "pack-000001.bin", PackOffset: 10, StoredBytes: 20, SHA256: blockDigest},
			},
		}},
	}
	if err := validatePackedBlockBounds(index, map[string]int64{"pack-000001.bin": 30}); err != nil {
		t.Fatalf("valid packed bounds: %v", err)
	}
	if err := validatePackedBlockBounds(index, map[string]int64{"pack-000001.bin": 29}); err == nil {
		t.Fatal("truncated pack bounds were accepted")
	}
	if err := validatePackedBlockBounds(index, map[string]int64{}); err == nil {
		t.Fatal("missing pack bounds were accepted")
	}
}

func TestBuildInterruptionKeepsPreviousGenerationCurrent(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("first-generation"))
	writeManifest(t, root, "session", refs)
	first, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatalf("first Build returned error: %v", err)
	}

	stop := errors.New("stop before publish")
	if _, err := Build(context.Background(), root, BuildOptions{BeforePublish: func() error { return stop }}); !errors.Is(err, stop) {
		t.Fatalf("interrupted Build error = %v, want %v", err, stop)
	}
	current, err := os.ReadFile(filepath.Join(root, "packs", "CURRENT"))
	if err != nil {
		t.Fatalf("read CURRENT: %v", err)
	}
	if string(bytes.TrimSpace(current)) != first.Generation {
		t.Fatalf("CURRENT = %q, want %q", bytes.TrimSpace(current), first.Generation)
	}

	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatalf("Open previous generation: %v", err)
	}
	defer resolver.Close()
	buffer := make([]byte, refs[0].RawBytes)
	if _, err := resolver.ReadAt(context.Background(), refs[0], buffer, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read previous generation: %v", err)
	}
}

func TestBuildBudgetRejectsBeforeCreatingCandidateGeneration(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("budgeted-pack-object"))
	writeManifest(t, root, "session", refs)
	checker := rejectingChecker{}
	if _, err := Build(context.Background(), root, BuildOptions{Budget: &checker}); !errors.Is(err, storage.ErrBudgetExceeded) {
		t.Fatalf("Build error = %v, want storage budget rejection", err)
	}
	if checker.Calls != 1 {
		t.Fatalf("budget checks = %d, want 1", checker.Calls)
	}
	if _, err := os.Stat(filepath.Join(root, "packs")); !os.IsNotExist(err) {
		t.Fatalf("candidate packs directory exists after preflight rejection: %v", err)
	}
}

func TestBuildReportsStorageAccounting(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("accounted-pack"))
	writeManifest(t, root, "session", refs)
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Storage == nil || result.Storage.Budget.ProjectedPeakBytes <= result.Storage.Budget.CurrentPhysicalBytes || result.Storage.After.Packs.ApparentBytes == 0 {
		t.Fatalf("pack storage accounting is incomplete: %#v", result.Storage)
	}
}

func TestResolverAndDoctorDetectPackCorruption(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, bytes.Repeat([]byte("protected"), 10000))
	writeManifest(t, root, "session", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	index := loadCurrentIndex(t, root)
	block := index.Objects[0].Blocks[0]
	packPath := filepath.Join(root, "packs", index.Generation, block.Pack)
	file, err := os.OpenFile(packPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open pack: %v", err)
	}
	if _, err := file.WriteAt([]byte{0xff}, block.PackOffset); err != nil {
		_ = file.Close()
		t.Fatalf("corrupt pack: %v", err)
	}
	_ = file.Close()

	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer resolver.Close()
	if _, err := resolver.ReadAt(context.Background(), refs[0], make([]byte, 16), 0); err == nil {
		t.Fatal("ReadAt should reject a corrupt pack block")
	}
	report, err := Doctor(context.Background(), root)
	if err != nil {
		t.Fatalf("Doctor returned error: %v", err)
	}
	if report.IssueCount == 0 {
		t.Fatalf("Doctor did not report corruption: %#v", report)
	}
}

func TestBuildIncludesObjectsReferencedByGenerationManifests(t *testing.T) {
	root := t.TempDir()
	data := []byte("generation-only-object")
	store := fold.NewObjectStore(root)
	ref, _, err := store.Put(data, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SyncPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	manifest := fold.Manifest{
		Version: fold.ManifestVersion, Kind: fold.ManifestKind,
		Session: fold.ManifestSession{ID: "session", RolloutPath: "session.jsonl", Archived: true},
		Source:  fold.ManifestSource{Bytes: int64(len(data)), SHA256: ref.SHA256},
		Parts:   []fold.Part{{Kind: fold.PartResidual, Object: ref}},
	}
	manifestPath := filepath.Join(root, "manifests", "generations", "session", "2.json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Build(context.Background(), root, BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.ObjectCount != 1 {
		t.Fatalf("object count = %d, want 1", result.ObjectCount)
	}
}

func putObjects(t *testing.T, root string, values ...[]byte) []fold.ObjectRef {
	t.Helper()
	store := fold.NewObjectStore(root)
	refs := make([]fold.ObjectRef, 0, len(values))
	for _, value := range values {
		ref, _, err := store.Put(value, true)
		if err != nil {
			t.Fatalf("Put returned error: %v", err)
		}
		refs = append(refs, ref)
	}
	if err := store.SyncPending(context.Background()); err != nil {
		t.Fatalf("SyncPending returned error: %v", err)
	}
	return refs
}

func writeManifest(t *testing.T, root string, sessionID string, refs []fold.ObjectRef) {
	t.Helper()
	manifest := fold.Manifest{
		Version: fold.ManifestVersion,
		Kind:    fold.ManifestKind,
		Session: fold.ManifestSession{ID: sessionID, RolloutPath: filepath.Join(root, sessionID+".jsonl")},
		Parts:   make([]fold.Part, 0, len(refs)),
	}
	hasher := sha256.New()
	store := fold.NewObjectStore(root)
	for _, ref := range refs {
		manifest.Source.Bytes += ref.RawBytes
		manifest.Parts = append(manifest.Parts, fold.Part{Kind: fold.PartResidual, Object: ref})
		value, err := store.Read(ref)
		if err != nil {
			t.Fatalf("read manifest object: %v", err)
		}
		_, _ = hasher.Write(value)
	}
	manifest.Source.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "manifests"), 0o755); err != nil {
		t.Fatalf("create manifests: %v", err)
	}
	if err := os.WriteFile(fold.ManifestPath(root, sessionID), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func loadCurrentIndex(t *testing.T, root string) Index {
	t.Helper()
	current, err := os.ReadFile(filepath.Join(root, "packs", "CURRENT"))
	if err != nil {
		t.Fatalf("read CURRENT: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "packs", string(bytes.TrimSpace(current)), "index.json"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var index Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("decode index: %v", err)
	}
	return index
}

type rejectingChecker struct {
	Calls int
}

func (c *rejectingChecker) Check(context.Context, storage.Projection) (storage.Assessment, error) {
	c.Calls++
	return storage.Assessment{}, storage.ErrBudgetExceeded
}

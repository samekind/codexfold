package pack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/jstar0/codexfold/internal/fold"
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
	for _, ref := range refs {
		manifest.Source.Bytes += ref.RawBytes
		manifest.Parts = append(manifest.Parts, fold.Part{Kind: fold.PartResidual, Object: ref})
	}
	manifest.Source.SHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
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

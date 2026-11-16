package pack

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samekind/codexfold/internal/fold"
)

func TestChoosePackRewrite(t *testing.T) {
	usage := map[string]*packUsage{
		"clean":     {size: 1000, live: 990},
		"half-dead": {size: 1000, live: 500},
		"all-dead":  {size: 1000, live: 0},
		"raw-heavy": {size: 1000, live: 1000, raw: 800},
	}
	plan := choosePackRewrite(usage, 1<<40, 100, 1000, 0)
	if !plan.rewrite["half-dead"] || !plan.rewrite["all-dead"] || !plan.rewrite["raw-heavy"] || plan.rewrite["clean"] || plan.full {
		t.Fatalf("ample space plan = %#v", plan)
	}
	plan = choosePackRewrite(usage, 600, 100, 1000, 0)
	if !plan.rewrite["all-dead"] || !plan.rewrite["half-dead"] || plan.rewrite["raw-heavy"] || plan.copyBytes != 500 {
		t.Fatalf("tight plan = %#v", plan)
	}
	plan = choosePackRewrite(usage, 0, 100, 1000, 0)
	if len(plan.rewrite) != 1 || !plan.rewrite["all-dead"] || plan.copyBytes != 0 {
		t.Fatalf("no-room plan = %#v", plan)
	}
	small := map[string]*packUsage{"a": {size: 10, live: 10}, "b": {size: 20, live: 20}, "c": {size: 1000, live: 1000}}
	plan = choosePackRewrite(small, 1<<40, 2, 1000, 0)
	if !plan.rewrite["a"] || !plan.rewrite["b"] || plan.rewrite["c"] {
		t.Fatalf("file-count plan = %#v", plan)
	}
}

func TestBuildRewritesOnlyTheFilesThatFit(t *testing.T) {
	root := t.TempDir()
	values := make([][]byte, 3)
	for index := range values {
		values[index] = make([]byte, 64<<10)
		if _, err := cryptorand.Read(values[index]); err != nil {
			t.Fatal(err)
		}
	}
	refs := putObjects(t, root, values...)
	for index, ref := range refs {
		writeManifest(t, root, []string{"keep-a", "drop", "keep-c"}[index], []fold.ObjectRef{ref})
	}
	// One object per pack file.
	if _, err := Build(context.Background(), root, BuildOptions{PackBytes: 64 << 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := RetireLoose(context.Background(), root, RetireLooseOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fold.ManifestPath(root, "drop")); err != nil {
		t.Fatal(err)
	}
	previous := packRewriteAllowance
	packRewriteAllowance = func(string, int64) (int64, error) { return 0, nil }
	t.Cleanup(func() { packRewriteAllowance = previous })
	if _, err := Build(context.Background(), root, BuildOptions{Incremental: true, PackBytes: 64 << 10}); err != nil {
		t.Fatal(err)
	}
	current, err := CurrentGeneration(root)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "packs", current))
	if err != nil {
		t.Fatal(err)
	}
	var reused, total int
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".pack") {
			total++
			if strings.HasPrefix(entry.Name(), "pack-reused-") {
				reused++
			}
		}
	}
	// The dead file is dropped for free; the two live files are linked.
	if reused != 2 || total != 2 {
		t.Fatalf("pack files reused=%d total=%d, want 2 linked and nothing copied", reused, total)
	}
	resolver, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	for _, index := range []int{0, 2} {
		got := make([]byte, len(values[index]))
		if n, err := resolver.ReadAt(context.Background(), refs[index], got, 0); n != len(got) || err != nil || !bytes.Equal(got, values[index]) {
			t.Fatalf("object %d after selective build: %d %v", index, n, err)
		}
	}
	if resolver.HasObject(refs[1]) {
		t.Fatal("dropped object is still indexed")
	}
}

func TestGenerationWaste(t *testing.T) {
	root := t.TempDir()
	value := make([]byte, 32<<10)
	if _, err := cryptorand.Read(value); err != nil {
		t.Fatal(err)
	}
	refs := putObjects(t, root, value)
	writeManifest(t, root, "only", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	files, wasted, err := GenerationWaste(root)
	if err != nil || files <= 0 || wasted != 0 {
		t.Fatalf("waste = %d/%d, %v", wasted, files, err)
	}
}

func TestBuildEstimatesPriceLooseObjectsByStoredSize(t *testing.T) {
	root := t.TempDir()
	compressible := bytes.Repeat([]byte(`{"type":"response_item","text":"the same assistant text again"}`+"\n"), 300000)
	refs := putObjects(t, root, compressible)
	writeManifest(t, root, "session", refs)
	references, count, rawBytes, err := buildReferenceIndex(context.Background(), root, filepath.Join(t.TempDir(), "refs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer references.Close()
	compacted, err := estimateCompactedGeneration(context.Background(), root, references, nil, count, rawBytes, defaultBlockBytes)
	if err != nil {
		t.Fatal(err)
	}
	metadataOnly, err := estimateGenerationMetadata(context.Background(), root, count, rawBytes, defaultBlockBytes, 0)
	if err != nil {
		t.Fatal(err)
	}
	if payload := compacted - metadataOnly; payload <= 0 || payload > int64(len(compressible))/10 {
		t.Fatalf("loose payload priced at %d bytes for %d raw bytes", payload, len(compressible))
	}
}

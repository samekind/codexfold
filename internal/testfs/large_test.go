package testfs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jstar0/codexfold/internal/codex"
	"github.com/jstar0/codexfold/internal/fold"
	"github.com/jstar0/codexfold/internal/fsctl"
	"github.com/jstar0/codexfold/internal/pack"
	"github.com/jstar0/codexfold/internal/vfs"
)

type largeGateReport struct {
	SourceBytes     int64                 `json:"source_bytes"`
	FoldDuration    time.Duration         `json:"fold_duration"`
	PackDuration    time.Duration         `json:"pack_duration"`
	ShadowDuration  time.Duration         `json:"shadow_duration"`
	First           fsctl.BenchmarkReport `json:"first"`
	Warm            fsctl.BenchmarkReport `json:"warm"`
	MaxRSSBytes     uint64                `json:"max_rss_bytes"`
	PackCacheBytes  int64                 `json:"pack_cache_bytes"`
	LooseObjectsOff bool                  `json:"loose_objects_offline"`
}

func TestLargePreviewBenchmark(t *testing.T) {
	if os.Getenv("CODEXFOLD_RUN_LARGE_TEST") != "1" {
		t.Skip("set CODEXFOLD_RUN_LARGE_TEST=1 to run the 758 MiB preview gate")
	}
	root := t.TempDir()
	const sourceBytes = int64(758 << 20)
	fixture, err := GenerateRollout(filepath.Join(root, "large.jsonl"), sourceBytes)
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(root, "store")
	foldStart := time.Now()
	if _, err := fold.Fold(context.Background(), codex.Session{ID: fixture.ID, RolloutPath: fixture.Path, Archived: true}, fold.FoldOptions{StoreDir: store, Apply: true, FieldThreshold: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	foldDuration := time.Since(foldStart)
	packStart := time.Now()
	if _, err := pack.Build(context.Background(), store, pack.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	packDuration := time.Since(packStart)
	const cacheBytes = int64(128 << 20)
	resolver, err := pack.Open(store, pack.OpenOptions{CacheBytes: cacheBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	manifest, err := fold.LoadManifest(store, fixture.ID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := vfs.NewView(manifest, resolver)
	if err != nil {
		t.Fatal(err)
	}
	objects := filepath.Join(store, "objects")
	offlineObjects := filepath.Join(store, "objects.offline")
	if err := os.Rename(objects, offlineObjects); err != nil {
		t.Fatal(err)
	}
	shadowStart := time.Now()
	shadow, err := fsctl.Shadow(context.Background(), fixture.Path, view, fsctl.ShadowOptions{BlockBytes: 4 << 20, RandomReads: 10000, Seed: 42})
	if err != nil || !shadow.Verified {
		t.Fatalf("shadow: %#v err=%v", shadow, err)
	}
	shadowDuration := time.Since(shadowStart)
	runtime.GC()
	options := fsctl.BenchmarkOptions{SequentialBlockBytes: 4 << 20, RandomBlockBytes: 4 << 10, RandomReads: 10000, Seed: 42}
	first, err := fsctl.Benchmark(context.Background(), fixture.Path, view, options)
	if err != nil {
		t.Fatal(err)
	}
	warm, err := fsctl.Benchmark(context.Background(), fixture.Path, view, options)
	if err != nil {
		t.Fatal(err)
	}
	report := largeGateReport{SourceBytes: sourceBytes, FoldDuration: foldDuration, PackDuration: packDuration, ShadowDuration: shadowDuration, First: first, Warm: warm, MaxRSSBytes: maxRSSBytes(), PackCacheBytes: cacheBytes, LooseObjectsOff: true}
	encoded, _ := json.MarshalIndent(report, "", "  ")
	t.Logf("large preview gate:\n%s", encoded)
	if first.Virtual.BytesPerSecond < 500<<20 || first.Virtual.BytesPerSecond < first.Native.BytesPerSecond*0.70 {
		t.Fatalf("first virtual throughput gate failed: native=%.0f virtual=%.0f", first.Native.BytesPerSecond, first.Virtual.BytesPerSecond)
	}
	if warm.Virtual.BytesPerSecond < 500<<20 || warm.Virtual.BytesPerSecond < warm.Native.BytesPerSecond*0.80 {
		t.Fatalf("warm virtual throughput gate failed: native=%.0f virtual=%.0f", warm.Native.BytesPerSecond, warm.Virtual.BytesPerSecond)
	}
	if report.MaxRSSBytes != 0 && report.MaxRSSBytes > 512<<20 {
		t.Fatalf("max RSS exceeded 512 MiB: %d", report.MaxRSSBytes)
	}
}

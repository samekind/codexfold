package pack

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/fold"
)

// An optional real rollout supplies bytes only: all storage, packing, deletion,
// and restored files live under t.TempDir(), never in the source store.
func TestRetireParallelRoundTrip(t *testing.T) {
	source := bytes.Repeat([]byte("{\"type\":\"event_msg\",\"payload\":{\"message\":\"reclamation verification\"}}\n"), 32768)
	if path := os.Getenv("CODEXFOLD_RETIRE_SAMPLE"); path != "" {
		var err error
		source, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(source) > 128<<20 {
			t.Fatal("sample exceeds 128 MiB bound")
		}
	}
	for _, workers := range []int{0, 1, 4} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			root := t.TempDir()
			sourcePath := filepath.Join(root, "sample.jsonl")
			if err := os.WriteFile(sourcePath, source, 0600); err != nil {
				t.Fatal(err)
			}
			folded, err := fold.Fold(context.Background(), fold.Session{ID: "sample", RolloutPath: sourcePath, Archived: true}, fold.FoldOptions{StoreDir: root, Apply: true})
			if err != nil || !folded.Verified {
				t.Fatalf("real fold failed: %v", err)
			}
			manifest, err := fold.LoadManifest(root, "sample")
			if err != nil {
				t.Fatal(err)
			}
			var refs []fold.ObjectRef
			for _, part := range manifest.Parts {
				refs = append(refs, part.Object)
			}
			background, _ := strconv.Atoi(os.Getenv("CODEXFOLD_RETIRE_BACKGROUND"))
			if background < 0 || background > 400 {
				t.Fatal("background fixture limit is 400")
			}
			small := []byte("{}\n")
			smallRefs := putObjects(t, root, small)
			for i := range background {
				writeManifest(t, root, fmt.Sprintf("background-%04d", i), smallRefs)
			}
			if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
				t.Fatal(err)
			}
			persistManagedManifestFixture(t, root, "sample", source)
			for i := range background {
				persistManagedManifestFixture(t, root, fmt.Sprintf("background-%04d", i), small)
			}
			start := time.Now()
			result, err := RetireLoose(context.Background(), root, RetireLooseOptions{Apply: true, Workers: workers})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("requested_workers=%d actual_workers=%d source_bytes=%d objects=%d elapsed=%s reclaimed=%d", workers, result.VerificationWorkers, len(source), result.RetiredCount, time.Since(start), result.ActualReclaimedBytes)
			if result.RetiredCount == 0 {
				t.Fatal("no real deletion occurred")
			}
			for _, ref := range refs {
				if _, err := os.Stat(fold.NewObjectStore(root).ObjectPath(ref.SHA256)); !os.IsNotExist(err) {
					t.Fatalf("loose object remains: %v", err)
				}
			}
			resolver, err := Open(root, OpenOptions{CacheBytes: -1})
			if err != nil {
				t.Fatal(err)
			}
			defer resolver.Close()
			target := filepath.Join(t.TempDir(), "restored.jsonl")
			if _, err := fold.UnfoldWithOptions(context.Background(), root, "sample", fold.UnfoldOptions{TargetPath: target, Reader: resolver}); err != nil {
				t.Fatal(err)
			}
			restored, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(restored, source) {
				t.Fatalf("pack-only reconstruction differs: %v", err)
			}
		})
	}
}

func TestRetireParallelCancellationPreservesCandidates(t *testing.T) {
	root := t.TempDir()
	refs := putObjects(t, root, []byte("first"), []byte("second"), []byte("third"))
	writeManifest(t, root, "sample", refs)
	if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := RetireLoose(ctx, root, RetireLooseOptions{Apply: true, Workers: 4, BeforeRemove: func(string) error { cancel(); return nil }})
	if err == nil {
		t.Fatal("cancellation reported success")
	}
	for _, ref := range refs {
		if _, err := os.Stat(fold.NewObjectStore(root).ObjectPath(ref.SHA256)); err != nil {
			t.Fatalf("removed after cancellation: %v", err)
		}
	}
}

func TestRetireParallelRejectsInvalidWorkerLimit(t *testing.T) {
	for _, workers := range []int{-1, 17} {
		if _, err := RetireLoose(context.Background(), t.TempDir(), RetireLooseOptions{Workers: workers}); err == nil {
			t.Fatalf("accepted workers=%d", workers)
		}
	}
}

func TestRetirementAutomaticWorkerCount(t *testing.T) {
	for _, size := range []int{64 << 10, 1 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			root := t.TempDir()
			refs := putObjects(t, root, bytes.Repeat([]byte("x"), size))
			writeManifest(t, root, "sample", refs)
			if _, err := Build(context.Background(), root, BuildOptions{}); err != nil {
				t.Fatal(err)
			}
			resolver, err := Open(root, OpenOptions{CacheBytes: -1})
			if err != nil {
				t.Fatal(err)
			}
			defer resolver.Close()
			want := 1
			if size >= 256<<10 {
				want = min(4, runtime.GOMAXPROCS(0))
			}
			got, err := retirementWorkerCount(resolver, 0)
			if err != nil || got != want {
				t.Fatalf("auto workers=%d want=%d err=%v", got, want, err)
			}
			if got, err := retirementWorkerCount(resolver, 3); err != nil || got != 3 {
				t.Fatalf("manual setting ignored: %d %v", got, err)
			}
		})
	}
}

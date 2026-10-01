package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
)

func TestNativeRetirementRealBatchComparison(t *testing.T) {
	sample, candidate, baseline := os.Getenv("CODEXFOLD_RETIRE_SAMPLE"), os.Getenv("CODEXFOLD_CANDIDATE_BIN"), os.Getenv("CODEXFOLD_BASELINE_BIN")
	if sample == "" || candidate == "" || baseline == "" {
		t.Skip("opt-in real batch comparison")
	}
	data, err := os.ReadFile(sample)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 16<<20 {
		t.Fatal("benchmark sample limit 16 MiB")
	}
	corpus := make([][]byte, 10)
	for i := range corpus {
		corpus[i] = data
	}
	if directory := os.Getenv("CODEXFOLD_RETIRE_SAMPLE_DIR"); directory != "" {
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() < 1<<20 || info.Size() > 16<<20 {
				continue
			}
			value, err := os.ReadFile(filepath.Join(directory, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			corpus[n] = value
			n++
			if n == len(corpus) {
				break
			}
		}
		if n != len(corpus) {
			t.Fatal("not enough bounded real samples")
		}
	}
	logicalBytes := 0
	for _, value := range corpus {
		logicalBytes += len(value)
	}
	for _, workers := range []int{-1, 1, 4, 8} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			home := t.TempDir()
			store := filepath.Join(home, "fold-store")
			ids := make([]string, 10)
			for i := range ids {
				id := fmt.Sprintf("sample-%02d", i)
				ids[i] = id
				source := filepath.Join(home, id+".jsonl")
				if err := os.WriteFile(source, corpus[i], 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := fold.Fold(context.Background(), fold.Session{ID: id, RolloutPath: source, Archived: true}, fold.FoldOptions{StoreDir: store, Apply: true}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pack.Build(context.Background(), store, pack.BuildOptions{}); err != nil {
				t.Fatal(err)
			}
			resolver, err := pack.Open(store, pack.OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range ids {
				manifest, err := fold.LoadManifest(store, id)
				if err != nil {
					t.Fatal(err)
				}
				snapshot := filepath.Join(store, "fs", "snapshots", id, "native.jsonl")
				if err := os.MkdirAll(filepath.Dir(snapshot), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(manifest.Session.RolloutPath, snapshot); err != nil {
					t.Fatal(err)
				}
				if _, err := vfs.OpenSession(context.Background(), vfs.SessionOptions{Root: store, ManifestPath: fold.ManifestPath(store, id), Manifest: manifest, Reader: resolver, NativeSnapshot: vfs.NativeFile{Path: snapshot, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256}}); err != nil {
					t.Fatal(err)
				}
			}
			resolver.Close()
			run := func(binary string, args ...string) []byte {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
				if err != nil {
					t.Fatalf("command failed: %v %s", err, output)
				}
				return output
			}
			start := time.Now()
			if workers < 0 {
				for _, id := range ids {
					run(baseline, "fs", "retire-native", id, "--codex-home", home, "--store", store, "--apply", "--json")
				}
				run(baseline, "pack", "retire-loose", "--codex-home", home, "--store", store, "--apply", "--json")
			} else {
				output := run(candidate, "fs", "enroll", "reclaim", "--codex-home", home, "--store", store, "--apply", "--workers", fmt.Sprint(workers), "--json")
				var result FSEnrollmentMaintenanceResult
				if err := json.Unmarshal(output, &result); err != nil {
					t.Fatal(err)
				}
				if result.NativeRetired != len(ids) || result.NativeProofPasses != 1 {
					t.Fatalf("batch proof not amortized: %+v", result)
				}
			}
			elapsed := time.Since(start)
			inventory, err := storage.Scan(context.Background(), storage.Options{StoreDir: store})
			if err != nil {
				t.Fatal(err)
			}
			if inventory.RetainedSnapshots.Files != 0 || inventory.UniqueLooseObjects.Files != 0 {
				t.Fatalf("cleanup left duplicate bytes: %+v", inventory)
			}
			for i, id := range ids {
				target := filepath.Join(home, "restored.jsonl")
				run(candidate, "unfold", id, "--codex-home", home, "--store", store, "--to", target, "--overwrite", "--json")
				restored, err := os.ReadFile(target)
				if err != nil || !bytes.Equal(restored, corpus[i]) {
					t.Fatalf("round trip differs: %v", err)
				}
			}
			t.Logf("workers=%d sessions=%d logical_bytes=%d elapsed=%s remaining_store_bytes=%d", workers, len(ids), logicalBytes, elapsed, inventory.TotalPhysicalBytes)
		})
	}
}

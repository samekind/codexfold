package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/fold"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/samekind/codexfold/internal/vfs"
	_ "modernc.org/sqlite"
)

// Opt-in: execute the installed artifact, never production storage. Session
// registration uses the VFS API; this does not stand in for FSKit/Desktop QA.
func TestInstalledFoldAndCompleteCleanup(t *testing.T) {
	binary, sample := os.Getenv("CODEXFOLD_INSTALLED_TEST_BIN"), os.Getenv("CODEXFOLD_RETIRE_SAMPLE")
	if binary == "" || sample == "" {
		t.Skip("requires an installed binary and read-only real sample")
	}
	source, err := os.ReadFile(sample)
	if err != nil {
		t.Fatal(err)
	}
	if len(source) > 128<<20 {
		t.Fatal("sample exceeds bound")
	}
	home := t.TempDir()
	store := filepath.Join(home, "fold-store")
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`create table threads (id text primary key, title text, cwd text, rollout_path text, model_provider text, model text, updated_at integer, archived integer, git_branch text)`)
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		start := time.Now()
		command := exec.CommandContext(ctx, binary, args...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v: %s", args[0], args[1], err, output)
		}
		t.Logf("stage=%s/%s elapsed=%s", args[0], args[1], time.Since(start))
		return output
	}
	oldRunner := runEnrollmentCommand
	t.Cleanup(func() { runEnrollmentCommand = oldRunner })
	runEnrollmentCommand = func(ctx context.Context, args []string) error {
		if os.Getenv("CODEXFOLD_BATCH_TEST") == "1" && len(args) > 2 && args[0] == "fs" && args[1] == "retire-native" {
			return runInProcessNativeRetirement(ctx, args)
		}
		run(args...)
		return nil
	}
	var previousGeneration string
	for _, id := range []string{"sample-one", "sample-two"} {
		path := filepath.Join(home, id+".jsonl")
		if err := os.WriteFile(path, source, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`insert into threads values (?, 'test', '', ?, 'test', '', 1, 1, '')`, id, path); err != nil {
			t.Fatal(err)
		}
		foldedJSON := run("fold", id, "--codex-home", home, "--store", store, "--apply", "--json")
		var folded fold.FoldResult
		if err := json.Unmarshal(foldedJSON, &folded); err != nil {
			t.Fatal(err)
		}
		if !folded.Verified {
			t.Fatal("fold did not verify")
		}
		t.Logf("session=%s source=%d new_stored=%d reused=%d unique=%d", id, len(source), folded.NewStoredBytes, folded.ReusedObjects, folded.UniqueObjects)
		if id == "sample-two" && folded.NewStoredBytes != 0 {
			t.Fatal("second identical session recreated packed objects")
		}
		run("pack", "build", "--codex-home", home, "--store", store, "--json")
		manifest, err := fold.LoadManifest(store, id)
		if err != nil {
			t.Fatal(err)
		}
		snapshot := filepath.Join(store, "fs", "snapshots", id, "native.jsonl")
		if err := os.MkdirAll(filepath.Dir(snapshot), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path, snapshot); err != nil {
			t.Fatal(err)
		}
		resolver, err := pack.Open(store, pack.OpenOptions{CacheBytes: -1})
		if err != nil {
			t.Fatal(err)
		}
		_, err = vfs.OpenSession(context.Background(), vfs.SessionOptions{Root: store, ManifestPath: fold.ManifestPath(store, id), Manifest: manifest, Reader: resolver, NativeSnapshot: vfs.NativeFile{Path: snapshot, Bytes: manifest.Source.Bytes, SHA256: manifest.Source.SHA256}})
		resolver.Close()
		if err != nil {
			t.Fatal(err)
		}
		before, err := storage.Scan(context.Background(), storage.Options{StoreDir: store})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		result, err := runEnrollmentMaintenance(context.Background(), home, store, 1, true)
		if err != nil {
			t.Fatal(err)
		}
		after, err := storage.Scan(context.Background(), storage.Options{StoreDir: store})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("cleanup session=%s elapsed=%s native_retired=%d gc_removed=%d before=%d after=%d loose=%d snapshots=%d old_generations=%d", id, time.Since(start), result.NativeRetired, result.StorageGC.RemovedCount, before.TotalPhysicalBytes, after.TotalPhysicalBytes, after.UniqueLooseObjects.Files, after.RetainedSnapshots.Files, after.OldGenerations.Files)
		if after.UniqueLooseObjects.Files != 0 || after.RetainedSnapshots.Files != 0 || after.OldGenerations.Files != 0 {
			t.Fatal("cleanup left duplicate storage")
		}
		if after.TotalPhysicalBytes >= before.TotalPhysicalBytes {
			t.Fatal("cleanup did not release physical storage")
		}
		if _, err := os.Stat(snapshot); !os.IsNotExist(err) {
			t.Fatal("native snapshot remains")
		}
		if previousGeneration != "" {
			if _, err := os.Stat(filepath.Join(store, "packs", previousGeneration)); !os.IsNotExist(err) {
				t.Fatal("previous pack generation remains")
			}
		}
		previousGeneration, err = pack.CurrentGeneration(store)
		if err != nil {
			t.Fatal(err)
		}
		for _, restoreID := range []string{"sample-one", id} {
			target := filepath.Join(home, "restored-"+restoreID+".jsonl")
			run("unfold", restoreID, "--codex-home", home, "--store", store, "--to", target, "--overwrite", "--json")
			data, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(data, source) {
				t.Fatalf("restored bytes differ: %v", err)
			}
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
		}
		run("pack", "doctor", "--codex-home", home, "--store", store, "--json")
		run("doctor", "--codex-home", home, "--store", store, "--json")
		if baseline := os.Getenv("CODEXFOLD_BASELINE_BIN"); baseline != "" {
			candidate := binary
			binary = baseline
			target := filepath.Join(home, "old-binary-restored.jsonl")
			run("unfold", id, "--codex-home", home, "--store", store, "--to", target, "--overwrite", "--json")
			data, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(data, source) {
				t.Fatalf("rollback binary cannot read incremental storage: %v", err)
			}
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			binary = candidate
		}
		repeat, err := runEnrollmentMaintenance(context.Background(), home, store, 0, true)
		if err != nil {
			t.Fatal(err)
		}
		if repeat.NativeRetired != 0 || repeat.LooseRetirementRan || repeat.StorageGC.RemovedCount != 0 {
			t.Fatalf("idempotent cleanup did additional work: %+v", repeat)
		}
	}
}

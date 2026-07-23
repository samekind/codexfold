package fold

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samekind/codexfold/internal/cdc"
	"github.com/samekind/codexfold/internal/storage"
)

func TestFoldRejectsSourceMutationBeforeManifestCommit(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "rollout.jsonl")
	storeDir := filepath.Join(root, "store")
	if err := os.WriteFile(sourcePath, []byte("{\"value\":\"large-field-value\"}\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	_, err := Fold(context.Background(), Session{
		ID: "changing", RolloutPath: sourcePath, Archived: true,
	}, FoldOptions{
		StoreDir: storeDir, Apply: true, FieldThreshold: 4,
		beforeCommit: func() error {
			file, openErr := os.OpenFile(sourcePath, os.O_APPEND|os.O_WRONLY, 0)
			if openErr != nil {
				return openErr
			}
			if _, writeErr := file.WriteString("{\"appended\":true}\n"); writeErr != nil {
				_ = file.Close()
				return writeErr
			}
			return file.Close()
		},
	})
	if err == nil || !strings.Contains(err.Error(), "changed during fold") {
		t.Fatalf("Fold error = %v, want changed-during-fold rejection", err)
	}
	if _, statErr := os.Stat(ManifestPath(storeDir, "changing")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("manifest was committed after source mutation: %v", statErr)
	}
	if _, statErr := os.Stat(sourcePath); statErr != nil {
		t.Fatalf("source was removed after mutation: %v", statErr)
	}
	gc, gcErr := GC(context.Background(), storeDir, false)
	if gcErr != nil {
		t.Fatalf("GC dry-run returned error: %v", gcErr)
	}
	if gc.OrphanCount == 0 {
		t.Fatalf("expected pre-commit objects to be reported as orphans: %#v", gc)
	}
}

func TestFoldBudgetRejectsBeforeWritingObjectsOrManifest(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "rollout.jsonl")
	storeDir := filepath.Join(root, "store")
	if err := os.WriteFile(sourcePath, []byte("{\"value\":\"budgeted-field\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checker := &foldRejectingChecker{}
	_, err := Fold(context.Background(), Session{ID: "budget", RolloutPath: sourcePath, Archived: true}, FoldOptions{
		StoreDir: storeDir, Apply: true, FieldThreshold: 4, Budget: checker,
	})
	if !errors.Is(err, storage.ErrBudgetExceeded) {
		t.Fatalf("Fold error = %v, want storage budget rejection", err)
	}
	if checker.Calls != 1 || checker.Projection.Operation != "fold" || checker.Projection.AdditionalPersistentBytes <= 0 {
		t.Fatalf("unexpected fold budget projection: %#v", checker)
	}
	if _, err := os.Stat(storeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fold store exists after preflight rejection: %v", err)
	}
}

func TestFoldReportsProjectedAndActualPhysicalAccounting(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.jsonl")
	storeDir := filepath.Join(root, "store")
	source := []byte("{\"value\":\"physical-accounting\"}\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Fold(context.Background(), Session{ID: "accounting", RolloutPath: sourcePath, Archived: true}, FoldOptions{StoreDir: storeDir, Apply: true, FieldThreshold: 4})
	if err != nil {
		t.Fatal(err)
	}
	if result.Storage == nil || result.Storage.Budget.ProjectedPeakBytes <= 0 || result.Storage.After.LogicalSessionBytes != int64(len(source)) {
		t.Fatalf("fold storage accounting is incomplete: %#v", result.Storage)
	}
	if result.Storage.ActualReclaimedBytes != 0 {
		t.Fatalf("fold with retained source claimed reclamation: %#v", result.Storage)
	}
}

func TestUnfoldBudgetRejectsBeforeCreatingRestoreTarget(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.jsonl")
	storeDir := filepath.Join(root, "store")
	if err := os.WriteFile(sourcePath, []byte("{\"value\":\"restore-budget\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Fold(context.Background(), Session{ID: "restore", RolloutPath: sourcePath, Archived: true}, FoldOptions{StoreDir: storeDir, Apply: true, FieldThreshold: 4}); err != nil {
		t.Fatal(err)
	}
	checker := &foldRejectingChecker{}
	target := filepath.Join(root, "output", "restored.jsonl")
	if _, err := UnfoldWithOptions(context.Background(), storeDir, "restore", UnfoldOptions{TargetPath: target, Budget: checker}); !errors.Is(err, storage.ErrBudgetExceeded) {
		t.Fatalf("UnfoldWithOptions error = %v, want storage budget rejection", err)
	}
	if checker.Calls != 1 || checker.Projection.Operation != "unfold" {
		t.Fatalf("unexpected unfold budget projection: %#v", checker)
	}
	if _, err := os.Stat(filepath.Dir(target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore target directory exists after preflight rejection: %v", err)
	}
}

func TestUnfoldReportsStorageBudgetWithoutClaimingReclamation(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.jsonl")
	storeDir := filepath.Join(root, "store")
	if err := os.WriteFile(sourcePath, []byte("{\"value\":\"unfold-accounting\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Fold(context.Background(), Session{ID: "unfold-accounting", RolloutPath: sourcePath, Archived: true}, FoldOptions{StoreDir: storeDir, Apply: true, FieldThreshold: 4}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "restored.jsonl")
	result, err := Unfold(context.Background(), storeDir, "unfold-accounting", target, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Storage == nil || result.Storage.Budget.ProjectedPeakBytes <= result.Storage.Budget.CurrentPhysicalBytes || result.Storage.ActualReclaimedBytes != 0 {
		t.Fatalf("unfold storage accounting is incomplete: %#v", result.Storage)
	}
}

func TestFoldRejectsSameSizeMutationEvenWhenMtimeIsRestored(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "rollout.jsonl")
	storeDir := filepath.Join(root, "store")
	source := []byte("{\"value\":\"large-field-value-A\"}\n")
	mutated := []byte("{\"value\":\"large-field-value-B\"}\n")
	if len(source) != len(mutated) {
		t.Fatal("test fixture mutation must preserve file size")
	}
	if err := os.WriteFile(sourcePath, source, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	before, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatalf("stat source: %v", err)
	}

	_, err = Fold(context.Background(), Session{
		ID: "same-size-change", RolloutPath: sourcePath, Archived: true,
	}, FoldOptions{
		StoreDir: storeDir, Apply: true, FieldThreshold: 4,
		beforeCommit: func() error {
			if writeErr := os.WriteFile(sourcePath, mutated, 0o644); writeErr != nil {
				return writeErr
			}
			return os.Chtimes(sourcePath, before.ModTime(), before.ModTime())
		},
	})
	if err == nil || !strings.Contains(err.Error(), "changed during fold") {
		t.Fatalf("Fold error = %v, want digest-based mutation rejection", err)
	}
	if _, statErr := os.Stat(ManifestPath(storeDir, "same-size-change")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("manifest was committed after same-size mutation: %v", statErr)
	}
}

func TestFoldCreatesVerifiedManifestAndReusesRepeatedField(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "rollout.jsonl")
	storeDir := filepath.Join(root, "store")
	field := "same-large-field-value"
	source := []byte("{\"payload\":{\"output\":\"" + field + "\"},\"n\":1}\n" +
		"{\"n\":2,\"payload\":{\"output\":\"" + field + "\"}}\n")
	if err := os.WriteFile(sourcePath, source, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	result, err := Fold(context.Background(), Session{
		ID: "fixture", Title: "Fixture", CWD: "/workspace", RolloutPath: sourcePath, Archived: true,
	}, FoldOptions{
		StoreDir:         storeDir,
		Apply:            true,
		FieldThreshold:   8,
		MaxJSONLineBytes: 1 << 20,
		CDC:              cdc.Options{MinBytes: 8, AverageBytes: 16, MaxBytes: 32},
	})
	if err != nil {
		t.Fatalf("Fold returned error: %v", err)
	}
	if !result.Verified || result.FieldParts != 2 || result.ReusedObjects == 0 {
		t.Fatalf("unexpected fold result: %#v", result)
	}
	manifest, err := LoadManifest(storeDir, "fixture")
	if err != nil {
		t.Fatalf("LoadManifest returned error: %v", err)
	}
	if manifest.Source.SHA256 != result.SourceSHA256 || len(manifest.Parts) == 0 {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}
	restoredPath := filepath.Join(root, "restored.jsonl")
	restore, err := Unfold(context.Background(), storeDir, "fixture", restoredPath, false)
	if err != nil {
		t.Fatalf("Unfold returned error: %v", err)
	}
	if !restore.Verified {
		t.Fatalf("restore not verified: %#v", restore)
	}
	restored, err := os.ReadFile(restoredPath)
	if err != nil {
		t.Fatalf("read restored: %v", err)
	}
	if string(restored) != string(source) {
		t.Fatalf("restored source differs\nwant: %q\ngot:  %q", source, restored)
	}
}

func TestFoldDryRunDoesNotCreateStore(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "rollout.jsonl")
	storeDir := filepath.Join(root, "store")
	if err := os.WriteFile(sourcePath, []byte("{\"value\":\"large-value\"}\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	result, err := Fold(context.Background(), Session{ID: "dry", RolloutPath: sourcePath, Archived: true}, FoldOptions{
		StoreDir: storeDir, FieldThreshold: 4,
	})
	if err != nil {
		t.Fatalf("Fold returned error: %v", err)
	}
	if !result.DryRun || !result.Verified {
		t.Fatalf("unexpected dry-run result: %#v", result)
	}
	if _, err := os.Stat(storeDir); !os.IsNotExist(err) {
		t.Fatalf("dry-run created store: %v", err)
	}
}

func TestFoldWritesToExplicitGenerationManifestWithoutReplacingPrimary(t *testing.T) {
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	sourcePath := filepath.Join(root, "session.jsonl")
	data := []byte("{\"value\":\"generation-manifest\"}\n")
	if err := os.WriteFile(sourcePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	primary := ManifestPath(storeDir, "session")
	if err := os.MkdirAll(filepath.Dir(primary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(primary, []byte("primary-sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	generationPath := filepath.Join(storeDir, "manifests", "generations", "session", "2.json")
	result, err := Fold(context.Background(), Session{ID: "session", RolloutPath: sourcePath, Archived: true}, FoldOptions{StoreDir: storeDir, Apply: true, FieldThreshold: 8, ManifestPathOverride: generationPath})
	if err != nil {
		t.Fatalf("Fold generation manifest: %v", err)
	}
	if result.ManifestPath != generationPath {
		t.Fatalf("manifest path = %q, want %q", result.ManifestPath, generationPath)
	}
	if primaryData, err := os.ReadFile(primary); err != nil || string(primaryData) != "primary-sentinel" {
		t.Fatalf("primary manifest changed: %q err=%v", primaryData, err)
	}
	manifest, err := LoadManifestPath(generationPath)
	if err != nil || manifest.Source.Bytes != int64(len(data)) {
		t.Fatalf("load generation manifest: %#v err=%v", manifest, err)
	}
}

func TestFoldRoundTripsEmptyInvalidAndOversizedRollouts(t *testing.T) {
	for _, test := range []struct {
		name          string
		source        []byte
		maxLineBytes  int64
		wantInvalid   int64
		wantOversized int64
	}{
		{name: "empty", source: []byte{}},
		{name: "invalid-json", source: []byte("not-json\n"), wantInvalid: 1},
		{name: "oversized-json", source: []byte("{\"value\":\"" + strings.Repeat("x", 4096) + "\"}\n"), maxLineBytes: 64, wantOversized: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			sourcePath := filepath.Join(root, "source.jsonl")
			storeDir := filepath.Join(root, "store")
			restoredPath := filepath.Join(root, "restored.jsonl")
			if err := os.WriteFile(sourcePath, test.source, 0o644); err != nil {
				t.Fatalf("write source: %v", err)
			}
			result, err := Fold(context.Background(), Session{ID: test.name, RolloutPath: sourcePath, Archived: true}, FoldOptions{
				StoreDir: storeDir, Apply: true, FieldThreshold: 4, MaxJSONLineBytes: test.maxLineBytes,
			})
			if err != nil {
				t.Fatalf("Fold returned error: %v", err)
			}
			if result.InvalidJSONLines != test.wantInvalid || result.OversizedLines != test.wantOversized {
				t.Fatalf("unexpected fallback counts: %#v", result)
			}
			if _, err := Unfold(context.Background(), storeDir, test.name, restoredPath, false); err != nil {
				t.Fatalf("Unfold returned error: %v", err)
			}
			got, err := os.ReadFile(restoredPath)
			if err != nil || !bytes.Equal(got, test.source) {
				t.Fatalf("round trip differs: bytes=%d err=%v", len(got), err)
			}
		})
	}
}

func TestFoldHonorsCanceledContextWithoutManifest(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.jsonl")
	storeDir := filepath.Join(root, "store")
	if err := os.WriteFile(sourcePath, []byte("{\"value\":1}\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Fold(ctx, Session{ID: "canceled", RolloutPath: sourcePath, Archived: true}, FoldOptions{StoreDir: storeDir, Apply: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Fold error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(ManifestPath(storeDir, "canceled")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("manifest committed after cancellation: %v", statErr)
	}
}

type foldRejectingChecker struct {
	Calls      int
	Projection storage.Projection
}

func (c *foldRejectingChecker) Check(_ context.Context, projection storage.Projection) (storage.Assessment, error) {
	c.Calls++
	c.Projection = projection
	return storage.Assessment{}, storage.ErrBudgetExceeded
}

package fold

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jstar0/codexfold/internal/cdc"
	"github.com/jstar0/codexfold/internal/codex"
)

func TestFoldRejectsSourceMutationBeforeManifestCommit(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "rollout.jsonl")
	storeDir := filepath.Join(root, "store")
	if err := os.WriteFile(sourcePath, []byte("{\"value\":\"large-field-value\"}\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	_, err := Fold(context.Background(), codex.Session{
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

	_, err = Fold(context.Background(), codex.Session{
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

	result, err := Fold(context.Background(), codex.Session{
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
	result, err := Fold(context.Background(), codex.Session{ID: "dry", RolloutPath: sourcePath, Archived: true}, FoldOptions{
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
			result, err := Fold(context.Background(), codex.Session{ID: test.name, RolloutPath: sourcePath, Archived: true}, FoldOptions{
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
	_, err := Fold(ctx, codex.Session{ID: "canceled", RolloutPath: sourcePath, Archived: true}, FoldOptions{StoreDir: storeDir, Apply: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Fold error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(ManifestPath(storeDir, "canceled")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("manifest committed after cancellation: %v", statErr)
	}
}

package scan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jstar0/codexfold/internal/codex"
)

func TestIncrementalEvaluateSkipsUnchangedFileWithoutDoubleCounting(t *testing.T) {
	root := t.TempDir()
	rolloutPath := filepath.Join(root, "rollout.jsonl")
	indexPath := filepath.Join(root, "incremental.sqlite")
	source := []byte("{\"value\":\"same-large-value\"}\n{\"value\":\"same-large-value\"}\n")
	if err := os.WriteFile(rolloutPath, source, 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	sessions := []codex.Session{{ID: "fixture", RolloutPath: rolloutPath}}
	options := Options{
		All: true, IndexPath: indexPath, Incremental: true,
		Layers: []string{LayerRecord, LayerField}, MinFieldBytes: 8,
	}

	first, err := Evaluate(context.Background(), sessions, options)
	if err != nil {
		t.Fatalf("first Evaluate returned error: %v", err)
	}
	second, err := Evaluate(context.Background(), sessions, options)
	if err != nil {
		t.Fatalf("second Evaluate returned error: %v", err)
	}
	if first.Scan != second.Scan {
		t.Fatalf("corpus stats changed after unchanged incremental scan:\nfirst=%#v\nsecond=%#v", first.Scan, second.Scan)
	}
	if second.ProcessedBytes != 0 || second.SkippedSessionCount != 1 {
		t.Fatalf("unchanged file was not skipped: %#v", second)
	}
	if got := findLayerStats(second.Layers, LayerField); got.ObjectCount != 2 || got.DuplicateOccurrences != 1 {
		t.Fatalf("field observations were double counted: %#v", got)
	}
}

func TestIncrementalEvaluateScansAppendTailAndMatchesFullScan(t *testing.T) {
	root := t.TempDir()
	rolloutPath := filepath.Join(root, "rollout.jsonl")
	indexPath := filepath.Join(root, "incremental.sqlite")
	firstRecord := []byte("{\"value\":\"same-large-value\"}\n")
	appendedRecord := []byte("{\"value\":\"same-large-value\"}\n")
	if err := os.WriteFile(rolloutPath, firstRecord, 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	sessions := []codex.Session{{ID: "fixture", RolloutPath: rolloutPath}}
	options := Options{
		All: true, IndexPath: indexPath, Incremental: true,
		Layers: []string{LayerRecord, LayerField}, MinFieldBytes: 8,
	}
	if _, err := Evaluate(context.Background(), sessions, options); err != nil {
		t.Fatalf("initial Evaluate returned error: %v", err)
	}
	file, err := os.OpenFile(rolloutPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open rollout for append: %v", err)
	}
	if _, err := file.Write(appendedRecord); err != nil {
		_ = file.Close()
		t.Fatalf("append rollout: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close appended rollout: %v", err)
	}

	incremental, err := Evaluate(context.Background(), sessions, options)
	if err != nil {
		t.Fatalf("incremental Evaluate returned error: %v", err)
	}
	full, err := Evaluate(context.Background(), sessions, Options{
		All: true, Layers: options.Layers, MinFieldBytes: options.MinFieldBytes,
	})
	if err != nil {
		t.Fatalf("full Evaluate returned error: %v", err)
	}
	if incremental.ProcessedBytes != int64(len(appendedRecord)) || incremental.AppendedSessionCount != 1 {
		t.Fatalf("append tail was not isolated: %#v", incremental)
	}
	if incremental.Scan != full.Scan {
		t.Fatalf("incremental corpus stats differ from full scan:\nincremental=%#v\nfull=%#v", incremental.Scan, full.Scan)
	}
	for _, layer := range options.Layers {
		if got, want := findLayerStats(incremental.Layers, layer), findLayerStats(full.Layers, layer); got != want {
			t.Fatalf("%s stats differ:\nincremental=%#v\nfull=%#v", layer, got, want)
		}
	}
}

func TestIncrementalEvaluateRejectsRewriteAndTruncate(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(path string) error
	}{
		{name: "rewrite", mutate: func(path string) error {
			if err := os.WriteFile(path, []byte("{\"value\":\"different-value!\"}\n"), 0o644); err != nil {
				return err
			}
			future := time.Now().Add(time.Second)
			return os.Chtimes(path, future, future)
		}},
		{name: "truncate", mutate: func(path string) error { return os.Truncate(path, 8) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			rolloutPath := filepath.Join(root, "rollout.jsonl")
			indexPath := filepath.Join(root, "incremental.sqlite")
			if err := os.WriteFile(rolloutPath, []byte("{\"value\":\"original-value!!\"}\n"), 0o644); err != nil {
				t.Fatalf("write rollout: %v", err)
			}
			sessions := []codex.Session{{ID: "fixture", RolloutPath: rolloutPath}}
			options := Options{All: true, IndexPath: indexPath, Incremental: true, Layers: []string{LayerField}, MinFieldBytes: 4}
			if _, err := Evaluate(context.Background(), sessions, options); err != nil {
				t.Fatalf("initial Evaluate returned error: %v", err)
			}
			if err := test.mutate(rolloutPath); err != nil {
				t.Fatalf("mutate rollout: %v", err)
			}
			if _, err := Evaluate(context.Background(), sessions, options); !errors.Is(err, ErrIndexRebuildRequired) {
				t.Fatalf("Evaluate error = %v, want ErrIndexRebuildRequired", err)
			}
		})
	}
}

func TestIncrementalEvaluateRejectsConfigurationChange(t *testing.T) {
	root := t.TempDir()
	rolloutPath := filepath.Join(root, "rollout.jsonl")
	indexPath := filepath.Join(root, "incremental.sqlite")
	if err := os.WriteFile(rolloutPath, []byte("{\"value\":\"original-value\"}\n"), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	sessions := []codex.Session{{ID: "fixture", RolloutPath: rolloutPath}}
	if _, err := Evaluate(context.Background(), sessions, Options{
		All: true, IndexPath: indexPath, Incremental: true, Layers: []string{LayerField}, MinFieldBytes: 4,
	}); err != nil {
		t.Fatalf("initial Evaluate returned error: %v", err)
	}
	_, err := Evaluate(context.Background(), sessions, Options{
		All: true, IndexPath: indexPath, Incremental: true, Layers: []string{LayerField}, MinFieldBytes: 8,
	})
	if !errors.Is(err, ErrIndexRebuildRequired) {
		t.Fatalf("Evaluate error = %v, want ErrIndexRebuildRequired", err)
	}
}

func TestIncrementalEvaluateRejectsConfigurationChangeFromInitialPersistentScan(t *testing.T) {
	root := t.TempDir()
	rolloutPath := filepath.Join(root, "rollout.jsonl")
	indexPath := filepath.Join(root, "persistent.sqlite")
	if err := os.WriteFile(rolloutPath, []byte("{\"value\":\"original-value\"}\n"), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	sessions := []codex.Session{{ID: "fixture", RolloutPath: rolloutPath}}
	if _, err := Evaluate(context.Background(), sessions, Options{
		All: true, IndexPath: indexPath, Layers: []string{LayerField}, MinFieldBytes: 4,
	}); err != nil {
		t.Fatalf("initial persistent Evaluate returned error: %v", err)
	}
	_, err := Evaluate(context.Background(), sessions, Options{
		All: true, IndexPath: indexPath, Incremental: true, Layers: []string{LayerField}, MinFieldBytes: 8,
	})
	if !errors.Is(err, ErrIndexRebuildRequired) {
		t.Fatalf("Evaluate error = %v, want ErrIndexRebuildRequired", err)
	}
}

func TestIncrementalEvaluateRebuildsCDCForAppendAndMatchesFullScan(t *testing.T) {
	root := t.TempDir()
	rolloutPath := filepath.Join(root, "rollout.jsonl")
	indexPath := filepath.Join(root, "incremental.sqlite")
	first := []byte("{\"value\":\"abcdefghijklmno\"}\n")
	appended := []byte("{\"value\":\"pqrstuvwxyz0123456789\"}\n")
	if err := os.WriteFile(rolloutPath, first, 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	sessions := []codex.Session{{ID: "fixture", RolloutPath: rolloutPath}}
	options := Options{
		All: true, IndexPath: indexPath, Incremental: true, Layers: []string{LayerCDC},
		CDC: DedupCDCOptions{MinBytes: 8, AverageBytes: 16, MaxBytes: 32},
	}
	if _, err := Evaluate(context.Background(), sessions, options); err != nil {
		t.Fatalf("initial Evaluate returned error: %v", err)
	}
	file, err := os.OpenFile(rolloutPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open rollout for append: %v", err)
	}
	if _, err := file.Write(appended); err != nil {
		_ = file.Close()
		t.Fatalf("append rollout: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close rollout: %v", err)
	}
	incremental, err := Evaluate(context.Background(), sessions, options)
	if err != nil {
		t.Fatalf("incremental Evaluate returned error: %v", err)
	}
	full, err := Evaluate(context.Background(), sessions, Options{All: true, Layers: []string{LayerCDC}, CDC: options.CDC})
	if err != nil {
		t.Fatalf("full Evaluate returned error: %v", err)
	}
	if got, want := findLayerStats(incremental.Layers, LayerCDC), findLayerStats(full.Layers, LayerCDC); got != want {
		t.Fatalf("incremental CDC differs from full scan:\ngot=%#v\nwant=%#v", got, want)
	}
}

func TestEvaluateRequiresExplicitSelector(t *testing.T) {
	_, err := Evaluate(context.Background(), nil, Options{})
	if !errors.Is(err, ErrSelectorRequired) {
		t.Fatalf("Evaluate error = %v, want ErrSelectorRequired", err)
	}
}

func TestEvaluateRejectsUnknownExplicitSession(t *testing.T) {
	_, err := Evaluate(context.Background(), []codex.Session{{ID: "known"}}, Options{
		SessionIDs: []string{"missing"},
		Layers:     []string{LayerField},
	})
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Evaluate error = %v, want ErrSessionNotFound", err)
	}
}

func TestSelectCandidatesTreatsMaxBytesAsHardLimit(t *testing.T) {
	root := t.TempDir()
	large := filepath.Join(root, "large.jsonl")
	small := filepath.Join(root, "small.jsonl")
	if err := os.WriteFile(large, make([]byte, 20), 0o644); err != nil {
		t.Fatalf("write large fixture: %v", err)
	}
	if err := os.WriteFile(small, make([]byte, 8), 0o644); err != nil {
		t.Fatalf("write small fixture: %v", err)
	}
	candidates, _, err := selectCandidates([]codex.Session{
		{ID: "large", RolloutPath: large},
		{ID: "small", RolloutPath: small},
	}, Options{All: true, MaxBytes: 10})
	if err != nil {
		t.Fatalf("selectCandidates returned error: %v", err)
	}
	if len(candidates) != 1 || candidates[0].session.ID != "small" {
		t.Fatalf("selected candidates = %#v, want only small fixture", candidates)
	}
}

func TestEvaluateFindsCrossSessionRawFieldDuplicates(t *testing.T) {
	root := t.TempDir()
	firstPath := filepath.Join(root, "first.jsonl")
	secondPath := filepath.Join(root, "second.jsonl")
	first := []byte("{\"payload\":{\"output\":\"shared-value-across-sessions\"}}\n{\"id\":1}\n")
	second := []byte("{\"id\":2}\n{\"payload\":{\"output\":\"shared-value-across-sessions\"}}\n")
	if err := os.WriteFile(firstPath, first, 0o644); err != nil {
		t.Fatalf("write first rollout: %v", err)
	}
	if err := os.WriteFile(secondPath, second, 0o644); err != nil {
		t.Fatalf("write second rollout: %v", err)
	}

	result, err := Evaluate(context.Background(), []codex.Session{
		{ID: "first", RolloutPath: firstPath},
		{ID: "second", RolloutPath: secondPath},
	}, Options{
		All:              true,
		IndexPath:        filepath.Join(root, "dedup.sqlite"),
		Layers:           []string{LayerRecord, LayerField},
		MinFieldBytes:    8,
		MaxJSONLineBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if result.SessionCount != 2 {
		t.Fatalf("session count = %d, want 2", result.SessionCount)
	}
	field := findLayerStats(result.Layers, LayerField)
	if field.DuplicateOccurrences != 1 || field.DuplicateBytes != int64(len(`"shared-value-across-sessions"`)) {
		t.Fatalf("field duplicate stats = %#v", field)
	}
	assertFileBytes(t, firstPath, first)
	assertFileBytes(t, secondPath, second)
}

func findLayerStats(layers []LayerStats, layer string) LayerStats {
	for _, stats := range layers {
		if stats.Layer == layer {
			return stats
		}
	}
	return LayerStats{}
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("source %s changed during read-only evaluation", path)
	}
}

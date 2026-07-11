package scan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jstar0/codexfold/internal/codex"
)

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

package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samekind/codexfold/internal/storage"
)

type reconcileRejectingChecker struct {
	Calls      int
	Projection storage.Projection
}

func (c *reconcileRejectingChecker) Check(_ context.Context, projection storage.Projection) (storage.Assessment, error) {
	c.Calls++
	c.Projection = projection
	return storage.Assessment{}, storage.ErrBudgetExceeded
}

func TestMergeInsertsBranchOnlyRecordsByTimestamp(t *testing.T) {
	dir := t.TempDir()
	base := writeRollout(t, dir, "base.jsonl", []string{
		record("2026-07-13T01:00:00Z", "a"),
		record("2026-07-13T01:02:00Z", "c"),
	})
	branch := writeRollout(t, dir, "branch.jsonl", []string{
		record("2026-07-13T01:00:00Z", "a"),
		record("2026-07-13T01:01:00Z", "b"),
		record("2026-07-13T01:02:00Z", "c"),
	})
	output := filepath.Join(dir, "merged.jsonl")

	result, err := Merge(base, branch, output)
	if err != nil {
		t.Fatal(err)
	}
	if result.SharedRecords != 2 || result.AddedFromBranch != 1 || result.OutputRecords != 3 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.Storage == nil || result.Storage.Budget.ProjectedPeakBytes <= 0 || result.Storage.ActualReclaimedBytes != 0 {
		t.Fatalf("merge storage accounting is incomplete: %#v", result.Storage)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		record("2026-07-13T01:00:00Z", "a"),
		record("2026-07-13T01:01:00Z", "b"),
		record("2026-07-13T01:02:00Z", "c"),
	}, "\n") + "\n"
	if string(data) != want {
		t.Fatalf("merged bytes:\n%s\nwant:\n%s", data, want)
	}
}

func TestMergeBudgetRejectsBeforeCreatingOutput(t *testing.T) {
	dir := t.TempDir()
	base := writeRollout(t, dir, "base.jsonl", []string{record("2026-07-13T01:00:00Z", "a")})
	branch := writeRollout(t, dir, "branch.jsonl", []string{record("2026-07-13T01:01:00Z", "b")})
	output := filepath.Join(dir, "output", "merged.jsonl")
	checker := &reconcileRejectingChecker{}
	if _, err := MergeWithOptions(base, branch, output, MergeOptions{Budget: checker}); !errors.Is(err, storage.ErrBudgetExceeded) {
		t.Fatalf("MergeWithOptions error = %v, want storage budget rejection", err)
	}
	if checker.Calls != 1 || checker.Projection.Operation != "reconcile-rollout" || checker.Projection.AdditionalPersistentBytes <= 0 {
		t.Fatalf("unexpected merge budget projection: %#v", checker)
	}
	if _, err := os.Stat(filepath.Dir(output)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("merge output directory exists after preflight rejection: %v", err)
	}
}

func TestMergePreservesExcessDuplicateOccurrence(t *testing.T) {
	dir := t.TempDir()
	line := record("2026-07-13T01:00:00Z", "same")
	base := writeRollout(t, dir, "base.jsonl", []string{line})
	branch := writeRollout(t, dir, "branch.jsonl", []string{line, line})
	output := filepath.Join(dir, "merged.jsonl")

	result, err := Merge(base, branch, output)
	if err != nil {
		t.Fatal(err)
	}
	if result.SharedRecords != 1 || result.AddedFromBranch != 1 || result.OutputRecords != 2 {
		t.Fatalf("unexpected result: %#v", result)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != line+"\n"+line+"\n" {
		t.Fatalf("duplicate occurrence was not preserved: %q", data)
	}
}

func TestMergeSortsTimestampRegression(t *testing.T) {
	dir := t.TempDir()
	base := writeRollout(t, dir, "base.jsonl", []string{
		record("2026-07-13T01:01:00Z", "later"),
		record("2026-07-13T01:00:00Z", "earlier"),
	})
	branch := writeRollout(t, dir, "branch.jsonl", []string{
		record("2026-07-13T01:00:30Z", "branch"),
	})

	report, err := Analyze(base, branch)
	if err != nil {
		t.Fatal(err)
	}
	if report.Base.TimestampRegressions != 1 {
		t.Fatalf("regressions = %d, want 1", report.Base.TimestampRegressions)
	}
	output := filepath.Join(dir, "merged.jsonl")
	if _, err := Merge(base, branch, output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"value":"earlier"`) || !strings.HasPrefix(string(data), record("2026-07-13T01:00:00Z", "earlier")) {
		t.Fatalf("merge did not sort records: %q", data)
	}
}

func TestAnalyzeRejectsMissingTimestamp(t *testing.T) {
	dir := t.TempDir()
	base := writeRollout(t, dir, "base.jsonl", []string{`{"type":"event_msg"}`})
	branch := writeRollout(t, dir, "branch.jsonl", []string{record("2026-07-13T01:00:00Z", "ok")})

	if _, err := Analyze(base, branch); err == nil {
		t.Fatal("analyze accepted a record without a timestamp")
	}
}

func TestAnalyzeFindsTopLevelTimestampAfterLargePayload(t *testing.T) {
	dir := t.TempDir()
	line := `{"payload":{"text":"` + strings.Repeat("x", 16*1024) + `","timestamp":"2000-01-01T00:00:00Z"},"timestamp":"2026-07-13T01:00:00Z","type":"session_meta"}`
	base := writeRollout(t, dir, "base.jsonl", []string{line})
	branch := writeRollout(t, dir, "branch.jsonl", []string{line})

	report, err := Analyze(base, branch)
	if err != nil {
		t.Fatal(err)
	}
	if report.Base.FirstTimestamp != "2026-07-13T01:00:00Z" {
		t.Fatalf("timestamp = %s", report.Base.FirstTimestamp)
	}
}

func record(timestamp, value string) string {
	return `{"timestamp":"` + timestamp + `","type":"event_msg","payload":{"value":"` + value + `"}}`
}

func writeRollout(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

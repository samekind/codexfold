package reconcile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jstar0/codexfold/internal/storage"
)

func TestRepairRestoresInterruptedRecordBeforeInsertedRecord(t *testing.T) {
	dir := t.TempDir()
	outer := recordWithText("2026-07-13T01:00:00Z", "abcdef")
	inner := recordWithText("2026-07-13T01:00:01Z", "inner")
	prefix, suffix := splitAt(t, outer, "abc")
	input := filepath.Join(dir, "broken.jsonl")
	if err := os.WriteFile(input, []byte(prefix+inner+"\n"+suffix+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "repaired.jsonl")

	result, err := Repair(input, output)
	if err != nil {
		t.Fatal(err)
	}
	if result.InvalidPhysicalLines != 2 || result.ReconstructedRecords != 2 || result.OutputRecords != 2 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.Storage == nil || result.Storage.Budget.ProjectedPeakBytes <= 0 || result.Storage.ActualReclaimedBytes != 0 {
		t.Fatalf("repair storage accounting is incomplete: %#v", result.Storage)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != outer+"\n"+inner+"\n" {
		t.Fatalf("repaired bytes:\n%s", data)
	}
}

func TestRepairBudgetRejectsBeforeCreatingOutput(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "broken.jsonl")
	if err := os.WriteFile(input, []byte(recordWithText("2026-07-13T01:00:00Z", "value")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "output", "repaired.jsonl")
	checker := &reconcileRejectingChecker{}
	if _, err := RepairWithOptions(input, output, RepairOptions{Budget: checker}); !errors.Is(err, storage.ErrBudgetExceeded) {
		t.Fatalf("RepairWithOptions error = %v, want storage budget rejection", err)
	}
	if checker.Calls != 1 || checker.Projection.Operation != "repair-rollout" {
		t.Fatalf("unexpected repair budget projection: %#v", checker)
	}
	if _, err := os.Stat(filepath.Dir(output)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repair output directory exists after preflight rejection: %v", err)
	}
}

func TestRepairBuffersValidPhysicalRecordsWhileOuterRecordIsOpen(t *testing.T) {
	dir := t.TempDir()
	outer := recordWithText("2026-07-13T01:00:00Z", "abcdef")
	inner := recordWithText("2026-07-13T01:00:01Z", "inner")
	prefix, suffix := splitAt(t, outer, "abc")
	input := filepath.Join(dir, "broken.jsonl")
	if err := os.WriteFile(input, []byte(prefix+"\n"+inner+"\n"+suffix+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "repaired.jsonl")

	if _, err := Repair(input, output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != outer+"\n"+inner+"\n" {
		t.Fatalf("repaired bytes:\n%s", data)
	}
}

func TestRepairRestoresNestedInterruptions(t *testing.T) {
	dir := t.TempDir()
	outer := recordWithText("2026-07-13T01:00:00Z", "abcdef")
	middle := recordWithText("2026-07-13T01:00:01Z", "ghijkl")
	inner := recordWithText("2026-07-13T01:00:02Z", "inner")
	outerPrefix, outerSuffix := splitAt(t, outer, "abc")
	middlePrefix, middleSuffix := splitAt(t, middle, "ghi")
	input := filepath.Join(dir, "broken.jsonl")
	physical := outerPrefix + middlePrefix + inner + "\n" + middleSuffix + "\n" + outerSuffix + "\n"
	if err := os.WriteFile(input, []byte(physical), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "repaired.jsonl")

	if _, err := Repair(input, output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != outer+"\n"+middle+"\n"+inner+"\n" {
		t.Fatalf("repaired bytes:\n%s", data)
	}
}

func TestRepairSalvageKeepsValidRecordsAfterUnfinishedFragment(t *testing.T) {
	dir := t.TempDir()
	before := recordWithText("2026-07-13T01:00:00Z", "before")
	unfinished := `{"timestamp":"2026-07-13T01:00:01Z","type":"event_msg","payload":{"text":"unfinished`
	afterOne := recordWithText("2026-07-13T01:00:02Z", "after-one")
	afterTwo := recordWithText("2026-07-13T01:00:03Z", "after-two")
	input := filepath.Join(dir, "broken.jsonl")
	physical := before + "\n" + unfinished + "\n" + afterOne + "\n" + afterTwo + "\n"
	if err := os.WriteFile(input, []byte(physical), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "repaired.jsonl")
	orphans := filepath.Join(dir, "orphans.bin")

	result, err := RepairWithOptions(input, output, RepairOptions{AllowOrphans: true, OrphanPath: orphans})
	if err != nil {
		t.Fatal(err)
	}
	if result.OutputRecords != 3 || result.OrphanLines != 1 || result.OrphanBytes != int64(len(unfinished)) {
		t.Fatalf("unexpected salvage result: %#v", result)
	}
	repaired, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(repaired) != before+"\n"+afterOne+"\n"+afterTwo+"\n" {
		t.Fatalf("salvaged bytes:\n%s", repaired)
	}
	orphaned, err := os.ReadFile(orphans)
	if err != nil {
		t.Fatal(err)
	}
	if string(orphaned) != unfinished+"\n" {
		t.Fatalf("orphan bytes: %q", orphaned)
	}
}

func TestRepairSalvageKeepsValidRecordsAcrossNestedUnfinishedFragments(t *testing.T) {
	dir := t.TempDir()
	outer := `{"timestamp":"2026-07-13T01:00:00Z","type":"event_msg","payload":{"text":"outer`
	between := recordWithText("2026-07-13T01:00:01Z", "between")
	inner := `{"timestamp":"2026-07-13T01:00:02Z","type":"event_msg","payload":{"text":"inner`
	after := recordWithText("2026-07-13T01:00:03Z", "after")
	input := filepath.Join(dir, "broken.jsonl")
	physical := outer + "\n" + between + "\n" + inner + "\n" + after + "\n"
	if err := os.WriteFile(input, []byte(physical), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "repaired.jsonl")
	orphans := filepath.Join(dir, "orphans.bin")

	result, err := RepairWithOptions(input, output, RepairOptions{AllowOrphans: true, OrphanPath: orphans})
	if err != nil {
		t.Fatal(err)
	}
	if result.OutputRecords != 2 || result.OrphanLines != 2 {
		t.Fatalf("unexpected nested salvage result: %#v", result)
	}
	repaired, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(repaired) != between+"\n"+after+"\n" {
		t.Fatalf("nested salvaged bytes:\n%s", repaired)
	}
	orphaned, err := os.ReadFile(orphans)
	if err != nil {
		t.Fatal(err)
	}
	if string(orphaned) != outer+"\n"+inner+"\n" {
		t.Fatalf("nested orphan bytes: %q", orphaned)
	}
}

func recordWithText(timestamp, text string) string {
	return `{"timestamp":"` + timestamp + `","type":"event_msg","payload":{"text":"` + text + `"}}`
}

func splitAt(t *testing.T, value, marker string) (string, string) {
	t.Helper()
	index := strings.Index(value, marker)
	if index < 0 {
		t.Fatalf("marker %q not found", marker)
	}
	index += len(marker)
	return value[:index], value[index:]
}

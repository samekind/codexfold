package reconcile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != outer+"\n"+inner+"\n" {
		t.Fatalf("repaired bytes:\n%s", data)
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

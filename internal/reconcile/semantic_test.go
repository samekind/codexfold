package reconcile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepairVerifiesConversationRecordsAcrossSalvage(t *testing.T) {
	dir := t.TempDir()
	user := conversationRecord("2026-07-16T00:00:00Z", "response_item", "message", "user")
	unfinished := `{"timestamp":"2026-07-16T00:00:01Z","type":"event_msg","payload":{"text":"unfinished`
	agent := conversationRecord("2026-07-16T00:00:02Z", "event_msg", "agent_message", "")
	source := filepath.Join(dir, "source.jsonl")
	if err := os.WriteFile(source, []byte(user+"\n"+unfinished+"\n"+agent+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := RepairWithOptions(source, filepath.Join(dir, "repaired.jsonl"), RepairOptions{
		AllowOrphans: true,
		OrphanPath:   filepath.Join(dir, "orphans.bin"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ConversationIntegrityVerified || result.SourceConversationRecords != 2 || result.PreservedConversationRecords != 2 || result.ReconstructedConversationRecords != 0 {
		t.Fatalf("unexpected conversation verification: %#v", result)
	}
}

func TestRepairCountsReconstructedConversationRecord(t *testing.T) {
	dir := t.TempDir()
	record := conversationRecord("2026-07-16T00:00:00Z", "response_item", "message", "assistant")
	prefix, suffix := splitAt(t, record, `"role"`)
	source := filepath.Join(dir, "source.jsonl")
	if err := os.WriteFile(source, []byte(prefix+"\n"+suffix+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Repair(source, filepath.Join(dir, "repaired.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !result.ConversationIntegrityVerified || result.SourceConversationRecords != 0 || result.PreservedConversationRecords != 0 || result.ReconstructedConversationRecords != 1 {
		t.Fatalf("unexpected reconstructed conversation verification: %#v", result)
	}
}

func TestRepairRefusesCompleteConversationRecordInOrphan(t *testing.T) {
	dir := t.TempDir()
	record := conversationRecord("2026-07-16T00:00:00Z", "event_msg", "user_message", "")
	source := filepath.Join(dir, "source.jsonl")
	if err := os.WriteFile(source, []byte("garbage"+record+"trailing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := RepairWithOptions(source, filepath.Join(dir, "repaired.jsonl"), RepairOptions{
		AllowOrphans: true,
		OrphanPath:   filepath.Join(dir, "orphans.bin"),
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to orphan") {
		t.Fatalf("RepairWithOptions error = %v, want complete conversation refusal", err)
	}
}

func conversationRecord(timestamp, entryType, payloadType, role string) string {
	payload := `{"type":"` + payloadType + `"`
	if role != "" {
		payload += `,"role":"` + role + `"`
	}
	payload += `}`
	return `{"timestamp":"` + timestamp + `","type":"` + entryType + `","payload":` + payload + `}`
}

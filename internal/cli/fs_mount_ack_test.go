package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteMountAcknowledgementSkipsMatchingDurableRecord(t *testing.T) {
	store := t.TempDir()
	directory := filepath.Join(store, "fs", "sessions", "session")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "mounted.json")
	if err := writeMountAcknowledgement(store, "session", 7, "/archived_sessions/session.jsonl"); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1234, 0)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := writeMountAcknowledgement(store, "session", 7, "/archived_sessions/session.jsonl"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Fatalf("matching acknowledgement was rewritten: mtime=%s", info.ModTime())
	}
	if err := writeMountAcknowledgement(store, "session", 8, "/archived_sessions/session.jsonl"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var updated mountAcknowledgement
	if err := json.Unmarshal(data, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Generation != 8 {
		t.Fatalf("stale acknowledgement after generation change: %#v", updated)
	}
	if err := os.WriteFile(path, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeMountAcknowledgement(store, "session", 8, "/archived_sessions/session.jsonl"); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &updated); err != nil || updated.Generation != 8 {
		t.Fatalf("corrupt acknowledgement was not repaired: %#v err=%v", updated, err)
	}
}

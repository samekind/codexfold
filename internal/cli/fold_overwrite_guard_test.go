package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefuseManagedFoldOverwrite(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	sessionID := "6a6efb33-8898-469a-bc88-45c312cb2c6f"
	if err := refuseManagedFoldOverwrite(store, sessionID, true, true); err != nil {
		t.Fatalf("absent state: %v", err)
	}
	if err := refuseManagedFoldOverwrite(store, sessionID, false, true); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	directory := filepath.Join(store, "fs", "sessions", sessionID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "state.json"), []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := refuseManagedFoldOverwrite(store, sessionID, true, true)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite fold manifest") {
		t.Fatalf("managed overwrite error = %v", err)
	}
}

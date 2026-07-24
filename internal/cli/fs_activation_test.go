package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitApplyMayActivateTheRealCodexHomeDuringPreview(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("CODEX_HOME", "")

	codexHome := filepath.Join(userHome, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := requireFilesystemActivationAllowed(codexHome); err != nil {
		t.Fatalf("explicit real-home activation was rejected: %v", err)
	}
}

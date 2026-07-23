package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreviewRejectsEveryRealCodexHomeActivationEntryPoint(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("CODEX_HOME", "")

	codexHome := filepath.Join(userHome, ".codex")
	store := filepath.Join(codexHome, "fold-store")
	mount := filepath.Join(codexHome, "fold-fs")
	native := filepath.Join(codexHome, "fold-native")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	definition := filepath.Join(userHome, "com.codexfold.fs.plist")
	if err := os.WriteFile(definition, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	previousProbe := mountHealthProbe
	mountHealthProbe = func(string) error { return nil }
	t.Cleanup(func() { mountHealthProbe = previousProbe })

	want := "real Codex home activation is disabled while filesystem capability is fs-engine-preview"
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "serve",
			args: []string{"fs", "serve", "--codex-home", codexHome, "--store", store, "--mount", mount, "--apply"},
		},
		{
			name: "service install",
			args: []string{
				"fs", "service", "install", "--codex-home", codexHome, "--store", store, "--mount", mount,
				"--native-root", native, "--canonical-namespace", "--plist", definition, "--apply",
			},
		},
		{
			name: "service start",
			args: []string{"fs", "service", "start", "--codex-home", codexHome, "--mount", mount, "--plist", definition, "--apply"},
		},
		{
			name: "namespace activate",
			args: []string{
				"fs", "namespace", "activate", "--codex-home", codexHome, "--mount", mount,
				"--native-root", native, "--apply",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := NewRootCommand()
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(test.args)
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("activation error = %v, want %q", err, want)
			}
		})
	}
}

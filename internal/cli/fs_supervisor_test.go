package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestFSNativeSupervisorDryRunReportsAbsoluteRuntimePaths(t *testing.T) {
	root := t.TempDir()
	command := NewRootCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{
		"fs", "supervise",
		"--resource", filepath.Join(root, "resource.bin"),
		"--mount", filepath.Join(root, "mount"),
		"--json",
	})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var result FSNativeSupervisorResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode supervisor dry-run: %v\n%s", err, output.String())
	}
	if !result.DryRun || result.ResourcePath == "" || result.MountPoint == "" {
		t.Fatalf("unexpected supervisor dry-run: %#v", result)
	}
}

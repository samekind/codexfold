//go:build windows

package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/samekind/codexfold/internal/mountid"
)

func TestWindowsMountProbeRejectsOrdinaryDirectory(t *testing.T) {
	path := t.TempDir()
	if present, err := MountPresent(path); err != nil || present {
		t.Fatalf("ordinary directory: present=%v err=%v", present, err)
	}
	if present, err := MountPresent(filepath.Join(path, "missing")); err != nil || present {
		t.Fatalf("missing directory: present=%v err=%v", present, err)
	}
	if err := os.WriteFile(filepath.Join(path, mountid.Path), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := defaultMountProbe(path); err == nil {
		t.Fatal("marker in an ordinary directory accepted as a mounted filesystem")
	}
}

func TestWindowsMountRootRecognizesLocalUNCAndRejectsSubdirectories(t *testing.T) {
	for _, path := range []string{`\Device\Volume{test}\`, `\Device\Mup\codexfold\test\`} {
		if !windowsMountRoot(path) {
			t.Fatalf("mount root rejected: %s", path)
		}
	}
	for _, path := range []string{`\Device\HarddiskVolume3\Users\guo`, `\Device\Mup\codexfold\test\sessions`, `\Device\Mup\other\test\`} {
		if windowsMountRoot(path) {
			t.Fatalf("ordinary path accepted as mount root: %s", path)
		}
	}
}

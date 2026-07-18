//go:build darwin && fuse && cgo

package mountfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateDarwinFUSEProviderRequiresFUSEt(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "libfuse-t-1.2.7.dylib")
	if err := os.WriteFile(target, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "libfuse-t.dylib")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := validateDarwinFUSEProviderPaths(link, nil); err != nil {
		t.Fatalf("valid FUSE-T layout rejected: %v", err)
	}

	competitor := filepath.Join(root, "libfuse.2.dylib")
	if err := os.WriteFile(competitor, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateDarwinFUSEProviderPaths(link, []string{competitor}); err == nil || !strings.Contains(err.Error(), "take precedence") {
		t.Fatalf("competing FUSE library was not rejected: %v", err)
	}
}

func TestValidateDarwinFUSEProviderRejectsMissingFUSEt(t *testing.T) {
	err := validateDarwinFUSEProviderPaths(filepath.Join(t.TempDir(), "libfuse-t.dylib"), nil)
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing FUSE-T library was not rejected: %v", err)
	}
}

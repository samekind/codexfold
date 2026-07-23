//go:build darwin && fuse && cgo

package mountfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const darwinFUSEtLibraryPath = "/usr/local/lib/libfuse-t.dylib"

var darwinHigherPriorityFUSELibraries = []string{
	"/usr/local/lib/libfuse.2.dylib",
	"/usr/local/lib/libosxfuse.2.dylib",
}

func validateFuseProvider() error {
	return validateDarwinFUSEProviderPaths(darwinFUSEtLibraryPath, darwinHigherPriorityFUSELibraries)
}

func validateDarwinFUSEProviderPaths(fuseTPath string, higherPriority []string) error {
	for _, path := range higherPriority {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("unsupported macOS FUSE library %q would take precedence over FUSE-T", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect competing macOS FUSE library %q: %w", path, err)
		}
	}

	resolved, err := filepath.EvalSymlinks(fuseTPath)
	if err != nil {
		return fmt.Errorf("FUSE-T library %q is unavailable: %w", fuseTPath, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("inspect FUSE-T library %q: %w", resolved, err)
	}
	if !info.Mode().IsRegular() || !strings.Contains(filepath.Base(resolved), "libfuse-t") {
		return fmt.Errorf("FUSE-T library resolves to an unexpected file %q", resolved)
	}
	return nil
}

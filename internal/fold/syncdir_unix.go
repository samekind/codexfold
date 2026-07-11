//go:build !windows

package fold

import (
	"fmt"
	"os"
)

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory for sync %s: %w", path, err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync directory %s: %w", path, err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close synced directory %s: %w", path, err)
	}
	return nil
}

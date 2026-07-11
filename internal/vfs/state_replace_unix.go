//go:build !windows

package vfs

import (
	"fmt"
	"os"
)

func replaceStateFile(source string, target string) error { return os.Rename(source, target) }

func syncStateDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync directory %s: %w", path, err)
	}
	return directory.Close()
}

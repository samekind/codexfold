package storage

import (
	"errors"
	"os"
	"path/filepath"
)

func existingSpaceProbePath(path string) (string, error) {
	path = filepath.Clean(path)
	for {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", os.ErrNotExist
		}
		path = parent
	}
}

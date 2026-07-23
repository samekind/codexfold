//go:build !darwin && !linux && !windows

package storage

import (
	"os"
	"path/filepath"
)

func physicalFile(path string, info os.FileInfo) (string, int64, error) {
	return filepath.Clean(path), info.Size(), nil
}

func physicalLinkCount(_ string, _ os.FileInfo) (uint64, error) {
	return 1, nil
}

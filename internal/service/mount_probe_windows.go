//go:build windows

package service

import (
	"os"
	"path/filepath"

	"github.com/jstar0/codexfold/internal/mountid"
)

func defaultMountProbe(path string) error {
	value, err := os.ReadFile(filepath.Join(path, mountid.Path))
	if err != nil {
		return err
	}
	return mountid.Validate(value)
}

func MountPresent(string) (bool, error) { return false, nil }

//go:build !darwin

package service

import (
	"errors"
	"os"
)

func defaultMountProbe(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("mount path is not a directory")
	}
	return nil
}

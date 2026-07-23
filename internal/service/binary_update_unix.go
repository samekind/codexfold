//go:build !windows

package service

import (
	"errors"
	"os"
)

func replaceServiceBinary(source string, target string) error {
	return os.Rename(source, target)
}

func syncServiceDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

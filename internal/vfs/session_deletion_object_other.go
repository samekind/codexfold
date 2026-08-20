//go:build !darwin && !linux

package vfs

import (
	"errors"
	"os"
)

func sessionDeletionObjectFromFile(*os.File, os.FileInfo) (SessionDeletionPurgeObject, error) {
	return SessionDeletionPurgeObject{}, errors.New("stable session deletion object identity is unsupported on this platform")
}

func captureSessionDeletionXattrs(*os.File, os.FileInfo, string) (SessionDeletionPurgeXattrs, error) {
	return SessionDeletionPurgeXattrs{}, errors.New("exact session deletion metadata validation is unsupported on this platform")
}

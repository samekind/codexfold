//go:build !darwin && !linux

package vfs

import (
	"errors"
)

func sessionDeletionRenameNoReplace(store *sessionDeletionStoreRoot, source string, target string) error {
	_ = store
	_ = source
	_ = target
	return errors.New("atomic no-replace session deletion rename is unsupported on this platform")
}

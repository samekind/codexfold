//go:build darwin

package vfs

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

func sessionDeletionRenameNoReplace(store *sessionDeletionStoreRoot, source string, target string) error {
	sourceRelative, err := store.relative(source)
	if err != nil {
		return err
	}
	targetRelative, err := store.relative(target)
	if err != nil {
		return err
	}
	sourceParent, err := store.root.Open(filepath.Dir(sourceRelative))
	if err != nil {
		return err
	}
	defer sourceParent.Close()
	targetParent, err := store.root.Open(filepath.Dir(targetRelative))
	if err != nil {
		return err
	}
	defer targetParent.Close()
	return unix.RenameatxNp(int(sourceParent.Fd()), filepath.Base(sourceRelative), int(targetParent.Fd()), filepath.Base(targetRelative), unix.RENAME_EXCL)
}

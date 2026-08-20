//go:build darwin

package prune

import "golang.org/x/sys/unix"

func renameRemovalNoReplace(source string, target string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, source, unix.AT_FDCWD, target, unix.RENAME_EXCL)
}

//go:build linux

package prune

import "golang.org/x/sys/unix"

func renameRemovalNoReplace(source string, target string) error {
	return unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE)
}

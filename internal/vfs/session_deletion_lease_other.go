//go:build !windows

package vfs

import "os"

func openDeletionWriterLease(store *sessionDeletionStoreRoot, relative, _ string) (*os.File, error) {
	return store.root.OpenFile(relative, os.O_RDWR, 0)
}

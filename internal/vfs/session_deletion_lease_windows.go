//go:build windows

package vfs

import "os"

func openDeletionWriterLease(_ *sessionDeletionStoreRoot, _ string, path string) (*os.File, error) {
	// Pin the same verified file through its store-side hard-link alias;
	// a locked handle opened inside the state tree blocks NTFS directory moves.
	return openWriterLeaseFile(path, false)
}

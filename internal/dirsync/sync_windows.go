//go:build windows

package dirsync

import (
	"fmt"
	"os"
)

// Sync validates the directory handle on Windows. Windows does not expose the
// POSIX fsync-directory operation on a read-only directory handle: File.Sync
// calls FlushFileBuffers and fails with ERROR_ACCESS_DENIED. Data files must
// still be flushed before publication. Existing MoveFileEx write-through paths
// remain in use, but this is NOT a power-loss durability guarantee for directory
// metadata. Windows remains a preview until its crash/power-loss gates pass.
func Sync(directory *os.File) error {
	info, err := directory.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("directory sync requires a directory: %s", directory.Name())
	}
	return nil
}

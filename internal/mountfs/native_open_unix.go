//go:build !windows

package mountfs

import "os"

func openNativeBacking(path string, flags int, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flags, mode)
}

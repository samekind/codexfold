//go:build darwin

package mountfs

import "golang.org/x/sys/unix"

func cloneNativeFile(source string, destination string) error {
	return unix.Clonefile(source, destination, 0)
}

//go:build darwin

package fsctl

import (
	"os"

	"golang.org/x/sys/unix"
)

func configureNoCache(file *os.File) (bool, error) {
	_, err := unix.FcntlInt(file.Fd(), unix.F_NOCACHE, 1)
	return err == nil, err
}

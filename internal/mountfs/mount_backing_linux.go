//go:build linux && fuse && fuse3 && cgo

package mountfs

import (
	"errors"
	"os"
	"sync"
)

type linuxMountBacking struct {
	mu        sync.Mutex
	directory *os.File
	sealed    bool
	closed    bool
}

func prepareMountedBacking(path string) (*linuxMountBacking, error) {
	directory, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if err := directory.Chmod(0o700); err != nil {
		_ = directory.Close()
		return nil, err
	}
	return &linuxMountBacking{directory: directory}, nil
}

func (backing *linuxMountBacking) Seal() error {
	backing.mu.Lock()
	defer backing.mu.Unlock()
	if backing.closed {
		return errors.New("mount backing guard is already closed")
	}
	if backing.sealed {
		return nil
	}
	if err := backing.directory.Chmod(0o500); err != nil {
		return err
	}
	backing.sealed = true
	return nil
}

func (backing *linuxMountBacking) Close() error {
	backing.mu.Lock()
	defer backing.mu.Unlock()
	if backing.closed {
		return nil
	}
	var sealErr error
	if !backing.sealed {
		sealErr = backing.directory.Chmod(0o500)
		backing.sealed = sealErr == nil
	}
	closeErr := backing.directory.Close()
	backing.closed = true
	return errors.Join(sealErr, closeErr)
}

//go:build darwin || linux

package nativezst

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// ReadMarker reports the marker on path, without following symlinks.
func ReadMarker(path string) (Marker, bool, error) {
	buffer := make([]byte, maxMarkerBytes)
	n, err := unix.Lgetxattr(path, MarkerAttribute, buffer)
	if err != nil {
		if missingAttribute(err) {
			return Marker{}, false, nil
		}
		return Marker{}, false, &os.PathError{Op: "getxattr", Path: path, Err: err}
	}
	marker, err := decodeMarker(buffer[:n])
	return marker, true, err
}

func writeMarker(file *os.File, marker Marker) error {
	data, err := marker.encode()
	if err != nil {
		return err
	}
	if err := unix.Fsetxattr(int(file.Fd()), MarkerAttribute, data, 0); err != nil {
		return &os.PathError{Op: "setxattr", Path: file.Name(), Err: err}
	}
	return nil
}

func removeMarker(file *os.File) error {
	if err := unix.Fremovexattr(int(file.Fd()), MarkerAttribute); err != nil && !missingAttribute(err) {
		return &os.PathError{Op: "removexattr", Path: file.Name(), Err: err}
	}
	return nil
}

func missingAttribute(err error) bool {
	return errors.Is(err, noAttribute) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}

// allocatedBytes is the space a file's data blocks occupy.
func allocatedBytes(info os.FileInfo) int64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(stat.Blocks) * 512
	}
	return info.Size()
}

func inode(info os.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Ino)
	}
	return 0
}

func supported() bool { return true }

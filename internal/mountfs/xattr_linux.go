//go:build linux

package mountfs

import (
	"bytes"
	"syscall"

	"github.com/jstar0/codexfold/internal/fskitproto"
	"golang.org/x/sys/unix"
)

func platformSetXattr(path string, attribute string, value []byte, policy fskitproto.XattrPolicy) error {
	flags := 0
	switch policy {
	case fskitproto.XattrAlwaysSet:
	case fskitproto.XattrMustCreate:
		flags = unix.XATTR_CREATE
	case fskitproto.XattrMustReplace:
		flags = unix.XATTR_REPLACE
	default:
		return syscall.EINVAL
	}
	return unix.Setxattr(path, attribute, value, flags)
}

func platformGetXattr(path string, attribute string) ([]byte, error) {
	size, err := unix.Getxattr(path, attribute, nil)
	if err != nil {
		return nil, err
	}
	value := make([]byte, size)
	n, err := unix.Getxattr(path, attribute, value)
	if err != nil {
		return nil, err
	}
	return value[:n], nil
}

func platformListXattrs(path string) ([]string, error) {
	size, err := unix.Listxattr(path, nil)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return []string{}, nil
	}
	buffer := make([]byte, size)
	n, err := unix.Listxattr(path, buffer)
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(bytes.TrimRight(buffer[:n], "\x00"), []byte{0})
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) != 0 {
			result = append(result, string(part))
		}
	}
	return result, nil
}

func platformRemoveXattr(path string, attribute string) error {
	return unix.Removexattr(path, attribute)
}

func xattrMissingErrno() syscall.Errno { return syscall.ENODATA }

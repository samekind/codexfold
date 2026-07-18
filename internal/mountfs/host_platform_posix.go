//go:build (darwin && fuse && cgo) || (linux && fuse && fuse3 && cgo)

package mountfs

import (
	"bytes"

	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/unix"
)

func setFileTimes(path string, times []fuse.Timespec) int {
	values := []unix.Timespec{{Sec: times[0].Sec, Nsec: times[0].Nsec}, {Sec: times[1].Sec, Nsec: times[1].Nsec}}
	return unixResult(unix.UtimesNanoAt(unix.AT_FDCWD, path, values, 0))
}

func setExtendedAttribute(path string, attribute string, value []byte, flags int) int {
	return unixResult(unix.Setxattr(path, attribute, value, flags))
}

func getExtendedAttribute(path string, attribute string) (int, []byte) {
	size, err := unix.Getxattr(path, attribute, nil)
	if err != nil {
		return unixResult(err), nil
	}
	value := make([]byte, size)
	n, err := unix.Getxattr(path, attribute, value)
	if err != nil {
		return unixResult(err), nil
	}
	return 0, value[:n]
}

func listExtendedAttributes(path string) (int, []string) {
	size, err := unix.Listxattr(path, nil)
	if err != nil {
		return unixResult(err), nil
	}
	buffer := make([]byte, size)
	n, err := unix.Listxattr(path, buffer)
	if err != nil {
		return unixResult(err), nil
	}
	parts := bytes.Split(bytes.TrimRight(buffer[:n], "\x00"), []byte{0})
	attributes := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) != 0 {
			attributes = append(attributes, string(part))
		}
	}
	return 0, attributes
}

func removeExtendedAttribute(path string, attribute string) int {
	return unixResult(unix.Removexattr(path, attribute))
}

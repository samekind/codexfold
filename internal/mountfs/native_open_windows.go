//go:build windows

package mountfs

import (
	"os"

	"golang.org/x/sys/windows"
)

func openNativeBacking(path string, flags int, _ os.FileMode) (*os.File, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.GENERIC_READ)
	if flags&(os.O_WRONLY|os.O_RDWR) != 0 {
		access |= windows.GENERIC_WRITE
	}
	creation := uint32(windows.OPEN_EXISTING)
	if flags&os.O_CREATE != 0 {
		creation = windows.OPEN_ALWAYS
		if flags&os.O_EXCL != 0 {
			creation = windows.CREATE_NEW
		}
	}
	attributes := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if flags&os.O_SYNC != 0 {
		attributes |= windows.FILE_FLAG_WRITE_THROUGH
	}
	// Keep native readers and append handles usable across archive moves.
	handle, err := windows.CreateFile(pointer, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, creation, attributes, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
